/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package admin

import (
	"github.com/spf13/cobra"
)

// DescribeCmd returns the `ochami-admin describe` command stub.
func DescribeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "describe",
		Short: "Dry-run view of what the operator would apply for a given cluster manifest",
		RunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
	}
}
