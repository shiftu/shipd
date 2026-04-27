package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/shiftu/shipd/internal/ai"
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
		aiNotes     bool
		aiSince     string
		aiModel     string
		repoDir     string
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

			if aiNotes {
				if notes != "" {
					return errors.New("--ai-notes and --notes are mutually exclusive")
				}
				generated, err := generateNotes(ctx, c, appName, version, aiSince, aiModel, repoDir)
				if err != nil {
					return fmt.Errorf("ai-notes: %w", err)
				}
				notes = generated
				fmt.Fprintln(cmd.ErrOrStderr(), "--- generated notes ---")
				fmt.Fprintln(cmd.ErrOrStderr(), notes)
				fmt.Fprintln(cmd.ErrOrStderr(), "-----------------------")
			}

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
	cmd.Flags().BoolVar(&aiNotes, "ai-notes", false, "generate release notes from git log via Claude (requires $ANTHROPIC_API_KEY)")
	cmd.Flags().StringVar(&aiSince, "ai-since", "", "git revision to log from (default: tag matching previous shipd release)")
	cmd.Flags().StringVar(&aiModel, "ai-model", "", "Claude model to use for --ai-notes (default: claude-sonnet-4-6)")
	cmd.Flags().StringVar(&repoDir, "repo-dir", "", "git repo directory for --ai-notes (default: current working dir)")
	return cmd
}

// generateNotes runs the AI release-notes pipeline: look up the previous
// shipd release for this app, resolve a git ref for it, then call Claude.
func generateNotes(ctx context.Context, c *client.Client, app, version, sinceFlag, model, repoDir string) (string, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return "", errors.New("ANTHROPIC_API_KEY not set")
	}

	// Look up the most recent published version for this app, if any.
	// "since" defaults to the tag matching that version. If --ai-since is set,
	// it wins regardless.
	prevVersion := ""
	if sinceFlag == "" {
		if rels, err := c.ListReleases(ctx, app); err == nil && len(rels) > 0 {
			prevVersion = rels[0].Version
		}
	}
	since, err := ai.ResolveSinceRef(ctx, sinceFlag, prevVersion, repoDir)
	if err != nil {
		return "", err
	}
	aiClient := ai.NewClient(ai.Config{APIKey: apiKey, Model: model})
	return ai.GenerateReleaseNotes(ctx, aiClient, version, since, repoDir)
}

