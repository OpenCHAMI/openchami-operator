// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package admin

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

const (
	backupTestClusterName = "venado"
	backupTestNamespace   = "default"
	backupTestVaultAddr   = "https://vault.example.com:8200"
	backupTestVaultToken  = "s.testtoken"
	backupTestS3Endpoint  = "http://versitygw.example.com:10000"
	backupTestS3Bucket    = "venado-backups"
)

// backupTestScheme builds a scheme that knows about the openchami types and
// the CNPG types — both are needed by the backup runner.
func backupTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("registering client-go scheme: %v", err)
	}
	if err := openchamiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering openchami scheme: %v", err)
	}
	if err := cnpgv1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering cnpg scheme: %v", err)
	}
	return scheme
}

// backupTestCluster returns an OpenCHAMICluster CR usable as input to the
// backup runner. ClusterName and metadata.name are aligned so the canonical
// per-cluster namespace ("openchami-{clusterName}") is well-defined.
func backupTestCluster(name, ns string) *openchamiv1alpha1.OpenCHAMICluster {
	return &openchamiv1alpha1.OpenCHAMICluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: openchamiv1alpha1.OpenCHAMIClusterSpec{
			ClusterName: name,
			Domain:      name + ".test.local",
		},
	}
}

// backupTestRunner constructs a runner with the canonical happy-path config.
// Tests can mutate fields after the fact (outputDir, dryRun, etc.) before
// calling run().
func backupTestRunner(t *testing.T, stdout, stderr *bytes.Buffer) *backupRunner {
	t.Helper()
	o := &backupOpts{
		clusterName:  backupTestClusterName,
		namespace:    backupTestNamespace,
		vaultAddr:    backupTestVaultAddr,
		vaultToken:   backupTestVaultToken,
		s3Endpoint:   backupTestS3Endpoint,
		s3Bucket:     backupTestS3Bucket,
		outputPrefix: t.TempDir(),
	}
	r, err := o.toRunner(stdout, stderr)
	if err != nil {
		t.Fatalf("toRunner: %v", err)
	}
	return r
}

func TestBackup_DryRunPrintsPlan(t *testing.T) {
	var stdout, stderr bytes.Buffer
	r := backupTestRunner(t, &stdout, &stderr)
	r.dryRun = true

	if err := r.runDryRun(context.Background()); err != nil {
		t.Fatalf("runDryRun: %v", err)
	}

	out := stdout.String()
	for _, want := range []string{
		"DRY RUN",
		backupCNPGHeader,
		backupTokensmithHeader,
		backupVaultRaftHeader,
		backupCRYAMLHeader,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run stdout missing %q\n--- stdout ---\n%s", want, out)
		}
	}

	// Dry run must not have created the output directory contents.
	yamlPath := filepath.Join(r.outputDir, backupClusterYAMLFile)
	if _, err := os.Stat(yamlPath); !os.IsNotExist(err) {
		t.Errorf("dry-run wrote %s; expected no filesystem mutation", yamlPath)
	}
}

func TestBackup_HappyPath(t *testing.T) {
	scheme := backupTestScheme(t)
	cluster := backupTestCluster(backupTestClusterName, backupTestNamespace)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	var stdout, stderr bytes.Buffer
	r := backupTestRunner(t, &stdout, &stderr)

	if err := r.run(context.Background(), c); err != nil {
		t.Fatalf("run: %v", err)
	}

	// (a) cluster.yaml was written and parses back to an OpenCHAMICluster
	//     with the correct ClusterName.
	yamlPath := filepath.Join(r.outputDir, backupClusterYAMLFile)
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("reading written cluster YAML: %v", err)
	}
	var roundTrip openchamiv1alpha1.OpenCHAMICluster
	if err := yaml.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("parsing written cluster YAML: %v\n--- yaml ---\n%s", err, string(data))
	}
	if roundTrip.Spec.ClusterName != backupTestClusterName {
		t.Errorf("ClusterName in written YAML = %q, want %q",
			roundTrip.Spec.ClusterName, backupTestClusterName)
	}

	// (b) a CNPG Backup CR exists in the cluster namespace targeting the
	//     conventional CNPG cluster name.
	wantNS := "openchami-" + backupTestClusterName
	wantCNPGCluster := "openchami-" + backupTestClusterName + "-postgres"

	var list cnpgv1.BackupList
	if err := c.List(context.Background(), &list, client.InNamespace(wantNS)); err != nil {
		t.Fatalf("listing cnpg backups in %s: %v", wantNS, err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected exactly 1 cnpg Backup in %s, got %d", wantNS, len(list.Items))
	}
	got := list.Items[0]
	if got.Spec.Cluster.Name != wantCNPGCluster {
		t.Errorf("cnpg Backup targets cluster %q, want %q", got.Spec.Cluster.Name, wantCNPGCluster)
	}
	if !strings.HasPrefix(got.Name, backupTestClusterName+"-backup-") {
		t.Errorf("cnpg Backup name %q does not match %q-backup-* convention",
			got.Name, backupTestClusterName)
	}
	if got.Labels["openchami.org/cluster"] != backupTestClusterName {
		t.Errorf("cnpg Backup label openchami.org/cluster = %q, want %q",
			got.Labels["openchami.org/cluster"], backupTestClusterName)
	}

	// Stdout summary must reference both automated artifacts.
	out := stdout.String()
	if !strings.Contains(out, "ochami-admin backup complete") {
		t.Errorf("stdout missing completion summary header\n--- stdout ---\n%s", out)
	}
	if !strings.Contains(out, yamlPath) {
		t.Errorf("stdout summary does not reference written YAML path %q\n--- stdout ---\n%s", yamlPath, out)
	}
}

func TestBackup_ClusterNotFound(t *testing.T) {
	scheme := backupTestScheme(t)
	// No OpenCHAMICluster pre-created.
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	var stdout, stderr bytes.Buffer
	r := backupTestRunner(t, &stdout, &stderr)

	err := r.run(context.Background(), c)
	if err == nil {
		t.Fatal("expected error when cluster does not exist, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q does not mention not-found", err.Error())
	}
	if !strings.Contains(err.Error(), backupTestClusterName) {
		t.Errorf("error %q does not mention cluster name %q", err.Error(), backupTestClusterName)
	}
}

func TestBackup_ManualStepsEmittedToStderr(t *testing.T) {
	scheme := backupTestScheme(t)
	cluster := backupTestCluster(backupTestClusterName, backupTestNamespace)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	var stdout, stderr bytes.Buffer
	r := backupTestRunner(t, &stdout, &stderr)

	if err := r.run(context.Background(), c); err != nil {
		t.Fatalf("run: %v", err)
	}

	errOut := stderr.String()
	if !strings.Contains(errOut, backupTokensmithHeader) {
		t.Errorf("stderr missing tokensmith manual step header\n--- stderr ---\n%s", errOut)
	}
	if !strings.Contains(errOut, backupVaultRaftHeader) {
		t.Errorf("stderr missing vault raft snapshot manual step header\n--- stderr ---\n%s", errOut)
	}
	// Sanity: the printed Vault command should include the supplied flag
	// values verbatim so an admin can copy-paste it.
	if !strings.Contains(errOut, "vault operator raft snapshot save") {
		t.Errorf("stderr missing vault raft snapshot command\n--- stderr ---\n%s", errOut)
	}
	if !strings.Contains(errOut, backupTestVaultAddr) {
		t.Errorf("stderr missing VAULT_ADDR=%s\n--- stderr ---\n%s", backupTestVaultAddr, errOut)
	}
	if !strings.Contains(errOut, backupTestVaultToken) {
		t.Errorf("stderr missing supplied vault token in raft snapshot command\n--- stderr ---\n%s", errOut)
	}
}

func TestBackup_RequiredFlags(t *testing.T) {
	cases := []struct {
		name string
		opts *backupOpts
		want string
	}{
		{
			name: "missing cluster-name",
			opts: &backupOpts{
				vaultAddr:  backupTestVaultAddr,
				vaultToken: backupTestVaultToken,
				s3Endpoint: backupTestS3Endpoint,
				s3Bucket:   backupTestS3Bucket,
			},
			want: "--cluster-name",
		},
		{
			name: "missing vault-addr",
			opts: &backupOpts{
				clusterName: backupTestClusterName,
				vaultToken:  backupTestVaultToken,
				s3Endpoint:  backupTestS3Endpoint,
				s3Bucket:    backupTestS3Bucket,
			},
			want: "--vault-addr",
		},
		{
			name: "missing vault-token",
			opts: &backupOpts{
				clusterName: backupTestClusterName,
				vaultAddr:   backupTestVaultAddr,
				s3Endpoint:  backupTestS3Endpoint,
				s3Bucket:    backupTestS3Bucket,
			},
			want: "--vault-token",
		},
		{
			name: "missing s3-endpoint",
			opts: &backupOpts{
				clusterName: backupTestClusterName,
				vaultAddr:   backupTestVaultAddr,
				vaultToken:  backupTestVaultToken,
				s3Bucket:    backupTestS3Bucket,
			},
			want: "--s3-endpoint",
		},
		{
			name: "missing s3-bucket",
			opts: &backupOpts{
				clusterName: backupTestClusterName,
				vaultAddr:   backupTestVaultAddr,
				vaultToken:  backupTestVaultToken,
				s3Endpoint:  backupTestS3Endpoint,
			},
			want: "--s3-bucket",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.opts.toRunner(&bytes.Buffer{}, &bytes.Buffer{})
			if err == nil {
				t.Fatalf("expected error mentioning %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

// Compile-time guard: backupClientForKubeconfig must return a controller-
// runtime client. We don't exercise it (no live cluster) but referencing
// the symbol catches signature drift.
var _ = func() (client.Client, error) { return backupClientForKubeconfig("") }
