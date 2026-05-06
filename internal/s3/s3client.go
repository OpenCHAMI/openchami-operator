// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package s3

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
)

// realClient implements Client against an S3-compatible API (VersityGW,
// LocalStack, MinIO, etc.). It uses path-style addressing because none of
// these gateways advertise virtual-hosted-style bucket DNS, and it pins the
// endpoint via BaseEndpoint rather than the legacy EndpointResolver.
type realClient struct {
	api *awss3.Client
}

// NewClient constructs a production S3 client from cfg.
//
// Region defaults to "us-east-1" when unset (VersityGW ignores region but
// the SDK still requires a value for request signing). TLSInsecure disables
// certificate verification — intended for dev/test against self-signed
// gateways only.
func NewClient(_ context.Context, cfg Config) (Client, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("s3: endpoint is required")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("s3: accessKey and secretKey are required")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}

	awsCfg := aws.Config{
		Region: region,
		Credentials: credentials.NewStaticCredentialsProvider(
			cfg.AccessKey, cfg.SecretKey, ""),
	}
	if cfg.TLSInsecure {
		awsCfg.HTTPClient = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // Config.TLSInsecure is dev-only and documented
			},
		}
	}

	endpoint := cfg.Endpoint
	api := awss3.NewFromConfig(awsCfg, func(o *awss3.Options) {
		o.BaseEndpoint = &endpoint
		o.UsePathStyle = true
	})
	return &realClient{api: api}, nil
}

func (c *realClient) EnsureBucket(ctx context.Context, bucket string) error {
	_, err := c.api.CreateBucket(ctx, &awss3.CreateBucketInput{
		Bucket: &bucket,
	})
	if err == nil {
		return nil
	}
	// BucketAlreadyOwnedByYou and BucketAlreadyExists (when the existing
	// bucket is ours) are both success cases — the goal is convergence,
	// not first-write semantics.
	var owned *s3types.BucketAlreadyOwnedByYou
	if errors.As(err, &owned) {
		return nil
	}
	var exists *s3types.BucketAlreadyExists
	if errors.As(err, &exists) {
		// Verify ownership before treating as success: HeadBucket against
		// the same credentials only succeeds for buckets we can read.
		ok, headErr := c.bucketReachable(ctx, bucket)
		if headErr != nil {
			return fmt.Errorf("creating bucket %s: bucket already exists, ownership check failed: %w", bucket, headErr)
		}
		if !ok {
			return fmt.Errorf("creating bucket %s: bucket already exists and is not owned by us", bucket)
		}
		return nil
	}
	return fmt.Errorf("creating bucket %s: %w", bucket, err)
}

func (c *realClient) EnsureLifecycleRule(ctx context.Context, bucket string, retentionDays int32) error {
	if retentionDays <= 0 {
		// A zero/negative retention would create a rule that never
		// expires — refuse rather than silently produce a no-op rule
		// that masks misconfiguration.
		return fmt.Errorf("s3: retentionDays must be positive, got %d", retentionDays)
	}
	rule := s3types.LifecycleRule{
		ID:     aws.String("openchami-retention"),
		Status: s3types.ExpirationStatusEnabled,
		Filter: &s3types.LifecycleRuleFilter{
			Prefix: aws.String(""),
		},
		Expiration: &s3types.LifecycleExpiration{
			Days: aws.Int32(retentionDays),
		},
	}
	_, err := c.api.PutBucketLifecycleConfiguration(ctx,
		&awss3.PutBucketLifecycleConfigurationInput{
			Bucket: &bucket,
			LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{
				Rules: []s3types.LifecycleRule{rule},
			},
		})
	if err != nil {
		return fmt.Errorf("putting lifecycle on %s: %w", bucket, err)
	}
	return nil
}

func (c *realClient) BucketExists(ctx context.Context, bucket string) (bool, error) {
	return c.bucketReachable(ctx, bucket)
}

// bucketReachable returns true when HeadBucket succeeds and false when the
// gateway returns NotFound. Other errors propagate.
func (c *realClient) bucketReachable(ctx context.Context, bucket string) (bool, error) {
	_, err := c.api.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: &bucket})
	if err == nil {
		return true, nil
	}
	var notFound *s3types.NotFound
	if errors.As(err, &notFound) {
		return false, nil
	}
	// Some gateways (notably VersityGW versions older than 1.0.7) return a
	// generic 404 wrapped in a smithy.GenericAPIError instead of NotFound.
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NotFound" {
		return false, nil
	}
	return false, fmt.Errorf("head bucket %s: %w", bucket, err)
}

func (c *realClient) DeleteBucket(ctx context.Context, bucket string) error {
	if err := c.emptyBucket(ctx, bucket); err != nil {
		return err
	}
	_, err := c.api.DeleteBucket(ctx, &awss3.DeleteBucketInput{Bucket: &bucket})
	if err != nil {
		var nsb *s3types.NoSuchBucket
		if errors.As(err, &nsb) {
			return nil
		}
		return fmt.Errorf("deleting bucket %s: %w", bucket, err)
	}
	return nil
}

// emptyBucket lists and deletes every object in the bucket in 1000-key
// batches. VersityGW and LocalStack do not enable versioning by default;
// versioned buckets would need ListObjectVersions instead — handle that
// when a deployment surfaces the need.
func (c *realClient) emptyBucket(ctx context.Context, bucket string) error {
	paginator := awss3.NewListObjectsV2Paginator(c.api, &awss3.ListObjectsV2Input{
		Bucket: &bucket,
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			var nsb *s3types.NoSuchBucket
			if errors.As(err, &nsb) {
				return nil
			}
			return fmt.Errorf("listing objects in %s: %w", bucket, err)
		}
		if len(page.Contents) == 0 {
			continue
		}
		ids := make([]s3types.ObjectIdentifier, 0, len(page.Contents))
		for _, obj := range page.Contents {
			ids = append(ids, s3types.ObjectIdentifier{Key: obj.Key})
		}
		_, err = c.api.DeleteObjects(ctx, &awss3.DeleteObjectsInput{
			Bucket: &bucket,
			Delete: &s3types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("deleting objects in %s: %w", bucket, err)
		}
	}
	return nil
}
