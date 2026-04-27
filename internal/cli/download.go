package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func newDownloadCmd() *cobra.Command {
	var (
		channel string
		outDir  string
	)
	cmd := &cobra.Command{
		Use:   "download <app[@version]>",
		Short: "Download a release artifact (defaults to latest)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientFromFlags(cmd)
			if err != nil {
				return err
			}
			app, version := splitAppRef(args[0])
			ctx, cancel := signalContext()
			defer cancel()

			if version == "" {
				latest, err := c.Latest(ctx, app, channel)
				if err != nil {
					return err
				}
				version = latest.Version
			}

			if err := os.MkdirAll(outDir, 0o755); err != nil {
				return err
			}
			tmp, err := os.CreateTemp(outDir, ".shipd-download-*")
			if err != nil {
				return err
			}
			tmpName := tmp.Name()
			defer os.Remove(tmpName)

			h := sha256.New()
			expectedSHA, filename, n, err := c.Download(ctx, app, version, channel, io.MultiWriter(tmp, h))
			closeErr := tmp.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}

			gotSHA := hex.EncodeToString(h.Sum(nil))
			if expectedSHA != "" && expectedSHA != gotSHA {
				return fmt.Errorf("sha256 mismatch: got %s, expected %s", gotSHA, expectedSHA)
			}
			if filename == "" {
				filename = fmt.Sprintf("%s-%s", app, version)
			}
			dst := filepath.Join(outDir, filename)
			if err := os.Rename(tmpName, dst); err != nil {
				return errors.Join(err, os.Remove(tmpName))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "downloaded %s (%s, sha256=%s)\n", dst, humanSize(n), short(gotSHA))
			return nil
		},
	}
	cmd.Flags().StringVar(&channel, "channel", "", "release channel (default: stable)")
	cmd.Flags().StringVarP(&outDir, "out-dir", "o", ".", "output directory")
	return cmd
}
