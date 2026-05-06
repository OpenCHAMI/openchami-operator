// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

// Package s3 provides an S3 client interface for VersityGW operations.
// The operator uses S3 only for bucket provisioning and lifecycle management.
// It never reads or writes object data (that is the services' job).
package s3

import "context"

// Client is the interface for S3 bucket operations against VersityGW.
// Implementations must be safe for concurrent use.
type Client interface {
	// EnsureBucket creates the bucket if it does not exist.
	// Returns nil if the bucket already exists and is owned by the
	// configured credentials (BucketAlreadyOwnedByYou is not an error).
	EnsureBucket(ctx context.Context, bucket string) error

	// EnsureLifecycleRule applies a delete-after-N-days lifecycle rule
	// to the bucket. Replaces any existing lifecycle configuration.
	EnsureLifecycleRule(ctx context.Context, bucket string, retentionDays int32) error

	// BucketExists returns true if the bucket exists and is accessible.
	BucketExists(ctx context.Context, bucket string) (bool, error)

	// DeleteBucket deletes the bucket and all its contents.
	// Only called during cluster cleanup when the cleanup annotation is set.
	DeleteBucket(ctx context.Context, bucket string) error
}

// Config holds the connection parameters for an S3 client.
type Config struct {
	Endpoint    string
	AccessKey   string
	SecretKey   string
	Region      string // default "us-east-1"
	TLSInsecure bool
}
