package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/shiftu/shipd/internal/client"
	"github.com/shiftu/shipd/internal/pkginfo"
	"github.com/spf13/cobra"
)

func newPublishCmd() *cobra.Command {
	var (
		appName     string
		version     string
		channel     string
		platform    string
		notes       string
		bundleID    string
		displayName string
	)
	cmd := &cobra.Command{
		Use:   "publish <file>",
		Short: "Upload a build artifact",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]
			c, err := clientFromFlags(cmd)
			if err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			info, err := f.Stat()
			if err != nil {
				return err
			}

			// Smart defaults: derive app name and platform from filename if not given.
			base := filepath.Base(path)
			if appName == "" {
				appName = pkginfo.InferAppName(base)
			}
			if platform == "" {
				platform = string(pkginfo.Detect(path))
			}
			if version == "" {
				return errors.New("--version is required")
			}

			ctx, cancel := signalContext()
			defer cancel()

			rel, err := c.Publish(ctx, client.PublishOpts{
				App:         appName,
				Version:     version,
				Channel:     channel,
				Platform:    platform,
				Notes:       notes,
				Filename:    base,
				BundleID:    bundleID,
				DisplayName: displayName,
			}, f, info.Size())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"published %s@%s [%s] platform=%s size=%s sha256=%s\n",
				rel.AppName, rel.Version, rel.Channel, platform, humanSize(rel.Size), short(rel.SHA256))
			return nil
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name (default: inferred from filename)")
	cmd.Flags().StringVar(&version, "version", "", "release version (required)")
	cmd.Flags().StringVar(&channel, "channel", "stable", "release channel")
	cmd.Flags().StringVar(&platform, "platform", "", "platform (default: inferred from extension)")
	cmd.Flags().StringVar(&notes, "notes", "", "release notes")
	cmd.Flags().StringVar(&bundleID, "bundle-id", "", "iOS bundle identifier (required for ipa install pages)")
	cmd.Flags().StringVar(&displayName, "display-name", "", "human-readable title shown on install pages")
	return cmd
}

