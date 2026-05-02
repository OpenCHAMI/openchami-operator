/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package admin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// Shared literals — extracted to satisfy goconst and to keep test cases terse.
const (
	logsTestCluster        = "venado"
	logsTestS3EndpointHTTP = "http://versitygw.example.com:10000"
	logsTestS3EndpointTLS  = "https://versitygw.example.com:443"
	logsTestService        = "tokensmith"
	logsTestSince          = "2026-05-01T00:00:00Z"
	logsTestSQLLiteral     = "SELECT 1"

	logsFlagCluster    = "--cluster"
	logsFlagS3Endpoint = "--s3-endpoint"
	logsFlagS3Bucket   = "--s3-bucket"
	logsFlagSQL        = "--sql"
	logsFlagService    = "--service"
	logsFlagSince      = "--since"
	logsFlagLimit      = "--limit"
	logsFlagFormat     = "--format"
	logsFlagOutput     = "--output"
)

// logsTestCapture holds the captured invocation of logsQueryRunner so tests
// can assert on the SQL that would have been sent to duckdb without actually
// shelling out.
type logsTestCapture struct {
	called bool
	sql    string
}

// logsInstallCapture swaps logsQueryRunner with a stub that records the SQL
// and returns nil. The original runner is restored via t.Cleanup.
func logsInstallCapture(t *testing.T) *logsTestCapture {
	t.Helper()
	cap := &logsTestCapture{}
	prev := logsQueryRunner
	logsQueryRunner = func(_ context.Context, sql string, _ io.Writer) error {
		cap.called = true
		cap.sql = sql
		return nil
	}
	t.Cleanup(func() { logsQueryRunner = prev })
	return cap
}

// logsRunSubcmd builds a fresh `logs` command, wires capture buffers,
// runs the named sub-command with the supplied flag args, and returns
// (stderr, err). Stdout is discarded — the runner stub captures the SQL
// directly so stdout content is not test-relevant.
func logsRunSubcmd(t *testing.T, sub string, args ...string) (string, error) {
	t.Helper()
	root := LogsCmd()
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SilenceUsage = true
	root.SilenceErrors = true
	root.SetArgs(append([]string{sub}, args...))
	err := root.Execute()
	return errBuf.String(), err
}

// logsRunQuery is a thin wrapper around logsRunSubcmd("query", ...).
func logsRunQuery(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return logsRunSubcmd(t, "query", args...)
}

// logsRunExport is a thin wrapper around logsRunSubcmd("export", ...).
func logsRunExport(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return logsRunSubcmd(t, "export", args...)
}

// logsBaseQueryArgs returns the minimum required flag set for a happy-path
// `logs query` invocation.
func logsBaseQueryArgs() []string {
	return []string{
		logsFlagCluster, logsTestCluster,
		logsFlagS3Endpoint, logsTestS3EndpointHTTP,
	}
}

func TestLogsQuery_SQLPassthrough(t *testing.T) {
	cap := logsInstallCapture(t)

	args := append(logsBaseQueryArgs(), logsFlagSQL, logsTestSQLLiteral)
	if _, err := logsRunQuery(t, args...); err != nil {
		t.Fatalf("query: unexpected error: %v", err)
	}
	if !cap.called {
		t.Fatalf("query runner was not invoked")
	}
	if !strings.Contains(cap.sql, logsTestSQLLiteral) {
		t.Errorf("expected literal SQL %q to appear in dispatched script, got:\n%s",
			logsTestSQLLiteral, cap.sql)
	}
	// Structured-query path must NOT have been used: the read_parquet glob
	// is the unique fingerprint of logsBuildSQL.
	if strings.Contains(cap.sql, "read_parquet(") {
		t.Errorf("structured-query path leaked into --sql passthrough; got:\n%s", cap.sql)
	}
}

func TestLogsQuery_StructuredQuery(t *testing.T) {
	cap := logsInstallCapture(t)

	args := append(logsBaseQueryArgs(),
		logsFlagService, logsTestService,
		logsFlagSince, logsTestSince,
		logsFlagLimit, "50",
	)
	if _, err := logsRunQuery(t, args...); err != nil {
		t.Fatalf("query: unexpected error: %v", err)
	}
	for _, want := range []string{
		"service = 'tokensmith'",
		"ts >= '2026-05-01T00:00:00Z'",
		"LIMIT 50",
		"ORDER BY ts DESC",
		"read_parquet('s3://venado-logs/**/*.parquet')",
	} {
		if !strings.Contains(cap.sql, want) {
			t.Errorf("expected dispatched SQL to contain %q; full SQL:\n%s", want, cap.sql)
		}
	}
}

func TestLogsQuery_SQLEscaping(t *testing.T) {
	cap := logsInstallCapture(t)

	const evil = "foo'; DROP TABLE x; --"
	args := append(logsBaseQueryArgs(), logsFlagService, evil)
	if _, err := logsRunQuery(t, args...); err != nil {
		t.Fatalf("query: unexpected error: %v", err)
	}
	// The apostrophe in the user input must be doubled to defuse the
	// would-be statement break. After escaping the literal in the SQL
	// becomes:  service = 'foo''; DROP TABLE x; --'
	const wantEscaped = "service = 'foo''; DROP TABLE x; --'"
	if !strings.Contains(cap.sql, wantEscaped) {
		t.Errorf("expected escaped service literal %q; full SQL:\n%s", wantEscaped, cap.sql)
	}
	// The unescaped version (which would close the string and execute the
	// follow-on statement) must NOT appear.
	const wantNotPresent = "service = 'foo'; DROP TABLE x; --'"
	if strings.Contains(cap.sql, wantNotPresent) {
		t.Errorf("unescaped injection sequence leaked into SQL; full SQL:\n%s", cap.sql)
	}
}

func TestLogsQuery_BucketDefault(t *testing.T) {
	cap := logsInstallCapture(t)

	if _, err := logsRunQuery(t, logsBaseQueryArgs()...); err != nil {
		t.Fatalf("query: unexpected error: %v", err)
	}
	const wantPath = "s3://venado-logs/**/*.parquet"
	if !strings.Contains(cap.sql, wantPath) {
		t.Errorf("expected default bucket path %q; full SQL:\n%s", wantPath, cap.sql)
	}
}

func TestLogsQuery_BucketOverride(t *testing.T) {
	cap := logsInstallCapture(t)

	args := append(logsBaseQueryArgs(), logsFlagS3Bucket, "custom-bucket")
	if _, err := logsRunQuery(t, args...); err != nil {
		t.Fatalf("query: unexpected error: %v", err)
	}
	if !strings.Contains(cap.sql, "s3://custom-bucket/**/*.parquet") {
		t.Errorf("expected custom bucket path; full SQL:\n%s", cap.sql)
	}
}

func TestLogsQuery_HTTPSEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		wantSSL  string // exact substring expected in the prelude
		wantHost string
	}{
		{
			name:     "HTTPS",
			endpoint: logsTestS3EndpointTLS,
			wantSSL:  "SET s3_use_ssl=true;",
			wantHost: "SET s3_endpoint='versitygw.example.com:443';",
		},
		{
			name:     "HTTP",
			endpoint: logsTestS3EndpointHTTP,
			wantSSL:  "SET s3_use_ssl=false;",
			wantHost: "SET s3_endpoint='versitygw.example.com:10000';",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := logsInstallCapture(t)
			args := []string{
				logsFlagCluster, logsTestCluster,
				logsFlagS3Endpoint, tc.endpoint,
			}
			if _, err := logsRunQuery(t, args...); err != nil {
				t.Fatalf("query: unexpected error: %v", err)
			}
			if !strings.Contains(cap.sql, tc.wantSSL) {
				t.Errorf("missing %q in prelude; got:\n%s", tc.wantSSL, cap.sql)
			}
			if !strings.Contains(cap.sql, tc.wantHost) {
				t.Errorf("missing %q in prelude; got:\n%s", tc.wantHost, cap.sql)
			}
		})
	}
}

func TestLogsExport_CSV(t *testing.T) {
	cap := logsInstallCapture(t)

	args := []string{
		logsFlagCluster, logsTestCluster,
		logsFlagS3Endpoint, logsTestS3EndpointHTTP,
		logsFlagFormat, "csv",
		logsFlagOutput, "/tmp/results.csv",
	}
	if _, err := logsRunExport(t, args...); err != nil {
		t.Fatalf("export: unexpected error: %v", err)
	}
	const wantWrap = "COPY ("
	const wantTarget = "TO '/tmp/results.csv' (FORMAT 'csv')"
	if !strings.Contains(cap.sql, wantWrap) {
		t.Errorf("expected COPY wrapper in dispatched SQL; full SQL:\n%s", cap.sql)
	}
	if !strings.Contains(cap.sql, wantTarget) {
		t.Errorf("expected %q in dispatched SQL; full SQL:\n%s", wantTarget, cap.sql)
	}
	// The wrapped SELECT body must still contain the structured query.
	if !strings.Contains(cap.sql, "read_parquet('s3://venado-logs/**/*.parquet')") {
		t.Errorf("expected wrapped SELECT body in dispatched SQL; full SQL:\n%s", cap.sql)
	}
}

func TestLogsExport_StdoutSentinel(t *testing.T) {
	cap := logsInstallCapture(t)

	args := []string{
		logsFlagCluster, logsTestCluster,
		logsFlagS3Endpoint, logsTestS3EndpointHTTP,
		logsFlagFormat, "json",
		logsFlagOutput, "-",
	}
	if _, err := logsRunExport(t, args...); err != nil {
		t.Fatalf("export: unexpected error: %v", err)
	}
	const wantTarget = "TO '/dev/stdout' (FORMAT 'json')"
	if !strings.Contains(cap.sql, wantTarget) {
		t.Errorf("expected %q for '-' output; full SQL:\n%s", wantTarget, cap.sql)
	}
}

func TestLogsExport_InvalidFormat(t *testing.T) {
	logsInstallCapture(t)

	args := []string{
		logsFlagCluster, logsTestCluster,
		logsFlagS3Endpoint, logsTestS3EndpointHTTP,
		logsFlagFormat, "xml",
		logsFlagOutput, "/tmp/x",
	}
	_, err := logsRunExport(t, args...)
	if err == nil {
		t.Fatalf("expected error for invalid --format, got nil")
	}
	if !strings.Contains(err.Error(), "format") {
		t.Errorf("expected error mentioning 'format'; got %v", err)
	}
}

func TestLogsQuery_MissingRequiredFlags(t *testing.T) {
	logsInstallCapture(t)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "MissingCluster",
			args: []string{logsFlagS3Endpoint, logsTestS3EndpointHTTP},
			want: "--cluster",
		},
		{
			name: "MissingEndpoint",
			args: []string{logsFlagCluster, logsTestCluster},
			want: "--s3-endpoint",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := logsRunQuery(t, tc.args...)
			if err == nil {
				t.Fatalf("expected error mentioning %q; got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected error to mention %q; got %v", tc.want, err)
			}
		})
	}
}

func TestLogsQuery_SQLMutuallyExclusive(t *testing.T) {
	logsInstallCapture(t)

	args := append(logsBaseQueryArgs(),
		logsFlagSQL, logsTestSQLLiteral,
		logsFlagService, logsTestService,
	)
	_, err := logsRunQuery(t, args...)
	if err == nil {
		t.Fatalf("expected mutual-exclusion error; got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected error to mention mutual exclusion; got %v", err)
	}
}

func TestLogsQuery_RunnerErrorPropagates(t *testing.T) {
	prev := logsQueryRunner
	t.Cleanup(func() { logsQueryRunner = prev })
	sentinel := errors.New("duckdb went sideways")
	logsQueryRunner = func(_ context.Context, _ string, _ io.Writer) error {
		return sentinel
	}

	_, err := logsRunQuery(t, logsBaseQueryArgs()...)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error to propagate; got %v", err)
	}
}

func TestLogsBuildSQL_NoLimit(t *testing.T) {
	got := logsBuildSQL("b", "", "", "", 0)
	if strings.Contains(got, "LIMIT") {
		t.Errorf("limit=0 should omit LIMIT clause; got:\n%s", got)
	}
}

func TestLogsParseEndpoint_Bare(t *testing.T) {
	host, useSSL, err := logsParseEndpoint("foo:9000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "foo:9000" || useSSL {
		t.Errorf("bare endpoint should be (foo:9000, false); got (%s, %t)", host, useSSL)
	}
}

func TestLogsParseEndpoint_Empty(t *testing.T) {
	if _, _, err := logsParseEndpoint(""); err == nil {
		t.Fatalf("expected error for empty endpoint")
	}
}
