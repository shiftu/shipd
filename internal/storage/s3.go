package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
)

// S3Config bundles the values needed to point shipd at an S3-compatible
// object store. AWS proper, MinIO, Cloudflare R2, and Aliyun OSS all work
// with the same shape — set Endpoint to override the default AWS resolver.
//
// Authentication uses the standard aws-sdk-go-v2 default chain: env vars
// (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN), shared
// config files, IAM roles, etc. We don't expose secret-key flags on the CLI
// so they don't end up in shell histories.
type S3Config struct {
	Bucket   string
	Region   string // empty → SDK default chain
	Endpoint string // empty → AWS default; set for MinIO/R2/OSS
	Prefix   string // optional key prefix; trailing "/" recommended

	// PathStyle forces path-style addressing (https://endpoint/bucket/key
	// instead of https://bucket.endpoint/key). MinIO and R2 typically need
	// this; AWS does not.
	PathStyle bool
}

// S3BlobStore stores blobs as S3 objects, keyed by their SHA-256.
//
// Uploads stage through a temp file (see stagedBlob) because content
// addressing requires the hash before the key, and S3's PutObject needs the
// body up front. A HeadObject "skip if exists" check avoids re-uploading the
// same content twice.
type S3BlobStore struct {
	cfg    S3Config
	client *s3.Client
}

// NewS3BlobStore builds an S3 client from the standard SDK config chain plus
// shipd's overrides. It does NOT perform a probe call against S3 — bucket
// existence and credential issues surface on the first Put/Get.
func NewS3BlobStore(ctx context.Context, cfg S3Config) (*S3BlobStore, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("s3: bucket is required")
	}
	if cfg.Prefix != "" && !strings.HasSuffix(cfg.Prefix, "/") {
		cfg.Prefix += "/"
	}

	loadOpts := []func(*config.LoadOptions) error{}
	if cfg.Region != "" {
		loadOpts = append(loadOpts, config.WithRegion(cfg.Region))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("s3: load aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.PathStyle
	})
	return &S3BlobStore{cfg: cfg, client: client}, nil
}

func (s *S3BlobStore) Put(ctx context.Context, body io.Reader) (string, int64, string, error) {
	tmp, sum, size, cleanup, err := stagedBlob(body, "")
	if err != nil {
		return "", 0, "", err
	}
	defer cleanup()

	key := s.objectKey(sum)

	// HeadObject is a few-millisecond round-trip and saves megabytes of
	// upload bandwidth when the same artifact is re-published. Identical
	// content under content addressing means the object already there is
	// byte-identical, so we can short-circuit.
	if _, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	}); err == nil {
		return sum, size, sum, nil
	} else if !isS3NotFound(err) {
		// Surface unexpected errors (auth, permissions, network) instead of
		// silently falling through to a Put that will fail the same way.
		return "", 0, "", fmt.Errorf("s3 head: %w", err)
	}

	if _, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.cfg.Bucket),
		Key:           aws.String(key),
		Body:          tmp,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String("application/octet-stream"),
	}); err != nil {
		return "", 0, "", fmt.Errorf("s3 put: %w", err)
	}
	return sum, size, sum, nil
}

func (s *S3BlobStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(s.objectKey(key)),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("s3 get: %w", err)
	}
	return out.Body, nil
}

// objectKey applies the configured prefix and the same two-character split
// as the FS backend, so a single blob's path looks the same shape across
// backends ("ab/cdef..."). The split is purely cosmetic for S3 — listings
// just navigate prefixes — but it keeps mental models aligned.
func (s *S3BlobStore) objectKey(sum string) string {
	if len(sum) < 2 {
		return s.cfg.Prefix + sum
	}
	return s.cfg.Prefix + sum[:2] + "/" + sum[2:]
}

// isS3NotFound matches the smithy/SDK shapes for "object missing" — both
// the typed NoSuchKey error and the API-level NotFound that HeadObject uses.
func isS3NotFound(err error) bool {
	if err == nil {
		return false
	}
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}
