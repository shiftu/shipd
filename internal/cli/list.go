package cli

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func newListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list [app]",
		Short: "List apps, or releases of an app",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromFlags(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()

			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			defer tw.Flush()

			if len(args) == 0 {
				apps, err := c.ListApps(ctx)
				if err != nil {
					return err
				}
				fmt.Fprintln(tw, "NAME\tPLATFORM\tCREATED")
				for _, a := range apps {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", a.Name, a.Platform, formatTime(a.CreatedAt))
				}
				return nil
			}
			rels, err := c.ListReleases(ctx, args[0])
			if err != nil {
				return err
			}
			fmt.Fprintln(tw, "VERSION\tCHANNEL\tSIZE\tSHA256\tYANKED\tCREATED")
			for _, r := range rels {
				yanked := "-"
				if r.Yanked {
					yanked = "yes"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					r.Version, r.Channel, humanSize(r.Size), short(r.SHA256), yanked, formatTime(r.CreatedAt))
			}
			return nil
		},
	}
	return cmd
}

func formatTime(unix int64) string {
	if unix == 0 {
		return "-"
	}
	return time.Unix(unix, 0).Format("2006-01-02 15:04")
}
