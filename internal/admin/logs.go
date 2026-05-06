// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package admin

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// logs.go-scoped constants. Prefixed with `logs` to avoid collision with the
// other `internal/admin` files written by parallel sub-agents.
const (
	logsStdoutSentinel = "-"

	logsEnvAccessKey = "OPENCHAMI_S3_ACCESS_KEY"
	logsEnvSecretKey = "OPENCHAMI_S3_SECRET_KEY"

	logsDefaultLimit      = 100
	logsDefaultDuckDBPath = "duckdb"

	logsBucketSuffix = "-logs"

	logsSchemeHTTPS = "https://"
	logsSchemeHTTP  = "http://"

	logsFormatCSV     = "csv"
	logsFormatJSON    = "json"
	logsFormatParquet = "parquet"

	logsLong = "Query funicular logs in S3-compatible storage (e.g. VersityGW). " +
		"Reads Parquet files written by the legendary-funicular collector " +
		"DaemonSet. Requires the `duckdb` binary on PATH (override with " +
		"--duckdb-path); the operator does NOT embed DuckDB to avoid CGO."
)

// logsRunner is the shape of a function that executes a fully-formed DuckDB
// SQL script and writes any tabular output to stdout. Tests override the
// package-level logsQueryRunner variable to capture the SQL that would be
// dispatched without actually shelling out to duckdb.
type logsRunner func(ctx context.Context, sql string, stdout io.Writer) error

// logsQueryRunner is the indirection point for tests. The default
// implementation shells out to the `duckdb` binary identified by the
// package-level logsDuckDBBinary, which the cobra commands set from the
// caller's --duckdb-path flag (defaults to "duckdb" on $PATH). Tests
// reassign this variable to capture and assert on the generated SQL.
var logsQueryRunner logsRunner = logsRunDuckDB

// logsDuckDBBinary is the path to the duckdb binary used by the default
// logsQueryRunner. It is set from the --duckdb-path flag at command-run
// time. Defaulted here so the default runner is callable in isolation.
var logsDuckDBBinary = logsDefaultDuckDBPath

// logsOpts holds every flag value common to `query` and `export`.
type logsOpts struct {
	cluster     string
	s3Endpoint  string
	s3Bucket    string
	s3AccessKey string
	s3SecretKey string
	sqlOverride string
	service     string
	since       string
	until       string
	limit       int
	duckdbPath  string

	// export-only fields
	format string
	output string
}

// LogsCmd returns the `ochami-admin logs` command tree.
func LogsCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "logs",
		Short: "Query funicular logs in S3",
		Long:  logsLong,
	}
	root.AddCommand(logsQueryCmd(), logsExportCmd())
	return root
}

// logsQueryCmd builds the `ochami-admin logs query` sub-command.
func logsQueryCmd() *cobra.Command {
	o := &logsOpts{}
	cmd := &cobra.Command{
		Use:   "query",
		Short: "Run a SQL query against cluster log Parquet files",
		Long:  logsLong,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.runQuery(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	o.bindCommonFlags(cmd.Flags())
	return cmd
}

// logsExportCmd builds the `ochami-admin logs export` sub-command.
func logsExportCmd() *cobra.Command {
	o := &logsOpts{}
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export query results to a file in csv, json, or parquet format",
		Long:  logsLong,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.runExport(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	o.bindCommonFlags(cmd.Flags())
	cmd.Flags().StringVar(&o.format, "format", logsFormatCSV, "Export format (csv|json|parquet)")
	cmd.Flags().StringVar(&o.output, "output", logsStdoutSentinel, "Output file path, or '-' for stdout")
	return cmd
}

// bindCommonFlags wires the flags shared by `query` and `export`.
func (o *logsOpts) bindCommonFlags(f *pflag.FlagSet) {
	f.StringVar(&o.cluster, "cluster", "", "Cluster name (required; used to derive the default bucket)")
	f.StringVar(&o.s3Endpoint, "s3-endpoint", "", "S3 endpoint URL (required, e.g. http://versitygw:10000)")
	f.StringVar(&o.s3Bucket, "s3-bucket", "", "S3 bucket holding Parquet logs (default: <cluster>-logs)")
	f.StringVar(&o.s3AccessKey, "s3-access-key", "", "S3 access key (falls back to $"+logsEnvAccessKey+")")
	f.StringVar(&o.s3SecretKey, "s3-secret-key", "", "S3 secret key (falls back to $"+logsEnvSecretKey+")")
	f.StringVar(&o.sqlOverride, "sql", "", "Raw SQL passthrough (mutually exclusive with --service/--since/--until/--limit)")
	f.StringVar(&o.service, "service", "", "Filter by service name (e.g. tokensmith)")
	f.StringVar(&o.since, "since", "", "RFC3339 timestamp; include rows where ts >= since")
	f.StringVar(&o.until, "until", "", "RFC3339 timestamp; include rows where ts < until")
	f.IntVar(&o.limit, "limit", logsDefaultLimit, "Maximum number of rows to return")
	f.StringVar(&o.duckdbPath, "duckdb-path", logsDefaultDuckDBPath, "Path to the duckdb binary")
}

// runQuery is the entry point for `ochami-admin logs query`.
func (o *logsOpts) runQuery(ctx context.Context, stdout, stderr io.Writer) error {
	if err := o.validate(); err != nil {
		return err
	}

	sql, err := o.assembleSQL("")
	if err != nil {
		return err
	}
	logsDuckDBBinary = o.duckdbPath
	_, _ = fmt.Fprintf(stderr, "querying logs in bucket %q via %s\n",
		o.resolveBucket(), o.s3Endpoint)
	return logsQueryRunner(ctx, sql, stdout)
}

// runExport is the entry point for `ochami-admin logs export`. It wraps the
// generated SELECT in a `COPY (...) TO '<output>' (FORMAT '<fmt>')` clause.
func (o *logsOpts) runExport(ctx context.Context, stdout, stderr io.Writer) error {
	if err := o.validate(); err != nil {
		return err
	}
	if err := o.validateExport(); err != nil {
		return err
	}

	sql, err := o.assembleSQL(o.exportWrapper())
	if err != nil {
		return err
	}
	logsDuckDBBinary = o.duckdbPath
	_, _ = fmt.Fprintf(stderr, "exporting logs (format=%s) to %s\n",
		o.format, o.output)
	return logsQueryRunner(ctx, sql, stdout)
}

// validate enforces the documented common flag constraints.
func (o *logsOpts) validate() error {
	if o.cluster == "" {
		return fmt.Errorf("--cluster is required")
	}
	if o.s3Endpoint == "" {
		return fmt.Errorf("--s3-endpoint is required")
	}
	if o.sqlOverride != "" {
		if o.service != "" || o.since != "" || o.until != "" {
			return fmt.Errorf("--sql is mutually exclusive with --service/--since/--until")
		}
	}
	return nil
}

// validateExport enforces the export-only flag constraints.
func (o *logsOpts) validateExport() error {
	switch o.format {
	case logsFormatCSV, logsFormatJSON, logsFormatParquet:
	default:
		return fmt.Errorf("--format %q is invalid; must be one of: csv, json, parquet", o.format)
	}
	if o.output == "" {
		return fmt.Errorf("--output is required")
	}
	return nil
}

// resolveBucket returns the user-supplied bucket or the default
// `<cluster>-logs`.
func (o *logsOpts) resolveBucket() string {
	if o.s3Bucket != "" {
		return o.s3Bucket
	}
	return o.cluster + logsBucketSuffix
}

// resolveCredentials picks up credentials from flags, falling back to the
// documented OPENCHAMI_S3_* environment variables.
func (o *logsOpts) resolveCredentials() (string, string) {
	access := o.s3AccessKey
	if access == "" {
		access = os.Getenv(logsEnvAccessKey)
	}
	secret := o.s3SecretKey
	if secret == "" {
		secret = os.Getenv(logsEnvSecretKey)
	}
	return access, secret
}

// assembleSQL prepends the DuckDB httpfs/S3 prelude to either the user-supplied
// SQL (when --sql is set) or the structured-query SQL generated from the other
// flags. When wrap is non-empty, the resulting body is wrapped in a COPY (...)
// clause for export.
func (o *logsOpts) assembleSQL(wrap string) (string, error) {
	body := o.sqlOverride
	if body == "" {
		body = logsBuildSQL(o.resolveBucket(), o.service, o.since, o.until, o.limit)
	}
	body = strings.TrimRight(strings.TrimSpace(body), ";")

	if wrap != "" {
		body = fmt.Sprintf(wrap, body)
	}

	access, secret := o.resolveCredentials()
	prelude, err := logsBuildPrelude(o.s3Endpoint, access, secret)
	if err != nil {
		return "", err
	}
	return prelude + body + ";\n", nil
}

// exportWrapper returns a `COPY (%s) TO '<output>' (FORMAT '<fmt>')` template
// whose `%s` is replaced with the SELECT body. When --output is "-" the COPY
// target is /dev/stdout so DuckDB streams to the caller.
func (o *logsOpts) exportWrapper() string {
	target := o.output
	if target == logsStdoutSentinel {
		target = "/dev/stdout"
	}
	target = logsEscapeSQLString(target)
	format := logsEscapeSQLString(o.format)
	return "COPY (%s) TO '" + target + "' (FORMAT '" + format + "')"
}

// logsBuildSQL produces the structured SELECT that filters on service / time
// window. Each user-supplied string is single-quote-escaped via
// logsEscapeSQLString to defeat SQL injection through the CLI flags.
func logsBuildSQL(bucket, service, since, until string, limit int) string {
	var sb strings.Builder
	sb.WriteString("SELECT *\nFROM read_parquet('s3://")
	sb.WriteString(logsEscapeSQLString(bucket))
	sb.WriteString("/**/*.parquet')\nWHERE 1=1")
	if service != "" {
		sb.WriteString("\n  AND service = '")
		sb.WriteString(logsEscapeSQLString(service))
		sb.WriteString("'")
	}
	if since != "" {
		sb.WriteString("\n  AND ts >= '")
		sb.WriteString(logsEscapeSQLString(since))
		sb.WriteString("'")
	}
	if until != "" {
		sb.WriteString("\n  AND ts <  '")
		sb.WriteString(logsEscapeSQLString(until))
		sb.WriteString("'")
	}
	sb.WriteString("\nORDER BY ts DESC")
	if limit > 0 {
		fmt.Fprintf(&sb, "\nLIMIT %d", limit)
	}
	return sb.String()
}

// logsBuildPrelude returns the DuckDB initialisation SQL: install/load the
// httpfs extension and configure the S3 endpoint + credentials.
func logsBuildPrelude(endpoint, access, secret string) (string, error) {
	host, useSSL, err := logsParseEndpoint(endpoint)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString("INSTALL httpfs;\nLOAD httpfs;\n")
	fmt.Fprintf(&sb, "SET s3_endpoint='%s';\n", logsEscapeSQLString(host))
	fmt.Fprintf(&sb, "SET s3_use_ssl=%t;\n", useSSL)
	sb.WriteString("SET s3_url_style='path';\n")
	fmt.Fprintf(&sb, "SET s3_access_key_id='%s';\n", logsEscapeSQLString(access))
	fmt.Fprintf(&sb, "SET s3_secret_access_key='%s';\n", logsEscapeSQLString(secret))
	return sb.String(), nil
}

// logsParseEndpoint splits "<scheme>://host[:port]" into host (without scheme)
// and a useSSL flag derived from the scheme. A bare "host:port" is accepted
// and treated as plaintext (useSSL=false).
func logsParseEndpoint(endpoint string) (string, bool, error) {
	if endpoint == "" {
		return "", false, fmt.Errorf("s3 endpoint is empty")
	}
	switch {
	case strings.HasPrefix(endpoint, logsSchemeHTTPS):
		return strings.TrimPrefix(endpoint, logsSchemeHTTPS), true, nil
	case strings.HasPrefix(endpoint, logsSchemeHTTP):
		return strings.TrimPrefix(endpoint, logsSchemeHTTP), false, nil
	default:
		return endpoint, false, nil
	}
}

// logsEscapeSQLString doubles single quotes per the SQL string-literal rule,
// which is also DuckDB's accepted form. Defeats injection through CLI flags.
func logsEscapeSQLString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// logsRunDuckDB shells out to the `duckdb` binary identified by the
// package-level logsDuckDBBinary variable (set from --duckdb-path). Stdin
// receives the assembled SQL script; stdout is forwarded to the caller's
// writer; the duckdb binary's stderr is forwarded to os.Stderr so progress
// and parse errors surface.
//
// Tests do not exercise this function — they swap logsQueryRunner.
func logsRunDuckDB(ctx context.Context, sql string, stdout io.Writer) error {
	cmd := exec.CommandContext(ctx, logsDuckDBBinary)
	cmd.Stdin = strings.NewReader(sql)
	cmd.Stdout = stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s: %w", logsDuckDBBinary, err)
	}
	return nil
}
