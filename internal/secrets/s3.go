package secrets

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Fetcher reads objects with AWS SDK v2. Credentials come from the default
// chain (environment, shared config, instance/role credentials); nothing is
// hardcoded and nothing is logged.
type S3Fetcher struct {
	client *s3.Client
}

// NewS3Fetcher builds an S3Fetcher. It does not contact AWS until Fetch.
func NewS3Fetcher(ctx context.Context, region string) (*S3Fetcher, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return &S3Fetcher{client: s3.NewFromConfig(cfg)}, nil
}

// Fetch implements ObjectFetcher.
func (f *S3Fetcher) Fetch(ctx context.Context, bucket, key string) ([]byte, error) {
	out, err := f.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	b, err := io.ReadAll(io.LimitReader(out.Body, maxSecretBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSecretBytes {
		return nil, fmt.Errorf("object exceeds %d bytes", maxSecretBytes)
	}
	return b, nil
}
