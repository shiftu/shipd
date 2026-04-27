package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shiftu/shipd/internal/client"
	"github.com/shiftu/shipd/internal/pkginfo"
	"github.com/spf13/cobra"
)

func newPublishCmd() *cobra.Command {
	var (
		appName  string
		version  string
		channel  string
		platform string
		notes    string
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
				appName = inferAppName(base)
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
				App:      appName,
				Version:  version,
				Channel:  channel,
				Platform: platform,
				Notes:    notes,
				Filename: base,
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
	return cmd
}

// inferAppName drops the extension and any trailing -version-like suffix.
func inferAppName(filename string) string {
	name := strings.TrimSuffix(filename, filepath.Ext(filename))
	// Trim a trailing "-vX.Y.Z" or "-X.Y.Z" suffix when present.
	if i := strings.LastIndex(name, "-"); i > 0 {
		tail := name[i+1:]
		if looksLikeVersion(tail) {
			name = name[:i]
		}
	}
	return name
}

func looksLikeVersion(s string) bool {
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return false
	}
	dots := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r == '.':
			dots++
		default:
			return false
		}
	}
	return dots >= 1
}
