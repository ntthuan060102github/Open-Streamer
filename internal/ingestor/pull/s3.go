package pull

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/open-streamer/open-streamer/internal/domain"
)

const s3ReadChunk = 188 * 56

// S3Reader streams a single S3 object as raw bytes (expected to be MPEG-TS).
//
// URL format:
//
//	s3://bucket/key/path/file.ts
//	s3://bucket/key?region=us-east-1
//	s3://bucket/key?endpoint=http://minio:9000&region=us-east-1
//
// Credentials are read from Input.Params["access_key"] / ["secret_key"] if
// present; otherwise the default AWS credential chain is used
// (env vars, ~/.aws/credentials, IAM role, etc.).
//
// For looping the same object, the caller is responsible for re-opening the
// reader after Read returns io.EOF (the worker's reconnect logic handles this
// automatically).
type S3Reader struct {
	input domain.Input
	body  io.ReadCloser
	buf   []byte
}

// NewS3Reader constructs an S3Reader.
func NewS3Reader(input domain.Input) *S3Reader {
	return &S3Reader{input: input, buf: make([]byte, s3ReadChunk)}
}

// Open fetches the object and holds the response body for streaming reads.
func (r *S3Reader) Open(ctx context.Context) error {
	u, err := url.Parse(r.input.URL)
	if err != nil {
		return fmt.Errorf("s3 reader: parse url: %w", err)
	}
	if u.Scheme != "s3" {
		return fmt.Errorf("s3 reader: expected s3:// scheme, got %q", u.Scheme)
	}

	bucket := u.Host
	// S3 key: strip leading slash from URL path
	key := strings.TrimPrefix(u.Path, "/")
	if bucket == "" || key == "" {
		return fmt.Errorf("s3 reader: invalid URL %q — must be s3://bucket/key", r.input.URL)
	}

	q := u.Query()
	region := q.Get("region")
	endpoint := q.Get("endpoint")

	opts := []func(*config.LoadOptions) error{}
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}

	// Explicit credentials take priority over the default credential chain.
	// Set via input.Params: {"access_key": "AKID...", "secret_key": "wJal..."}
	if ak := r.input.Params["access_key"]; ak != "" {
		opts = append(opts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(
				ak,
				r.input.Params["secret_key"],
				"",
			),
		))
	}

	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return fmt.Errorf("s3 reader: load aws config: %w", err)
	}

	clientOpts := []func(*s3.Options){}
	if endpoint != "" {
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true // required for MinIO / custom endpoints
		})
	}

	client := s3.NewFromConfig(cfg, clientOpts...)
	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3 reader: GetObject s3://%s/%s: %w", bucket, key, err)
	}

	r.body = out.Body
	return nil
}

func (r *S3Reader) Read(ctx context.Context) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	n, err := r.body.Read(r.buf)
	if n > 0 {
		out := make([]byte, n)
		copy(out, r.buf[:n])
		return out, nil
	}
	if err == io.EOF {
		return nil, io.EOF
	}
	return nil, fmt.Errorf("s3 reader: read body: %w", err)
}

// Close closes the S3 GetObject body, if open.
func (r *S3Reader) Close() error {
	if r.body != nil {
		return r.body.Close()
	}
	return nil
}
