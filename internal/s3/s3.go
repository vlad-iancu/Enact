// Package s3 wraps the AWS S3 SDK for the platform's object storage needs
// (currently user avatars), mirroring the bedrock package's shape: an
// env-driven Config and a thin Client over the AWS SDK.
package s3

import (
	"bytes"
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

// Config holds the environment-driven S3 settings. Credentials come from
// the standard AWS chain (env vars, shared profile, instance role).
type Config struct {
	// Bucket is the storage bucket for platform objects.
	Bucket string `env:"S3_BUCKET, default=enact-storage"`
	// Region of the bucket; shares AWS_REGION with the Bedrock client.
	Region string `env:"AWS_REGION"`
}

// Client is a thin wrapper over the S3 API scoped to one bucket.
type Client struct {
	api    *awss3.Client
	bucket string
	region string
}

// NewClient loads AWS configuration and returns a Client. Construction does
// not touch the network; credential and bucket problems surface on first use.
func NewClient(ctx context.Context, cfg Config) (*Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("s3: load aws config: %w", err)
	}
	return &Client{
		api:    awss3.NewFromConfig(awsCfg),
		bucket: cfg.Bucket,
		region: cfg.Region,
	}, nil
}

// PutObject stores data under key with the given content type.
func (c *Client) PutObject(ctx context.Context, key, contentType string, data []byte) error {
	_, err := c.api.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("s3: put %s: %w", key, err)
	}
	return nil
}

// DeleteObject removes key; deleting a nonexistent object is not an error
// (S3 semantics), which makes cleanup calls idempotent.
func (c *Client) DeleteObject(ctx context.Context, key string) error {
	_, err := c.api.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3: delete %s: %w", key, err)
	}
	return nil
}

// ObjectURL returns the direct S3 URL of key — the fallback when no CDN is
// configured; requires the bucket (or key prefix) to allow public reads.
func (c *Client) ObjectURL(key string) string {
	return fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", c.bucket, c.region, key)
}
