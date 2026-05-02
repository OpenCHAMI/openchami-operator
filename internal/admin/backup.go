/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package admin

import (
	"github.com/spf13/cobra"
)

// BackupCmd returns the `ochami-admin backup` command stub.
func BackupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "backup",
		Short: "Snapshot cluster infrastructure state to object storage",
		RunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
	}
}
