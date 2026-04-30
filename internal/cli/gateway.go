package cli

import (
	"errors"
	"log"
	"os"
	"strconv"

	"github.com/shiftu/shipd/internal/ai"
	"github.com/shiftu/shipd/internal/gateway"
	"github.com/shiftu/shipd/internal/mcp"
	"github.com/spf13/cobra"
)

// `shipd gateway serve` runs an adapter that bridges chat platforms (or a
// local stdin REPL) into the same shipd tools agents see over MCP.
func newGatewayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Chat-platform bridge into shipd (Feishu, stdio, ...)",
	}

	var adapter string
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Run a gateway adapter",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := clientFromFlags(cmd)
			if err != nil {
				return err
			}
			reg := mcp.NewRegistry()
			mcp.RegisterShipdTools(reg, c, c.BaseURL)
			router := gateway.NewRouter(reg)

			// Enable the chat-mode "ask" verb when an Anthropic API key is
			// available. Without one the gateway still works for the
			// structured verbs (list, info, ...) — `ask` just replies with
			// a clear "not enabled" message.
			if apiKey := os.Getenv("ANTHROPIC_API_KEY"); apiKey != "" {
				model, _ := cmd.Flags().GetString("ai-model")
				agent := ai.NewAgent(ai.NewClient(ai.Config{APIKey: apiKey, Model: model}), reg, log.Default())
				router.WithAgent(agent)
				log.Default().Printf("ask verb enabled (model=%s)", firstNonEmpty(model, "claude-sonnet-4-6"))
			}

			ctx, cancel := signalContext()
			defer cancel()

			a, err := buildAdapter(adapter, cmd)
			if err != nil {
				return err
			}
			log.Default().Printf("gateway adapter=%s", a.Name())
			return a.Run(ctx, router.Dispatch)
		},
	}
	serve.Flags().StringVar(&adapter, "adapter", "stdio", "adapter: stdio | feishu | wechat-work | weixin | slack")
	serve.Flags().String("addr", ":8081", "listen address (HTTP adapters only)")
	serve.Flags().String("public-base-url", os.Getenv("SHIPD_GATEWAY_PUBLIC_BASE_URL"), "public URL prefix for onboarding pages (default: derived from request)")
	serve.Flags().String("feishu-mode", firstNonEmpty(os.Getenv("FEISHU_MODE"), "websocket"), "Feishu transport: websocket (default, no public URL needed) | webhook")
	serve.Flags().String("feishu-app-id", os.Getenv("FEISHU_APP_ID"), "Feishu app ID (or $FEISHU_APP_ID)")
	serve.Flags().String("feishu-app-secret", os.Getenv("FEISHU_APP_SECRET"), "Feishu app secret (or $FEISHU_APP_SECRET)")
	serve.Flags().String("feishu-verification-token", os.Getenv("FEISHU_VERIFICATION_TOKEN"), "Feishu event verification token (webhook mode only; or $FEISHU_VERIFICATION_TOKEN)")
	serve.Flags().String("wxwork-corp-id", os.Getenv("WXWORK_CORP_ID"), "WeChat Work corp ID (or $WXWORK_CORP_ID)")
	serve.Flags().Int("wxwork-agent-id", envInt("WXWORK_AGENT_ID"), "WeChat Work app agent ID (or $WXWORK_AGENT_ID)")
	serve.Flags().String("wxwork-secret", os.Getenv("WXWORK_SECRET"), "WeChat Work app secret (or $WXWORK_SECRET)")
	serve.Flags().String("wxwork-token", os.Getenv("WXWORK_TOKEN"), "WeChat Work callback verification token (or $WXWORK_TOKEN)")
	serve.Flags().String("wxwork-aes-key", os.Getenv("WXWORK_ENCODING_AES_KEY"), "WeChat Work 43-char EncodingAESKey (or $WXWORK_ENCODING_AES_KEY)")
	serve.Flags().String("slack-app-token", os.Getenv("SLACK_APP_TOKEN"), "Slack App-Level token (xapp-...) for Socket Mode (or $SLACK_APP_TOKEN)")
	serve.Flags().String("slack-bot-token", os.Getenv("SLACK_BOT_TOKEN"), "Slack Bot User OAuth token (xoxb-...) for chat.postMessage (or $SLACK_BOT_TOKEN)")
	serve.Flags().String("weixin-account-id", os.Getenv("WEIXIN_ACCOUNT_ID"), "Weixin (iLink) account_id from a prior weixin-login (or $WEIXIN_ACCOUNT_ID)")
	serve.Flags().String("weixin-token", os.Getenv("WEIXIN_TOKEN"), "Weixin bot token override (default: read from --weixin-state-dir)")
	serve.Flags().String("weixin-base-url", os.Getenv("WEIXIN_BASE_URL"), "Weixin iLink base URL override")
	serve.Flags().String("weixin-state-dir", defaultWeixinStateDir(), "directory for Weixin credentials and sync_buf")
	serve.Flags().String("ai-model", "", "Claude model for the 'ask' verb (default: claude-sonnet-4-6, requires $ANTHROPIC_API_KEY)")

	cmd.AddCommand(serve)
	cmd.AddCommand(newWeixinLoginCmd())
	return cmd
}

func buildAdapter(name string, cmd *cobra.Command) (gateway.Adapter, error) {
	switch name {
	case "stdio":
		return &gateway.StdioAdapter{
			In:     os.Stdin,
			Out:    os.Stdout,
			Prompt: "shipd> ",
		}, nil
	case "feishu":
		mode, _ := cmd.Flags().GetString("feishu-mode")
		addr, _ := cmd.Flags().GetString("addr")
		appID, _ := cmd.Flags().GetString("feishu-app-id")
		appSecret, _ := cmd.Flags().GetString("feishu-app-secret")
		vtok, _ := cmd.Flags().GetString("feishu-verification-token")
		if appID == "" || appSecret == "" {
			return nil, errors.New("feishu adapter needs --feishu-app-id and --feishu-app-secret")
		}
		return gateway.NewFeishuAdapter(gateway.FeishuConfig{
			Mode:              mode,
			Addr:              addr,
			AppID:             appID,
			AppSecret:         appSecret,
			VerificationToken: vtok,
		}, log.Default()), nil
	case "weixin":
		return loadWeixinAdapter(cmd)
	case "slack":
		appToken, _ := cmd.Flags().GetString("slack-app-token")
		botToken, _ := cmd.Flags().GetString("slack-bot-token")
		return gateway.NewSlackAdapter(gateway.SlackConfig{
			AppToken: appToken,
			BotToken: botToken,
		}, log.Default())
	case "wechat-work":
		addr, _ := cmd.Flags().GetString("addr")
		publicBase, _ := cmd.Flags().GetString("public-base-url")
		corpID, _ := cmd.Flags().GetString("wxwork-corp-id")
		agentID, _ := cmd.Flags().GetInt("wxwork-agent-id")
		secret, _ := cmd.Flags().GetString("wxwork-secret")
		token, _ := cmd.Flags().GetString("wxwork-token")
		aesKey, _ := cmd.Flags().GetString("wxwork-aes-key")
		if corpID == "" || agentID == 0 || secret == "" || token == "" || aesKey == "" {
			return nil, errors.New("wechat-work adapter needs --wxwork-corp-id, --wxwork-agent-id, --wxwork-secret, --wxwork-token, --wxwork-aes-key")
		}
		return gateway.NewWechatWorkAdapter(gateway.WechatWorkConfig{
			Addr:           addr,
			CorpID:         corpID,
			AgentID:        agentID,
			Secret:         secret,
			Token:          token,
			EncodingAESKey: aesKey,
			PublicBaseURL:  publicBase,
		}, log.Default())
	default:
		return nil, errors.New("unknown adapter " + name)
	}
}

// envInt reads an integer from an env var, returning 0 if unset/invalid.
// Used for cobra Int flags whose defaults come from the environment.
func envInt(name string) int {
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	n, _ := strconv.Atoi(v)
	return n
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
