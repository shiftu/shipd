package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func newInfoCmd() *cobra.Command {
	var channel string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "info <app[@version]>",
		Short: "Show release details (defaults to latest)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromFlags(cmd)
			if err != nil {
				return err
			}
			app, version := splitAppRef(args[0])
			ctx, cancel := signalContext()
			defer cancel()

			var rel any
			if version == "" {
				rel, err = c.Latest(ctx, app, channel)
			} else {
				rel, err = c.GetRelease(ctx, app, version, channel)
			}
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rel)
			}
			b, _ := json.MarshalIndent(rel, "", "  ")
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
			return nil
		},
	}
	cmd.Flags().StringVar(&channel, "channel", "", "release channel (default: stable)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON output (default is also JSON for now)")
	return cmd
}

// splitAppRef splits "name" or "name@version" — version is empty when omitted.
func splitAppRef(ref string) (string, string) {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
}
