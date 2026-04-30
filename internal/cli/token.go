package cli

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
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

	var ttl string
	createCmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new token (printed once, store it now)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := parseTokenTTL(ttl)
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
			plaintext, err := generateToken()
			if err != nil {
				return err
			}
			if err := st.CreateToken(cmd.Context(), args[0], plaintext, "rw", expiresAt); err != nil {
				return err
			}
			// Token plaintext is the only thing on stdout so the
			// `SHIPD_BOOTSTRAP_TOKEN=$(shipd token create ...)` pattern works.
			// Expiry notice goes to stderr.
			if expiresAt > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "expires at %s\n", formatTime(expiresAt))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", plaintext)
			return nil
		},
	}
	createCmd.Flags().StringVar(&ttl, "ttl", "", "token lifetime, e.g. 90d, 12h, 4w (default: never expires)")

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

func generateToken() (string, error) {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "shipd_" + base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// parseTokenTTL accepts standard Go duration strings (1h, 30m, 90s) plus the
// security-friendly extensions "Nd" (days) and "Nw" (weeks). Empty input
// returns 0, meaning "never expires".
//
// Days/weeks aren't part of time.ParseDuration because they're calendar-
// approximate, but for token lifetimes that's fine — operators think in
// "90 days", not "2160h".
func parseTokenTTL(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if mul, ok := tokenTTLSuffix(s); ok {
		n, err := strconv.Atoi(s[:len(s)-1])
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		if n < 0 {
			return 0, fmt.Errorf("ttl must be positive, got %q", s)
		}
		return time.Duration(n) * mul, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("ttl must be positive, got %q", s)
	}
	return d, nil
}

func tokenTTLSuffix(s string) (time.Duration, bool) {
	if len(s) < 2 {
		return 0, false
	}
	switch s[len(s)-1] {
	case 'd':
		return 24 * time.Hour, true
	case 'w':
		return 7 * 24 * time.Hour, true
	}
	return 0, false
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
