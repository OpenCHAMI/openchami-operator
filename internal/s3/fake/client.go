// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

// Package fake provides an in-memory implementation of s3.Client for tests.
package fake

import (
	"context"
	"sync"
	"testing"
)

// Client is an in-memory implementation of s3.Client.
// All operations are thread-safe.
type Client struct {
	mu sync.Mutex

	// Calls records every method invocation.
	Calls map[string][][]any

	// Buckets is the set of bucket names that exist.
	Buckets map[string]bool

	// Lifecycles maps bucket name to retentionDays.
	Lifecycles map[string]int32

	// Errors injects errors keyed by method name.
	Errors map[string]error
}

// NewClient returns an empty FakeClient ready for use.
func NewClient() *Client {
	return &Client{
		Calls:      map[string][][]any{},
		Buckets:    map[string]bool{},
		Lifecycles: map[string]int32{},
		Errors:     map[string]error{},
	}
}

func (c *Client) record(method string, args ...any) {
	c.Calls[method] = append(c.Calls[method], args)
}

func (c *Client) EnsureBucket(_ context.Context, bucket string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("EnsureBucket", bucket)
	if err := c.Errors["EnsureBucket"]; err != nil {
		return err
	}
	c.Buckets[bucket] = true
	return nil
}

func (c *Client) EnsureLifecycleRule(_ context.Context, bucket string, retentionDays int32) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("EnsureLifecycleRule", bucket, retentionDays)
	if err := c.Errors["EnsureLifecycleRule"]; err != nil {
		return err
	}
	c.Lifecycles[bucket] = retentionDays
	return nil
}

func (c *Client) BucketExists(_ context.Context, bucket string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("BucketExists", bucket)
	if err := c.Errors["BucketExists"]; err != nil {
		return false, err
	}
	return c.Buckets[bucket], nil
}

func (c *Client) DeleteBucket(_ context.Context, bucket string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("DeleteBucket", bucket)
	if err := c.Errors["DeleteBucket"]; err != nil {
		return err
	}
	delete(c.Buckets, bucket)
	delete(c.Lifecycles, bucket)
	return nil
}

// AssertCalled fails t if method was not called.
func (c *Client) AssertCalled(t *testing.T, method string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.Calls[method]) == 0 {
		t.Errorf("expected s3.%s to have been called", method)
	}
}

// AssertBucketExists fails t if bucket was not created.
func (c *Client) AssertBucketExists(t *testing.T, bucket string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.Buckets[bucket] {
		t.Errorf("expected s3 bucket %q to exist", bucket)
	}
}

// CallCount returns how many times method has been called.
func (c *Client) CallCount(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.Calls[method])
}
