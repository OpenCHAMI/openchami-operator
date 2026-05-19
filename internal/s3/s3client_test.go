// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package s3

import (
	"context"
	"strings"
	"testing"
)

// The constructor's validation paths are the only S3 logic worth covering
// in pure unit tests — exercising the AWS SDK against a real or mocked
// gateway belongs in an integration suite (LocalStack/VersityGW). A
// regression here would prevent the operator from ever wiring a working
// client, so the tests are cheap and worth keeping.

func TestNewClient_RejectsEmptyEndpoint(t *testing.T) {
	_, err := NewClient(context.Background(), Config{
		AccessKey: "k", SecretKey: "s",
	})
	if err == nil {
		t.Fatalf("expected error for empty endpoint, got nil")
	}
	if !strings.Contains(err.Error(), "endpoint") {
		t.Errorf("expected endpoint error, got %q", err)
	}
}

func TestNewClient_RejectsMissingCredentials(t *testing.T) {
	const exampleEndpoint = "http://s3.example"
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no access key", Config{Endpoint: exampleEndpoint, SecretKey: "s"}},
		{"no secret key", Config{Endpoint: exampleEndpoint, AccessKey: "k"}},
		{"both empty", Config{Endpoint: exampleEndpoint}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewClient(context.Background(), tc.cfg)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), "accessKey") && !strings.Contains(err.Error(), "secretKey") {
				t.Errorf("expected credential error, got %q", err)
			}
		})
	}
}

func TestNewClient_AcceptsValidConfig(t *testing.T) {
	c, err := NewClient(context.Background(), Config{
		Endpoint:  "http://s3.example:9000",
		AccessKey: "k",
		SecretKey: "s",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestNewClient_AppliesTLSInsecure(t *testing.T) {
	// We can't easily inspect the constructed http.Client's TLS settings
	// from outside, but we can confirm the constructor accepts the flag
	// without erroring. The flag's runtime effect is exercised by
	// integration tests against a self-signed gateway.
	_, err := NewClient(context.Background(), Config{
		Endpoint:    "https://s3.example",
		AccessKey:   "k",
		SecretKey:   "s",
		TLSInsecure: true,
	})
	if err != nil {
		t.Fatalf("unexpected error with TLSInsecure=true: %v", err)
	}
}
