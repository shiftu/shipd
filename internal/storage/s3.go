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

// OpenSeekable returns a lazy seekable reader. Each chunk of Reads after a
// Seek is satisfied by issuing a fresh GetObject with a Range: bytes=pos-
// header — S3 itself handles partial transfers, so iOS's chunked install
// fetches translate into one GetObject per resume point rather than one
// big stream we'd have to buffer in memory.
//
// size is supplied by the caller (the catalog row already knows it) so we
// avoid the HeadObject round-trip http.ServeContent would otherwise force
// via its Seek(0, io.SeekEnd) probe.
func (s *S3BlobStore) OpenSeekable(ctx context.Context, key string, size int64) (io.ReadSeekCloser, error) {
	return &s3SeekReader{
		ctx:    ctx,
		client: s.client,
		bucket: s.cfg.Bucket,
		key:    s.objectKey(key),
		size:   size,
	}, nil
}

// s3SeekReader implements io.ReadSeekCloser against an S3 object by
// re-opening GetObject with a Range header whenever a Seek invalidates the
// current body stream. Reads inside a contiguous range stream straight from
// the open body — only a Seek that moves the cursor away pays the round-trip.
type s3SeekReader struct {
	ctx    context.Context
	client *s3.Client
	bucket string
	key    string
	size   int64

	pos  int64
	body io.ReadCloser // nil until first Read; closed and re-opened on each non-trivial Seek
}

func (r *s3SeekReader) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	if r.body == nil {
		out, err := r.client.GetObject(r.ctx, &s3.GetObjectInput{
			Bucket: aws.String(r.bucket),
			Key:    aws.String(r.key),
			Range:  aws.String(fmt.Sprintf("bytes=%d-", r.pos)),
		})
		if err != nil {
			if isS3NotFound(err) {
				return 0, ErrNotFound
			}
			return 0, fmt.Errorf("s3 get(range): %w", err)
		}
		r.body = out.Body
	}
	n, err := r.body.Read(p)
	r.pos += int64(n)
	return n, err
}

func (r *s3SeekReader) Seek(offset int64, whence int) (int64, error) {
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = r.pos + offset
	case io.SeekEnd:
		newPos = r.size + offset
	default:
		return 0, fmt.Errorf("s3SeekReader: invalid whence %d", whence)
	}
	if newPos < 0 {
		return 0, fmt.Errorf("s3SeekReader: negative position %d", newPos)
	}
	if newPos != r.pos && r.body != nil {
		_ = r.body.Close()
		r.body = nil
	}
	r.pos = newPos
	return newPos, nil
}

func (r *s3SeekReader) Close() error {
	if r.body == nil {
		return nil
	}
	err := r.body.Close()
	r.body = nil
	return err
}

// Delete removes the object at key. S3 returns 204 even when the object is
// missing, so the call is idempotent — gc can re-run safely.
func (s *S3BlobStore) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(s.objectKey(key)),
	})
	if err != nil {
		return fmt.Errorf("s3 delete: %w", err)
	}
	return nil
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
