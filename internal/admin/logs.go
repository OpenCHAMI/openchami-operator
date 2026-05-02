/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package admin

import (
	"github.com/spf13/cobra"
)

// LogsCmd returns the `ochami-admin logs` command stub.
func LogsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Query cluster logs stored as Parquet in object storage",
	}
	cmd.AddCommand(logsQueryCmd(), logsExportCmd())
	return cmd
}

func logsQueryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "query",
		Short: "Run a SQL query against cluster log Parquet files",
		RunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
	}
}

func logsExportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "export",
		Short: "Export query results to a file",
		RunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
	}
}
