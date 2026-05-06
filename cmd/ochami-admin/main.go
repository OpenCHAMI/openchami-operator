// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/openchami/openchami-operator/internal/admin"
)

func main() {
	root := &cobra.Command{
		Use:   "ochami-admin",
		Short: "OpenCHAMI operator admin CLI",
		Long:  "Infrastructure operator's tool for managing OpenCHAMI cluster deployments.",
	}

	root.AddCommand(
		admin.InitCmd(),
		admin.DescribeCmd(),
		admin.BackupCmd(),
		admin.RestoreCmd(),
		admin.LogsCmd(),
	)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
