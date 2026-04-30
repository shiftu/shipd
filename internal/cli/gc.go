package cli

import (
	"errors"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/shiftu/shipd/internal/storage"
	"github.com/spf13/cobra"
)

// `shipd gc` reclaims storage from old yanked releases. Like `shipd token`,
// it talks to the local SQLite + blob backend directly — meant to run on
// the same host as `shipd serve`, on a cron, with the same backend flags.
//
// Default behavior is dry-run; --delete is required to actually mutate
// state. The destructive nature is loud on purpose — this breaks any
// download URL pinned to the deleted releases, and there is no undo.
func newGCCmd() *cobra.Command {
	var (
		dataDir   string
		doDelete  bool
		olderThan string
		keepLast  int

		blobBackend string
		s3Bucket    string
		s3Region    string
		s3Endpoint  string
		s3Prefix    string
		s3PathStyle bool
	)
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Reclaim storage from yanked releases (run on the server host)",
		Long: `gc lists yanked releases that were yanked at least --older-than ago.
With --delete, the corresponding metadata rows are removed and the backing
blobs are deleted from the storage backend (FS or S3). Blobs shared with
another release via content-addressed dedup are kept — only metadata is
removed for those.

The --keep-last N flag protects the N most-recently-published releases
per (app, channel, platform) regardless of yank state, so a sequence of
yanks on a slow-moving app can never reduce its storage to nothing —
the most-recent artifact bytes survive so an operator who over-yanked
can recover by un-yanking. Pass --keep-last 0 to disable this safety
net for full cleanup.

Run on the same host (and with the same --data-dir / --blob-backend args)
as your shipd serve.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ttl, err := storage.ParseTTL(olderThan)
			if err != nil {
				return fmt.Errorf("--older-than: %w", err)
			}

			ctx, cancel := signalContext()
			defer cancel()

			blobs, err := buildBlobStore(ctx, blobBackend, storage.S3Config{
				Bucket: s3Bucket, Region: s3Region, Endpoint: s3Endpoint,
				Prefix: s3Prefix, PathStyle: s3PathStyle,
			})
			if err != nil {
				return err
			}
			st, err := storage.Open(dataDir, blobs)
			if err != nil {
				return err
			}
			defer st.Close()

			if keepLast < 0 {
				return fmt.Errorf("--keep-last must be >= 0")
			}
			cands, err := st.GCCandidates(ctx, ttl, keepLast)
			if err != nil {
				return err
			}
			if len(cands) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no candidates")
				return nil
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			banner := "DRY-RUN — pass --delete to actually remove these"
			if doDelete {
				banner = "DELETING — this is destructive"
			}
			fmt.Fprintln(cmd.ErrOrStderr(), banner)
			fmt.Fprintln(tw, "APP\tVERSION\tCHANNEL\tYANKED_AT\tSIZE\tSHA256\tACTION")

			var (
				totalBytes      int64
				blobsDeleted    int
				blobsKept       int
				rowsDeleted     int
				errored         int
			)
			for _, c := range cands {
				totalBytes += c.Size
				action := "would delete"
				if doDelete {
					blobGone, err := st.DeleteReleaseAndBlob(ctx, c.AppName, c.Version, c.Channel)
					switch {
					case err != nil:
						action = "ERROR: " + err.Error()
						errored++
					case blobGone:
						action = "deleted"
						rowsDeleted++
						blobsDeleted++
					default:
						action = "row deleted, blob shared (kept)"
						rowsDeleted++
						blobsKept++
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					c.AppName, c.Version, c.Channel, formatTime(c.YankedAt),
					humanSize(c.Size), short(c.SHA256), action)
			}
			tw.Flush()

			out := cmd.ErrOrStderr()
			if doDelete {
				fmt.Fprintf(out, "\ndeleted %d row(s); %d blob(s) removed, %d kept (shared); %s scanned",
					rowsDeleted, blobsDeleted, blobsKept, humanSize(totalBytes))
				if errored > 0 {
					fmt.Fprintf(out, "; %d error(s)", errored)
				}
				fmt.Fprintln(out)
			} else {
				fmt.Fprintf(out, "\n%d candidate row(s), up to %s reclaimable\n",
					len(cands), humanSize(totalBytes))
			}

			if errored > 0 {
				return errors.New("gc completed with errors")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&dataDir, "data-dir", "./data", "shipd data directory")
	cmd.Flags().BoolVar(&doDelete, "delete", false, "actually delete (default: dry-run)")
	cmd.Flags().StringVar(&olderThan, "older-than", "30d", "minimum age since yank, e.g. 30d, 4w, 12h (use 0 for any age)")
	cmd.Flags().IntVar(&keepLast, "keep-last", 1, "minimum recent releases to keep per (app, channel, platform), regardless of yank state (use 0 for full cleanup)")

	cmd.Flags().StringVar(&blobBackend, "blob-backend", firstNonEmpty(os.Getenv("SHIPD_BLOB_BACKEND"), "fs"), "blob storage backend: fs | s3")
	cmd.Flags().StringVar(&s3Bucket, "s3-bucket", os.Getenv("SHIPD_S3_BUCKET"), "S3 bucket name")
	cmd.Flags().StringVar(&s3Region, "s3-region", os.Getenv("AWS_REGION"), "S3 region")
	cmd.Flags().StringVar(&s3Endpoint, "s3-endpoint", os.Getenv("SHIPD_S3_ENDPOINT"), "S3 endpoint override (for MinIO / R2 / OSS)")
	cmd.Flags().StringVar(&s3Prefix, "s3-prefix", firstNonEmpty(os.Getenv("SHIPD_S3_PREFIX"), "blobs/"), "S3 key prefix")
	cmd.Flags().BoolVar(&s3PathStyle, "s3-path-style", os.Getenv("SHIPD_S3_PATH_STYLE") == "true", "use path-style S3 addressing")
	return cmd
}
