package cli

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/shiftu/shipd/internal/storage"
	"github.com/spf13/cobra"
)

// Token administration runs against the local SQLite directly. It is intended
// to be run on the server host (where you have the data directory).
func newTokenCmd() *cobra.Command {
	var dataDir string
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage auth tokens (run on the server host)",
	}
	cmd.PersistentFlags().StringVar(&dataDir, "data-dir", "./data", "shipd data directory")

	var (
		ttl   string
		scope string
	)
	createCmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new token (printed once, store it now)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !storage.ValidScopes[scope] {
				return fmt.Errorf("--scope must be r, rw, or admin (got %q)", scope)
			}
			d, err := storage.ParseTTL(ttl)
			if err != nil {
				return fmt.Errorf("--ttl: %w", err)
			}
			var expiresAt int64
			if d > 0 {
				expiresAt = time.Now().Add(d).Unix()
			}

			st, err := storage.Open(dataDir, nil)
			if err != nil {
				return err
			}
			defer st.Close()
			plaintext, err := storage.GenerateTokenPlaintext()
			if err != nil {
				return err
			}
			if err := st.CreateToken(cmd.Context(), args[0], plaintext, scope, expiresAt); err != nil {
				return err
			}
			// Token plaintext is the only thing on stdout so the
			// `SHIPD_BOOTSTRAP_TOKEN=$(shipd token create ...)` pattern works.
			// Scope + expiry notice go to stderr.
			fmt.Fprintf(cmd.ErrOrStderr(), "scope=%s", scope)
			if expiresAt > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), " expires=%s", formatTime(expiresAt))
			}
			fmt.Fprintln(cmd.ErrOrStderr())

			// Friendly warning: an operator bootstrapping a fresh data dir
			// with the default scope ends up unable to call admin endpoints
			// (gc, /api/v1/admin/tokens). Surface that proactively instead
			// of letting them debug a 403 later.
			if scope != "admin" {
				if toks, err := st.ListTokens(cmd.Context()); err == nil {
					hasAdmin := false
					for _, t := range toks {
						if t.Scope == "admin" {
							hasAdmin = true
							break
						}
					}
					if !hasAdmin {
						fmt.Fprintln(cmd.ErrOrStderr(),
							"note: no admin-scope tokens exist — admin endpoints (gc, token creation via API) will be inaccessible. Re-run with --scope admin if needed.")
					}
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", plaintext)
			return nil
		},
	}
	createCmd.Flags().StringVar(&ttl, "ttl", "", "token lifetime, e.g. 90d, 12h, 4w (default: never expires)")
	createCmd.Flags().StringVar(&scope, "scope", "rw", "token scope: r (read-only) | rw (read+write, default) | admin (gc + token mgmt)")

	cmd.AddCommand(
		createCmd,
		&cobra.Command{
			Use:   "list",
			Short: "List token names (does not reveal secrets)",
			RunE: func(cmd *cobra.Command, _ []string) error {
				st, err := storage.Open(dataDir, nil)
				if err != nil {
					return err
				}
				defer st.Close()
				toks, err := st.ListTokens(cmd.Context())
				if err != nil {
					return err
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				defer tw.Flush()
				fmt.Fprintln(tw, "NAME\tSCOPE\tCREATED\tEXPIRES\tLAST_USED")
				for _, t := range toks {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
						t.Name, t.Scope, formatTime(t.CreatedAt), formatExpiry(t.ExpiresAt), formatTime(t.LastUsedAt))
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "revoke <name>",
			Short: "Delete a token by name",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				st, err := storage.Open(dataDir, nil)
				if err != nil {
					return err
				}
				defer st.Close()
				return st.RevokeToken(cmd.Context(), args[0])
			},
		},
	)
	return cmd
}

// formatExpiry renders the expires_at column for `shipd token list`. 0 means
// the token never expires; a past time is shown so operators can spot stale
// rows worth revoking.
func formatExpiry(unix int64) string {
	if unix == 0 {
		return "never"
	}
	if time.Unix(unix, 0).Before(time.Now()) {
		return formatTime(unix) + " (expired)"
	}
	return formatTime(unix)
}
