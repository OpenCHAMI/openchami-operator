/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package admin

import (
	"github.com/spf13/cobra"
)

// RestoreCmd returns the `ochami-admin restore` command stub.
func RestoreCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restore",
		Short: "Restore cluster infrastructure from a backup snapshot",
		RunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
	}
}
