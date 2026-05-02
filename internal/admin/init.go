/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package admin

import (
	"github.com/spf13/cobra"
)

// InitCmd returns the `ochami-admin init` command stub.
func InitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Generate a ready-to-apply OpenCHAMICluster manifest",
		RunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
	}
}
