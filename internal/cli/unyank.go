package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// `shipd unyank` reverses a yank. Mainly useful as the recovery path for
// gc's --keep-last safety net: a release whose bytes were preserved by
// keep-last but whose row is still flagged yanked can be brought back to
// live status with one command, without a re-publish.
func newUnyankCmd() *cobra.Command {
	var channel string
	cmd := &cobra.Command{
		Use:   "unyank <app@version>",
		Short: "Reverse a yank — bring a withdrawn release back to live status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, version := splitAppRef(args[0])
			if version == "" {
				return errors.New("unyank requires app@version")
			}
			c, err := clientFromFlags(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			if err := c.Unyank(ctx, app, version, channel); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "unyanked %s@%s\n", app, version)
			return nil
		},
	}
	cmd.Flags().StringVar(&channel, "channel", "", "release channel (default: stable)")
	return cmd
}
