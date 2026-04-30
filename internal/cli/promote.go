package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// promote is the staged-rollout verb: copy a release that's already on one
// channel (typically beta) onto another channel (typically stable) without
// re-uploading the artifact. The schema's content-addressed blob makes this
// a single new metadata row pointing at the same bytes.
func newPromoteCmd() *cobra.Command {
	var (
		toChannel   string
		fromChannel string
	)
	cmd := &cobra.Command{
		Use:   "promote <app@version> --to <channel>",
		Short: "Copy a release onto another channel without re-uploading",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, version := splitAppRef(args[0])
			if version == "" {
				return errors.New("promote requires app@version")
			}
			if toChannel == "" {
				return errors.New("--to is required")
			}
			c, err := clientFromFlags(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			rel, err := c.Promote(ctx, app, version, fromChannel, toChannel)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "promoted %s@%s -> %s\n", rel.AppName, rel.Version, rel.Channel)
			return nil
		},
	}
	cmd.Flags().StringVar(&toChannel, "to", "", "destination channel (required)")
	cmd.Flags().StringVar(&fromChannel, "from", "", "source channel (default: auto-detect when version is on exactly one channel)")
	return cmd
}
