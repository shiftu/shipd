package cli

import (
	"context"
	"errors"
	"log"
	"os"

	"github.com/shiftu/shipd/internal/server"
	"github.com/shiftu/shipd/internal/storage"
	"github.com/spf13/cobra"
)

func newServeCmd() *cobra.Command {
	var (
		addr          string
		dataDir       string
		publicReads   bool
		publicBaseURL string

		blobBackend string
		s3Bucket    string
		s3Region    string
		s3Endpoint  string
		s3Prefix    string
		s3PathStyle bool
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the shipd server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalContext()
			defer cancel()

			blobs, err := buildBlobStore(ctx, blobBackend, storage.S3Config{
				Bucket:    s3Bucket,
				Region:    s3Region,
				Endpoint:  s3Endpoint,
				Prefix:    s3Prefix,
				PathStyle: s3PathStyle,
			})
			if err != nil {
				return err
			}
			st, err := storage.Open(dataDir, blobs)
			if err != nil {
				return err
			}
			defer st.Close()
			s := server.New(server.Config{
				Addr:           addr,
				PublicReads:    publicReads,
				BootstrapToken: os.Getenv("SHIPD_BOOTSTRAP_TOKEN"),
				PublicBaseURL:  publicBaseURL,
			}, st, log.Default())
			return s.ListenAndServe(ctx)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", ":8080", "listen address")
	cmd.Flags().StringVar(&dataDir, "data-dir", "./data", "directory for SQLite (and blobs when --blob-backend=fs)")
	cmd.Flags().BoolVar(&publicReads, "public-reads", false, "allow unauthenticated read endpoints")
	cmd.Flags().StringVar(&publicBaseURL, "public-base-url", os.Getenv("SHIPD_PUBLIC_BASE_URL"),
		"public URL prefix for install pages and QR codes (default: derived from request)")

	cmd.Flags().StringVar(&blobBackend, "blob-backend", firstNonEmpty(os.Getenv("SHIPD_BLOB_BACKEND"), "fs"),
		"blob storage backend: fs | s3")
	cmd.Flags().StringVar(&s3Bucket, "s3-bucket", os.Getenv("SHIPD_S3_BUCKET"), "S3 bucket name")
	cmd.Flags().StringVar(&s3Region, "s3-region", os.Getenv("AWS_REGION"), "S3 region (default: AWS SDK chain)")
	cmd.Flags().StringVar(&s3Endpoint, "s3-endpoint", os.Getenv("SHIPD_S3_ENDPOINT"), "S3 endpoint override (for MinIO / R2 / OSS)")
	cmd.Flags().StringVar(&s3Prefix, "s3-prefix", firstNonEmpty(os.Getenv("SHIPD_S3_PREFIX"), "blobs/"), "S3 key prefix")
	cmd.Flags().BoolVar(&s3PathStyle, "s3-path-style", os.Getenv("SHIPD_S3_PATH_STYLE") == "true", "use path-style S3 addressing (set true for MinIO/R2)")
	return cmd
}

// buildBlobStore returns the configured backend. fs is the zero-config
// default (storage.Open creates one under <dataDir>/blobs when blobs is nil);
// s3 needs --s3-bucket and uses the AWS SDK's default credential chain
// (env vars, shared config, IAM role).
func buildBlobStore(ctx context.Context, backend string, s3 storage.S3Config) (storage.BlobStore, error) {
	switch backend {
	case "", "fs":
		return nil, nil
	case "s3":
		if s3.Bucket == "" {
			return nil, errors.New("--s3-bucket is required when --blob-backend=s3")
		}
		return storage.NewS3BlobStore(ctx, s3)
	default:
		return nil, errors.New("unknown --blob-backend " + backend)
	}
}
