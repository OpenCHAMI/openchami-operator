// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package admin_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/admin"
)

// Shared literals — extracted to satisfy goconst and to keep test cases terse.
const (
	initTestClusterName  = "venado"
	initTestDomain       = "venado.test.local"
	initTestVaultAddrTLS = "https://vault.example.com:8200"
	initTestS3Endpoint   = "http://s3.example.com:9000"

	initTestProvisionSubnet = "10.0.0.0/24"
	initTestBMCSubnet       = "10.1.0.0/24"

	initFlagClusterName        = "--cluster-name"
	initFlagDomain             = "--domain"
	initFlagVaultAddr          = "--vault-addr"
	initFlagS3Endpoint         = "--s3-endpoint"
	initFlagProvisionSubnet    = "--provision-subnet"
	initFlagBMCSubnet          = "--bmc-subnet"
	initFlagAllowInsecureVault = "--allow-insecure-vault"
	initFlagVaultAuth          = "--vault-auth"
	initFlagOutput             = "--output"
)

// initBaseArgs returns the minimum required flag set for a happy-path init.
// Each test should append/override as needed.
func initBaseArgs() []string {
	return []string{
		initFlagClusterName, initTestClusterName,
		initFlagDomain, initTestDomain,
		initFlagVaultAddr, initTestVaultAddrTLS,
		initFlagS3Endpoint, initTestS3Endpoint,
	}
}

// initRunCmd builds a fresh InitCmd, wires capture buffers, executes with the
// supplied args, and returns (stdout, stderr, err).
func initRunCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	cmd := admin.InitCmd()
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	// Silence cobra's own usage/error printing on validation failures so the
	// test stderr only contains the strings the implementation writes itself.
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(args)

	err := cmd.Execute()
	return outBuf.String(), errBuf.String(), err
}

// initUnmarshal parses the YAML manifest produced by the init command back
// into a typed *OpenCHAMICluster. Failures fail the test.
func initUnmarshal(t *testing.T, manifest string) *openchamiv1alpha1.OpenCHAMICluster {
	t.Helper()
	var c openchamiv1alpha1.OpenCHAMICluster
	if err := yaml.Unmarshal([]byte(manifest), &c); err != nil {
		t.Fatalf("unmarshalling generated manifest: %v\n--- YAML ---\n%s", err, manifest)
	}
	return &c
}

func TestInitCmd_HappyPathProbeOff(t *testing.T) {
	stdout, _, err := initRunCmd(t, initBaseArgs()...)
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
	if stdout == "" {
		t.Fatal("expected manifest on stdout, got empty string")
	}

	c := initUnmarshal(t, stdout)

	if got := c.Spec.ClusterName; got != initTestClusterName {
		t.Errorf("ClusterName = %q, want %q", got, initTestClusterName)
	}
	if got := c.Spec.Domain; got != initTestDomain {
		t.Errorf("Domain = %q, want %q", got, initTestDomain)
	}
	if got := c.Spec.Platform.Vault.Address; got != initTestVaultAddrTLS {
		t.Errorf("Vault.Address = %q, want %q", got, initTestVaultAddrTLS)
	}
	if c.Spec.NetworkProbe.Enabled {
		t.Error("NetworkProbe.Enabled = true, want false (no subnets supplied)")
	}

	// All four core services enabled by default.
	if !c.Spec.Services.SMD.Enabled {
		t.Error("services.smd.enabled = false, want true")
	}
	if !c.Spec.Services.Tokensmith.Enabled {
		t.Error("services.tokensmith.enabled = false, want true")
	}
	if !c.Spec.Services.BootService.Enabled {
		t.Error("services.bootService.enabled = false, want true")
	}
	if !c.Spec.Services.MetadataService.Enabled {
		t.Error("services.metadataService.enabled = false, want true")
	}

	// CoreDHCP node selector must contain a discriminator value substring-ing
	// the cluster name (webhook rule 6).
	sel := c.Spec.Services.CoreDHCP.NodeSelector
	if len(sel) == 0 {
		t.Fatal("coreDHCP.nodeSelector empty; expected a cluster-name discriminator label")
	}
	foundDiscriminator := false
	for k, v := range sel {
		if strings.Contains(v, initTestClusterName) {
			foundDiscriminator = true
			break
		}
		_ = k
	}
	if !foundDiscriminator {
		t.Errorf("coreDHCP.nodeSelector %v has no value containing %q", sel, initTestClusterName)
	}
}

func TestInitCmd_HappyPathProbeOn(t *testing.T) {
	args := append(initBaseArgs(),
		initFlagProvisionSubnet, initTestProvisionSubnet,
		initFlagBMCSubnet, initTestBMCSubnet,
	)

	stdout, _, err := initRunCmd(t, args...)
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}

	c := initUnmarshal(t, stdout)

	if !c.Spec.NetworkProbe.Enabled {
		t.Error("NetworkProbe.Enabled = false, want true")
	}
	if c.Spec.NetworkProbe.ProvisionNetwork == nil {
		t.Fatal("NetworkProbe.ProvisionNetwork is nil")
	}
	if got := c.Spec.NetworkProbe.ProvisionNetwork.Subnet; got != initTestProvisionSubnet {
		t.Errorf("ProvisionNetwork.Subnet = %q, want %q", got, initTestProvisionSubnet)
	}
	if c.Spec.NetworkProbe.BMCNetwork == nil {
		t.Fatal("NetworkProbe.BMCNetwork is nil")
	}
	if got := c.Spec.NetworkProbe.BMCNetwork.Subnet; got != initTestBMCSubnet {
		t.Errorf("BMCNetwork.Subnet = %q, want %q", got, initTestBMCSubnet)
	}
	if !c.Spec.Services.CoreDHCP.Enabled {
		t.Error("services.coreDHCP.enabled = false, want true")
	}
	if !c.Spec.Services.Magellan.Enabled {
		t.Error("services.magellan.enabled = false, want true")
	}
}

func TestInitCmd_ValidationRejectsBadClusterName(t *testing.T) {
	cases := []struct {
		name        string
		clusterName string
	}{
		{"uppercase", "Venado"},
		{"starts-with-digit", "1venado"},
		{"too-long-33-chars", strings.Repeat("a", 33)},
		{"empty", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{
				initFlagClusterName, tc.clusterName,
				initFlagDomain, initTestDomain,
				initFlagVaultAddr, initTestVaultAddrTLS,
				initFlagS3Endpoint, initTestS3Endpoint,
			}
			stdout, _, err := initRunCmd(t, args...)
			if err == nil {
				t.Fatalf("expected error for cluster-name=%q, got nil", tc.clusterName)
			}
			if stdout != "" {
				t.Errorf("expected no YAML on stdout for invalid input, got:\n%s", stdout)
			}
		})
	}
}

func TestInitCmd_ValidationRejectsBadDomain(t *testing.T) {
	cases := []struct {
		name   string
		domain string
	}{
		{"no-dot", "venado"},
		{"has-scheme", "http://venado.test.local"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{
				initFlagClusterName, initTestClusterName,
				initFlagDomain, tc.domain,
				initFlagVaultAddr, initTestVaultAddrTLS,
				initFlagS3Endpoint, initTestS3Endpoint,
			}
			stdout, _, err := initRunCmd(t, args...)
			if err == nil {
				t.Fatalf("expected error for domain=%q, got nil", tc.domain)
			}
			if stdout != "" {
				t.Errorf("expected no YAML on stdout for invalid input, got:\n%s", stdout)
			}
		})
	}
}

func TestInitCmd_ValidationRejectsHTTPVault(t *testing.T) {
	args := []string{
		initFlagClusterName, initTestClusterName,
		initFlagDomain, initTestDomain,
		initFlagVaultAddr, "http://vault.example.com:8200",
		initFlagS3Endpoint, initTestS3Endpoint,
	}
	stdout, _, err := initRunCmd(t, args...)
	if err == nil {
		t.Fatal("expected error for plain-http vault address without --allow-insecure-vault")
	}
	if stdout != "" {
		t.Errorf("expected no YAML on stdout for invalid input, got:\n%s", stdout)
	}
}

func TestInitCmd_AllowInsecureVaultPasses(t *testing.T) {
	args := []string{
		initFlagClusterName, initTestClusterName,
		initFlagDomain, initTestDomain,
		initFlagVaultAddr, "http://vault.example.com:8200",
		initFlagS3Endpoint, initTestS3Endpoint,
		initFlagAllowInsecureVault,
	}
	stdout, stderrOut, err := initRunCmd(t, args...)
	if err != nil {
		t.Fatalf("expected success with --allow-insecure-vault, got: %v", err)
	}
	if stdout == "" {
		t.Fatal("expected manifest on stdout")
	}
	if !strings.Contains(strings.ToLower(stderrOut), "warning") {
		t.Errorf("expected stderr to contain 'warning', got: %q", stderrOut)
	}
}

func TestInitCmd_AppRoleAuth(t *testing.T) {
	args := append(initBaseArgs(), initFlagVaultAuth, "appRole")

	stdout, _, err := initRunCmd(t, args...)
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}

	c := initUnmarshal(t, stdout)
	if got := string(c.Spec.Platform.Vault.AuthMethod); got != "appRole" {
		t.Errorf("Vault.AuthMethod = %q, want %q", got, "appRole")
	}
}

func TestInitCmd_WriteToFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.yaml")

	args := append(initBaseArgs(), initFlagOutput, path)

	stdout, _, err := initRunCmd(t, args...)
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
	if stdout != "" {
		t.Errorf("expected stdout to be empty when writing to file, got:\n%s", stdout)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("output file not written: %v", err)
	}
	if len(contents) == 0 {
		t.Fatal("output file is empty")
	}

	c := initUnmarshal(t, string(contents))
	if got := c.Spec.ClusterName; got != initTestClusterName {
		t.Errorf("ClusterName from file = %q, want %q", got, initTestClusterName)
	}
}
