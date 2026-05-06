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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/admin"
)

// Shared literals — extracted to satisfy goconst and to keep test cases terse.
const (
	describeTestClusterName = "venado"
	describeTestDomain      = "venado.hpc.example.com"
	describeTestNamespace   = "openchami-venado"

	// Use .svc.cluster.local hostnames so the NetworkPoliciesReconciler's
	// VaultEgressPeer / VersityGWEgressPeer helpers take their no-DNS branch.
	// That keeps the describe test hermetic — no real DNS lookups.
	describeTestVaultAddr  = "https://vault.vault-system.svc.cluster.local:8200"
	describeTestS3Endpoint = "http://versitygw.versitygw-system.svc.cluster.local:9000"

	describeFlagFile        = "--file"
	describeFlagShort       = "-f"
	describeFlagShowDetails = "--show-details"
	describeStdinSentinel   = "-"

	describeFixtureFile = "cluster.yaml"
)

// describeBuildCluster returns a fully-populated CR object suitable for the
// happy-path tests. All four core services are enabled; CoreDHCP/Magellan are
// disabled to keep node-selector requirements out of the fixture.
func describeBuildCluster(t *testing.T) *openchamiv1alpha1.OpenCHAMICluster {
	t.Helper()
	return &openchamiv1alpha1.OpenCHAMICluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "openchami.openchami.org/v1alpha1",
			Kind:       "OpenCHAMICluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      describeTestClusterName,
			Namespace: "default",
		},
		Spec: openchamiv1alpha1.OpenCHAMIClusterSpec{
			ClusterName: describeTestClusterName,
			Domain:      describeTestDomain,
			Platform: openchamiv1alpha1.PlatformSpec{
				Vault: openchamiv1alpha1.VaultSpec{
					Address:    describeTestVaultAddr,
					AuthMethod: openchamiv1alpha1.VaultAuthMethodKubernetes,
				},
				ObjectStorage: openchamiv1alpha1.ObjectStorageSpec{
					Endpoint: describeTestS3Endpoint,
				},
			},
			Services: openchamiv1alpha1.ServicesSpec{
				SMD: openchamiv1alpha1.SMDSpec{
					ServiceDefaults: openchamiv1alpha1.ServiceDefaults{Enabled: true, Replicas: 2},
				},
				Tokensmith: openchamiv1alpha1.TokensmithSpec{
					ServiceDefaults: openchamiv1alpha1.ServiceDefaults{Enabled: true, Replicas: 1},
					OIDCProvider:    "vault",
				},
				BootService: openchamiv1alpha1.BootServiceSpec{
					ServiceDefaults: openchamiv1alpha1.ServiceDefaults{Enabled: true, Replicas: 2},
				},
				MetadataService: openchamiv1alpha1.MetadataServiceSpec{
					ServiceDefaults: openchamiv1alpha1.ServiceDefaults{Enabled: true, Replicas: 2},
				},
				CoreDHCP: openchamiv1alpha1.CoreDHCPSpec{Enabled: false},
				Magellan: openchamiv1alpha1.MagellanSpec{Enabled: false},
			},
		},
	}
}

// describeWriteFixture marshals the cluster to YAML, writes it to a temp file,
// and returns the path. Always uses t.TempDir so cleanup is automatic.
func describeWriteFixture(t *testing.T, cluster *openchamiv1alpha1.OpenCHAMICluster) string {
	t.Helper()
	data, err := yaml.Marshal(cluster)
	if err != nil {
		t.Fatalf("marshalling fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), describeFixtureFile)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing fixture %s: %v", path, err)
	}
	return path
}

// describeRun executes a fresh DescribeCmd, capturing stdout/stderr and
// returning (stdout, err). The command silences cobra's own usage/error
// printing so callers see only what the implementation writes.
func describeRun(t *testing.T, in string, args ...string) (string, error) {
	t.Helper()
	cmd := admin.DescribeCmd()
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if in != "" {
		cmd.SetIn(strings.NewReader(in))
	}
	cmd.SetArgs(args)
	err := cmd.Execute()
	return outBuf.String(), err
}

func TestDescribe_HappyPath(t *testing.T) {
	cluster := describeBuildCluster(t)
	path := describeWriteFixture(t, cluster)

	out, err := describeRun(t, "", describeFlagFile, path)
	if err != nil {
		t.Fatalf("DescribeCmd returned error: %v\n--- stdout ---\n%s", err, out)
	}

	mustContain(t, out, "=== Cluster: "+describeTestClusterName+" ===")
	mustContain(t, out, "Domain:    "+describeTestDomain)
	mustContain(t, out, "Namespace: "+describeTestNamespace)

	// Each enabled service must produce a Deployment line in the appropriate
	// namespace. We only check the "Deployment/<ns>/<name>" prefix; the tail
	// (replicas/image) is implementation detail covered by --show-details.
	for _, svc := range []string{"smd", "tokensmith", "boot-service", "metadata-service"} {
		needle := "Deployment/" + describeTestNamespace + "/" + svc
		if !strings.Contains(out, needle) {
			t.Errorf("expected output to contain %q\n--- stdout ---\n%s", needle, out)
		}
	}

	// The Total line must be present and non-zero.
	if !strings.Contains(out, "Total:") {
		t.Fatalf("expected output to contain a Total line\n--- stdout ---\n%s", out)
	}
	totalLine := extractLineContaining(out, "Total:")
	if strings.Contains(totalLine, "Total: 0 ") {
		t.Errorf("expected non-zero object count, got: %s", totalLine)
	}
}

func TestDescribe_StdinInput(t *testing.T) {
	cluster := describeBuildCluster(t)
	data, err := yaml.Marshal(cluster)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	out, err := describeRun(t, string(data), describeFlagShort, describeStdinSentinel)
	if err != nil {
		t.Fatalf("DescribeCmd from stdin returned error: %v\n--- stdout ---\n%s", err, out)
	}
	mustContain(t, out, "=== Cluster: "+describeTestClusterName+" ===")
	mustContain(t, out, "Total:")
}

func TestDescribe_MissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	_, err := describeRun(t, "", describeFlagFile, missing)
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("expected error to mention %q, got: %v", missing, err)
	}
}

func TestDescribe_InvalidYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), describeFixtureFile)
	// `:` after a key with no value followed by another key produces a YAML
	// parse error that the unmarshaller surfaces.
	if err := os.WriteFile(path, []byte("this: is: not: valid: yaml: ::: ::\n"), 0o600); err != nil {
		t.Fatalf("writing bad fixture: %v", err)
	}
	_, err := describeRun(t, "", describeFlagFile, path)
	if err == nil {
		t.Fatal("expected parse error for malformed YAML, got nil")
	}
}

func TestDescribe_ShowDetails(t *testing.T) {
	cluster := describeBuildCluster(t)
	path := describeWriteFixture(t, cluster)

	out, err := describeRun(t, "",
		describeFlagFile, path,
		describeFlagShowDetails,
	)
	if err != nil {
		t.Fatalf("DescribeCmd --show-details returned error: %v\n--- stdout ---\n%s", err, out)
	}

	// Env vars are rendered. SMD_DBHOST is set unconditionally on the SMD
	// container — see internal/reconcilers/smd.go.
	mustContain(t, out, "SMD_DBHOST")

	// Secret references render as "secretName/key". The SMD password env var
	// is sourced from a secret keyed by VaultKeySMDPassword ("SMD_DB_PASSWORD").
	if !strings.Contains(out, "SMD_DB_PASSWORD") {
		t.Errorf("expected --show-details output to contain at least one secretRef key\n--- stdout ---\n%s", out)
	}
}

func TestDescribe_DisabledServiceOmitted(t *testing.T) {
	// CoreDHCP is one of the reconcilers whose Describe() returns an empty
	// slice when the service is disabled (see internal/reconcilers/coredhcp.go).
	// The base fixture leaves CoreDHCP disabled, so its section should render
	// as the empty marker — and crucially must contain no DaemonSet line.
	cluster := describeBuildCluster(t)
	path := describeWriteFixture(t, cluster)

	out, err := describeRun(t, "", describeFlagFile, path)
	if err != nil {
		t.Fatalf("DescribeCmd returned error: %v\n--- stdout ---\n%s", err, out)
	}

	dhcpSection := extractSection(out, "== CoreDHCP ==")
	if dhcpSection == "" {
		t.Fatalf("CoreDHCP section absent from output:\n%s", out)
	}
	if strings.Contains(dhcpSection, "DaemonSet/") {
		t.Errorf("expected CoreDHCP section to omit DaemonSet when disabled, got:\n%s", dhcpSection)
	}

	// Other (enabled) services should still be present in their own sections.
	mustContain(t, out, "Deployment/"+describeTestNamespace+"/tokensmith")
}

// mustContain fails the test when needle is absent from haystack.
func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("expected output to contain %q\n--- output ---\n%s", needle, haystack)
	}
}

// extractLineContaining returns the first line in s that contains needle, or
// "" when none match. Used for sub-string assertions on a single output line.
func extractLineContaining(s, needle string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

// extractSection returns the lines starting at header (inclusive) up to but
// not including the next "== " header or the Total: line. Returns "" when
// header is absent.
func extractSection(s, header string) string {
	idx := strings.Index(s, header)
	if idx < 0 {
		return ""
	}
	rest := s[idx:]
	// Find the next section header or the Total marker.
	endIdx := len(rest)
	if i := strings.Index(rest[len(header):], "== "); i >= 0 {
		endIdx = len(header) + i
	}
	if i := strings.Index(rest[len(header):], "Total:"); i >= 0 && len(header)+i < endIdx {
		endIdx = len(header) + i
	}
	return rest[:endIdx]
}
