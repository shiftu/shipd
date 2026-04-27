package cli

import (
	"errors"
	"log"
	"os"

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
	serve.Flags().StringVar(&adapter, "adapter", "stdio", "adapter: stdio | feishu")
	serve.Flags().String("addr", ":8081", "listen address (feishu only)")
	serve.Flags().String("feishu-app-id", os.Getenv("FEISHU_APP_ID"), "Feishu app ID (or $FEISHU_APP_ID)")
	serve.Flags().String("feishu-app-secret", os.Getenv("FEISHU_APP_SECRET"), "Feishu app secret (or $FEISHU_APP_SECRET)")
	serve.Flags().String("feishu-verification-token", os.Getenv("FEISHU_VERIFICATION_TOKEN"), "Feishu event verification token (or $FEISHU_VERIFICATION_TOKEN)")
	serve.Flags().String("ai-model", "", "Claude model for the 'ask' verb (default: claude-sonnet-4-6, requires $ANTHROPIC_API_KEY)")

	cmd.AddCommand(serve)
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
		addr, _ := cmd.Flags().GetString("addr")
		appID, _ := cmd.Flags().GetString("feishu-app-id")
		appSecret, _ := cmd.Flags().GetString("feishu-app-secret")
		vtok, _ := cmd.Flags().GetString("feishu-verification-token")
		if appID == "" || appSecret == "" {
			return nil, errors.New("feishu adapter needs --feishu-app-id and --feishu-app-secret")
		}
		return gateway.NewFeishuAdapter(gateway.FeishuConfig{
			Addr:              addr,
			AppID:             appID,
			AppSecret:         appSecret,
			VerificationToken: vtok,
		}, log.Default()), nil
	default:
		return nil, errors.New("unknown adapter " + name)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
