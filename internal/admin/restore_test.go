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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

// Shared literals used across the restore test cases. Extracted to satisfy
// goconst and to avoid colliding with peer files in the same package.
const (
	restoreTestClusterFoo  = "foo"
	restoreTestClusterBar  = "bar"
	restoreTestNamespaceNS = "default"
)

// restoreNewScheme returns a runtime scheme with the types this package needs
// for fake-client tests.
func restoreNewScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding clientgo scheme: %v", err)
	}
	if err := openchamiv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding openchami scheme: %v", err)
	}
	return s
}

// restoreWriteBackupFile writes a minimal cluster.yaml under a fresh temp
// directory and returns the directory path. The cluster object is built with
// the supplied name.
func restoreWriteBackupFile(t *testing.T, clusterName string) string {
	t.Helper()
	dir := t.TempDir()
	yamlBytes := []byte("" +
		"apiVersion: openchami.openchami.org/v1alpha1\n" +
		"kind: OpenCHAMIControlPlane\n" +
		"metadata:\n" +
		"  name: " + clusterName + "\n" +
		"  namespace: default\n" +
		"spec:\n" +
		"  clusterName: " + clusterName + "\n" +
		"  domain: " + clusterName + ".test.local\n" +
		"  platform:\n" +
		"    vault:\n" +
		"      address: https://vault.example.com:8200\n" +
		"      authMethod: kubernetes\n" +
		"    objectStorage:\n" +
		"      endpoint: http://s3.example.com:9000\n",
	)
	if err := os.WriteFile(filepath.Join(dir, restoreClusterFile), yamlBytes, 0o644); err != nil {
		t.Fatalf("writing backup file: %v", err)
	}
	return dir
}

// restoreWriteEmptyBackup writes a cluster.yaml with an empty spec.clusterName
// to exercise the validation branch in restoreLoadBackup. Returns the dir.
func restoreWriteEmptyBackup(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	yamlBytes := []byte("" +
		"apiVersion: openchami.openchami.org/v1alpha1\n" +
		"kind: OpenCHAMIControlPlane\n" +
		"metadata:\n" +
		"  name: orphan\n" +
		"spec:\n" +
		"  domain: orphan.test.local\n",
	)
	if err := os.WriteFile(filepath.Join(dir, restoreClusterFile), yamlBytes, 0o644); err != nil {
		t.Fatalf("writing backup file: %v", err)
	}
	return dir
}

// restoreNewRunner builds a restoreRunner with capture buffers and the
// supplied opts.
func restoreNewRunner(opts *restoreOpts) (*restoreRunner, *bytes.Buffer, *bytes.Buffer) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	return &restoreRunner{opts: opts, stdout: stdout, stderr: stderr}, stdout, stderr
}

func TestRestore_DryRunPrintsCRYAML(t *testing.T) {
	dir := restoreWriteBackupFile(t, restoreTestClusterFoo)
	scheme := restoreNewScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	r, stdout, _ := restoreNewRunner(&restoreOpts{
		clusterName: restoreTestClusterFoo,
		namespace:   restoreTestNamespaceNS,
		inputPrefix: dir,
		dryRun:      true,
	})

	if err := r.run(context.Background(), c); err != nil {
		t.Fatalf("dry-run restore failed: %v", err)
	}

	got := stdout.String()
	if !strings.Contains(got, "clusterName: "+restoreTestClusterFoo) {
		t.Errorf("dry-run stdout should contain the cluster YAML, got:\n%s", got)
	}
	if !strings.Contains(got, "kind: OpenCHAMIControlPlane") {
		t.Errorf("dry-run stdout should contain kind: OpenCHAMIControlPlane, got:\n%s", got)
	}

	// Dry-run must not have created the CR in the fake client.
	out := &openchamiv1alpha1.OpenCHAMIControlPlane{}
	err := c.Get(context.Background(),
		types.NamespacedName{Name: restoreTestClusterFoo, Namespace: restoreTestNamespaceNS}, out)
	if err == nil {
		t.Error("expected dry-run to leave the cluster un-created in the fake client")
	}
}

func TestRestore_PreFlightRejectsExistingNamespaceWithoutForce(t *testing.T) {
	dir := restoreWriteBackupFile(t, restoreTestClusterFoo)
	scheme := restoreNewScheme(t)

	existing := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "openchami-" + restoreTestClusterFoo},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()

	r, _, _ := restoreNewRunner(&restoreOpts{
		clusterName: restoreTestClusterFoo,
		namespace:   restoreTestNamespaceNS,
		inputPrefix: dir,
	})

	err := r.run(context.Background(), c)
	if err == nil {
		t.Fatal("expected restore to fail when target namespace exists and --force is unset")
	}
	msg := err.Error()
	if !strings.Contains(msg, "exists") {
		t.Errorf("error should mention namespace exists, got: %v", err)
	}
	if !strings.Contains(msg, "--force") {
		t.Errorf("error should mention --force, got: %v", err)
	}
}

func TestRestore_ForceProceedsWhenNamespaceExists(t *testing.T) {
	dir := restoreWriteBackupFile(t, restoreTestClusterFoo)
	scheme := restoreNewScheme(t)

	existing := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "openchami-" + restoreTestClusterFoo},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()

	r, _, stderr := restoreNewRunner(&restoreOpts{
		clusterName: restoreTestClusterFoo,
		namespace:   restoreTestNamespaceNS,
		inputPrefix: dir,
		force:       true,
	})

	if err := r.run(context.Background(), c); err != nil {
		t.Fatalf("restore with --force failed: %v", err)
	}

	if !strings.Contains(stderr.String(), "warning") {
		t.Errorf("expected stderr to contain a warning about the existing namespace, got:\n%s", stderr.String())
	}

	// CR should have been server-side applied.
	out := &openchamiv1alpha1.OpenCHAMIControlPlane{}
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: restoreTestClusterFoo, Namespace: restoreTestNamespaceNS}, out); err != nil {
		t.Fatalf("expected CR to be applied with --force, get err: %v", err)
	}
	if out.Spec.ClusterName != restoreTestClusterFoo {
		t.Errorf("applied CR has ClusterName %q, want %q", out.Spec.ClusterName, restoreTestClusterFoo)
	}
}

func TestRestore_HappyPath(t *testing.T) {
	dir := restoreWriteBackupFile(t, restoreTestClusterFoo)
	scheme := restoreNewScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	r, _, _ := restoreNewRunner(&restoreOpts{
		clusterName: restoreTestClusterFoo,
		namespace:   restoreTestNamespaceNS,
		inputPrefix: dir,
	})

	if err := r.run(context.Background(), c); err != nil {
		t.Fatalf("happy-path restore failed: %v", err)
	}

	out := &openchamiv1alpha1.OpenCHAMIControlPlane{}
	if err := c.Get(context.Background(),
		types.NamespacedName{Name: restoreTestClusterFoo, Namespace: restoreTestNamespaceNS}, out); err != nil {
		t.Fatalf("expected CR to exist post-restore: %v", err)
	}
	if out.Spec.ClusterName != restoreTestClusterFoo {
		t.Errorf("ClusterName = %q, want %q", out.Spec.ClusterName, restoreTestClusterFoo)
	}
}

func TestRestore_ClusterNameMismatch(t *testing.T) {
	// Backup file written with a different cluster-name than the flag.
	dir := restoreWriteBackupFile(t, restoreTestClusterBar)
	scheme := restoreNewScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	r, _, _ := restoreNewRunner(&restoreOpts{
		clusterName: restoreTestClusterFoo, // mismatch on purpose
		namespace:   restoreTestNamespaceNS,
		inputPrefix: dir,
	})

	err := r.run(context.Background(), c)
	if err == nil {
		t.Fatal("expected an error when --cluster-name does not match backup cluster-name")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Errorf("error should mention mismatch, got: %v", err)
	}
}

func TestRestore_BackupMissingClusterName(t *testing.T) {
	dir := restoreWriteEmptyBackup(t)
	scheme := restoreNewScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	r, _, _ := restoreNewRunner(&restoreOpts{
		clusterName: restoreTestClusterFoo,
		namespace:   restoreTestNamespaceNS,
		inputPrefix: dir,
	})

	err := r.run(context.Background(), c)
	if err == nil {
		t.Fatal("expected an error when backup file is missing spec.clusterName")
	}
	if !strings.Contains(err.Error(), "missing spec.clusterName") {
		t.Errorf("error should mention missing spec.clusterName, got: %v", err)
	}
}

func TestRestore_MissingInputFile(t *testing.T) {
	scheme := restoreNewScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	r, _, _ := restoreNewRunner(&restoreOpts{
		clusterName: restoreTestClusterFoo,
		namespace:   restoreTestNamespaceNS,
		inputPrefix: "/nonexistent/path/that/does/not/exist",
	})

	err := r.run(context.Background(), c)
	if err == nil {
		t.Fatal("expected an error when --input-prefix does not contain cluster.yaml")
	}
	if !strings.Contains(err.Error(), "reading backup file") {
		t.Errorf("error should mention reading backup file, got: %v", err)
	}
}

func TestRestore_ManualStepsEmittedToStderr(t *testing.T) {
	dir := restoreWriteBackupFile(t, restoreTestClusterFoo)
	scheme := restoreNewScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	r, _, stderr := restoreNewRunner(&restoreOpts{
		clusterName: restoreTestClusterFoo,
		namespace:   restoreTestNamespaceNS,
		inputPrefix: dir,
		vaultAddr:   "https://vault.example.com:8200",
		vaultToken:  "s.fake-token",
	})

	if err := r.run(context.Background(), c); err != nil {
		t.Fatalf("restore failed: %v", err)
	}

	stderrOut := stderr.String()
	if !strings.Contains(stderrOut, "Vault raft restore") {
		t.Errorf("stderr should contain a 'Vault raft restore' section, got:\n%s", stderrOut)
	}
	if !strings.Contains(stderrOut, "PVC restore") {
		t.Errorf("stderr should contain a 'PVC restore' section, got:\n%s", stderrOut)
	}
}

// Sanity: RestoreCmd wires --cluster-name and --input-prefix as required
// (validated inside the runner). Ensures the cobra command is actually
// connected to the runner.
func TestRestoreCmd_RejectsMissingFlags(t *testing.T) {
	cmd := RestoreCmd()
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{}) // no flags
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected RestoreCmd to fail when --cluster-name and --input-prefix are missing")
	}
}
