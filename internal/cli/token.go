package cli

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"text/tabwriter"

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

	cmd.AddCommand(
		&cobra.Command{
			Use:   "create <name>",
			Short: "Create a new token (printed once, store it now)",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				st, err := storage.Open(dataDir, nil)
				if err != nil {
					return err
				}
				defer st.Close()
				plaintext, err := generateToken()
				if err != nil {
					return err
				}
				if err := st.CreateToken(cmd.Context(), args[0], plaintext, "rw"); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\n", plaintext)
				return nil
			},
		},
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
				fmt.Fprintln(tw, "NAME\tSCOPE\tCREATED\tLAST_USED")
				for _, t := range toks {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t.Name, t.Scope, formatTime(t.CreatedAt), formatTime(t.LastUsedAt))
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
