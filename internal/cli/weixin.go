package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/shiftu/shipd/internal/gateway"
	"github.com/spf13/cobra"
)

// `shipd gateway weixin-login` walks a user through the iLink QR login flow
// and persists the resulting bot_token to disk so `shipd gateway serve
// --adapter weixin` can pick it up later.
func newWeixinLoginCmd() *cobra.Command {
	var (
		stateDir string
		botType  string
	)
	cmd := &cobra.Command{
		Use:   "weixin-login",
		Short: "Interactive QR login for the WeChat (iLink) personal-account adapter",
		Long: `Run an interactive QR login against Tencent's iLink bot endpoint and
persist the resulting bot_token under --state-dir for later reuse by
'shipd gateway serve --adapter weixin'.

NOTE: iLink is reverse-engineered from public reference implementations.
This adapter may stop working without notice and may not be ToS-compliant
in your jurisdiction. Use at your own risk.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			acc, err := gateway.QRLoginWeixin(ctx, gateway.WeixinQRLoginConfig{
				BotType:  botType,
				StateDir: stateDir,
				Out:      cmd.OutOrStdout(),
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStderr(),
				"saved %s/%s.json — start the gateway with --weixin-account-id %s\n",
				stateDir, acc.AccountID, acc.AccountID)
			return nil
		},
	}
	cmd.Flags().StringVar(&stateDir, "state-dir", defaultWeixinStateDir(), "directory for persisted Weixin credentials")
	cmd.Flags().StringVar(&botType, "bot-type", "3", "iLink bot_type parameter (rarely needs changing)")
	return cmd
}

// defaultWeixinStateDir picks a reasonable default depending on what's set.
// In development users typically run from the repo with --data-dir ./data;
// nesting weixin state under that keeps everything in one place.
func defaultWeixinStateDir() string {
	if v := os.Getenv("SHIPD_WEIXIN_STATE_DIR"); v != "" {
		return v
	}
	return "./data/weixin"
}

// loadWeixinAdapter is called from the gateway factory; pulled out so the
// adapter can read either a persisted account by ID or per-flag overrides.
func loadWeixinAdapter(cmd *cobra.Command) (gateway.Adapter, error) {
	stateDir, _ := cmd.Flags().GetString("weixin-state-dir")
	accountID, _ := cmd.Flags().GetString("weixin-account-id")
	tokenOverride, _ := cmd.Flags().GetString("weixin-token")
	baseOverride, _ := cmd.Flags().GetString("weixin-base-url")

	if accountID == "" {
		return nil, errors.New("weixin adapter needs --weixin-account-id (run `shipd gateway weixin-login` first)")
	}

	cfg := gateway.WeixinConfig{
		AccountID: accountID,
		Token:     tokenOverride,
		BaseURL:   baseOverride,
		StateDir:  stateDir,
	}
	if cfg.Token == "" {
		acc, err := gateway.LoadWeixinAccount(stateDir, accountID)
		if err != nil {
			return nil, fmt.Errorf("load weixin account %s from %s: %w", accountID, stateDir, err)
		}
		cfg.Token = acc.Token
		if cfg.BaseURL == "" {
			cfg.BaseURL = acc.BaseURL
		}
	}
	return gateway.NewWeixinAdapter(cfg, nil)
}
