package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newYankCmd() *cobra.Command {
	var (
		channel string
		reason  string
	)
	cmd := &cobra.Command{
		Use:   "yank <app@version>",
		Short: "Mark a release as withdrawn (does not delete the blob)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, version := splitAppRef(args[0])
			if version == "" {
				return fmt.Errorf("yank requires app@version")
			}
			c, err := clientFromFlags(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			if err := c.Yank(ctx, app, version, channel, reason); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "yanked %s@%s\n", app, version)
			return nil
		},
	}
	cmd.Flags().StringVar(&channel, "channel", "", "release channel (default: stable)")
	cmd.Flags().StringVar(&reason, "reason", "", "human-readable reason")
	return cmd
}
