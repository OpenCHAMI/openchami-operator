// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package admin

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/yaml"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

// backup.go-scoped constants. Prefixed with `backup` to avoid collision with
// peer files in this package written by parallel sub-agents.
const (
	backupDefaultNamespace = "default"
	backupClusterYAMLFile  = "cluster.yaml"

	backupCNPGAPIVersion = "postgresql.cnpg.io/v1"
	backupCNPGKindBackup = "Backup"

	backupCNPGClusterSuffix = "-postgres"
	backupCNPGNamePrefix    = "openchami-"

	backupTokensmithHeader = "=== Step 2: Tokensmith PVC snapshot (MANUAL) ==="
	backupVaultRaftHeader  = "=== Step 3: Vault raft snapshot (MANUAL) ==="
	backupCNPGHeader       = "=== Step 1: CNPG base backup ==="
	backupCRYAMLHeader     = "=== Step 4: OpenCHAMIControlPlane CR YAML ==="

	backupTimestampFormat = "20060102-150405"
)

// backupOpts holds every flag value for the `backup` sub-command. Kept
// unexported so it cannot collide with peer files.
type backupOpts struct {
	clusterName string
	namespace   string
	kubeconfig  string

	vaultAddr  string
	vaultToken string

	s3Endpoint string
	s3Bucket   string

	outputPrefix string
	dryRun       bool
}

// BackupCmd returns the `ochami-admin backup` command. The command snapshots
// a cluster's infrastructure state. Steps 1 (CNPG Backup CR) and 4 (CR YAML
// dump) are automated; steps 2 (Tokensmith PVC) and 3 (Vault raft) are
// emitted as copy-paste-ready manual instructions on stderr.
func BackupCmd() *cobra.Command {
	o := &backupOpts{}
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Snapshot cluster infrastructure state to object storage",
		Long: "Snapshot cluster infrastructure state. Automates the CNPG base " +
			"backup CR and OpenCHAMIControlPlane CR YAML dump; emits manual " +
			"copy-paste-ready commands on stderr for the Tokensmith PVC " +
			"snapshot and Vault raft snapshot steps.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			runner, err := o.toRunner(cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if runner.dryRun {
				return runner.runDryRun(ctx)
			}
			c, err := backupClientForKubeconfig(runner.kubeconfig)
			if err != nil {
				return err
			}
			return runner.run(ctx, c)
		},
	}
	o.bindFlags(cmd.Flags())
	return cmd
}

// bindFlags wires flag values into o.
func (o *backupOpts) bindFlags(f *pflag.FlagSet) {
	f.StringVar(&o.clusterName, "cluster-name", "", "OpenCHAMIControlPlane name (required)")
	f.StringVar(&o.namespace, "namespace", backupDefaultNamespace, "Namespace where the OpenCHAMIControlPlane CR lives")
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "Path to kubeconfig (default: standard kubeconfig discovery)")

	f.StringVar(&o.vaultAddr, "vault-addr", "", "Vault address (required; used in the printed manual raft snapshot command)")
	f.StringVar(&o.vaultToken, "vault-token", "", "Vault token with raft snapshot capability (required; used in the printed manual raft snapshot command)")

	f.StringVar(&o.s3Endpoint, "s3-endpoint", "", "Backup target S3 endpoint URL (required; used in the printed manual instructions)")
	f.StringVar(&o.s3Bucket, "s3-bucket", "", "Backup target S3 bucket name (required; used in the printed manual instructions)")

	f.StringVar(&o.outputPrefix, "output-prefix", "", "Local directory prefix for artifacts written by this command (default: backup/{clusterName}/{RFC3339}/). Treated as a local directory; admin uploads to S3 separately.")
	f.BoolVar(&o.dryRun, "dry-run", false, "Print planned actions without creating any resources or files")
}

// backupRunner is the testable form of backupOpts: validated, with writers
// and the resolved output directory bound, ready to execute.
type backupRunner struct {
	clusterName string
	namespace   string
	kubeconfig  string

	vaultAddr  string
	vaultToken string

	s3Endpoint string
	s3Bucket   string

	outputDir string
	dryRun    bool

	stdout io.Writer
	stderr io.Writer
}

// toRunner validates flags and produces a runner. Defaulting of the output
// prefix happens here so the timestamp is captured once per invocation.
func (o *backupOpts) toRunner(stdout, stderr io.Writer) (*backupRunner, error) {
	if o.clusterName == "" {
		return nil, fmt.Errorf("--cluster-name is required")
	}
	if o.vaultAddr == "" {
		return nil, fmt.Errorf("--vault-addr is required")
	}
	if o.vaultToken == "" {
		return nil, fmt.Errorf("--vault-token is required")
	}
	if o.s3Endpoint == "" {
		return nil, fmt.Errorf("--s3-endpoint is required")
	}
	if o.s3Bucket == "" {
		return nil, fmt.Errorf("--s3-bucket is required")
	}
	ns := o.namespace
	if ns == "" {
		ns = backupDefaultNamespace
	}
	outputDir := o.outputPrefix
	if outputDir == "" {
		outputDir = filepath.Join("backup", o.clusterName, time.Now().UTC().Format(time.RFC3339))
	}
	return &backupRunner{
		clusterName: o.clusterName,
		namespace:   ns,
		kubeconfig:  o.kubeconfig,
		vaultAddr:   o.vaultAddr,
		vaultToken:  o.vaultToken,
		s3Endpoint:  o.s3Endpoint,
		s3Bucket:    o.s3Bucket,
		outputDir:   outputDir,
		dryRun:      o.dryRun,
		stdout:      stdout,
		stderr:      stderr,
	}, nil
}

// run executes the backup steps against the supplied client.
func (r *backupRunner) run(ctx context.Context, c client.Client) error {
	cp, err := r.fetchCluster(ctx, c)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(r.outputDir, 0o755); err != nil {
		return fmt.Errorf("creating output directory %s: %w", r.outputDir, err)
	}

	yamlPath, err := r.writeClusterYAML(cp)
	if err != nil {
		return err
	}

	cnpgBackup, err := r.createCNPGBackup(ctx, c)
	if err != nil {
		return err
	}

	r.printManualSteps(cp)
	r.printSummary(yamlPath, cnpgBackup)
	return nil
}

// runDryRun prints the planned actions without contacting any API or
// touching the local filesystem.
func (r *backupRunner) runDryRun(_ context.Context) error {
	clusterNS := backupCNPGNamePrefix + r.clusterName
	cnpgClusterName := backupCNPGNamePrefix + r.clusterName + backupCNPGClusterSuffix
	backupName := r.clusterName + "-backup-" + time.Now().UTC().Format(backupTimestampFormat)

	_, _ = fmt.Fprintln(r.stdout, "DRY RUN: ochami-admin backup planned actions")
	_, _ = fmt.Fprintln(r.stdout, "  cluster:        "+r.clusterName)
	_, _ = fmt.Fprintln(r.stdout, "  cluster ns:     "+clusterNS)
	_, _ = fmt.Fprintln(r.stdout, "  CR namespace:   "+r.namespace)
	_, _ = fmt.Fprintln(r.stdout, "  output dir:     "+r.outputDir)
	_, _ = fmt.Fprintln(r.stdout, "")

	_, _ = fmt.Fprintln(r.stdout, backupCNPGHeader)
	_, _ = fmt.Fprintln(r.stdout, "  Would create cnpg Backup "+backupName+" in namespace "+clusterNS)
	_, _ = fmt.Fprintln(r.stdout, "  Targeting CNPG cluster: "+cnpgClusterName)
	_, _ = fmt.Fprintln(r.stdout, "  (CNPG controller performs the actual S3 upload via cluster.spec.backup configuration.)")
	_, _ = fmt.Fprintln(r.stdout, "")

	_, _ = fmt.Fprintln(r.stdout, backupTokensmithHeader)
	_, _ = fmt.Fprintln(r.stdout, "  See stderr for copy-paste-ready commands.")
	_, _ = fmt.Fprintln(r.stdout, "")

	_, _ = fmt.Fprintln(r.stdout, backupVaultRaftHeader)
	_, _ = fmt.Fprintln(r.stdout, "  See stderr for copy-paste-ready commands.")
	_, _ = fmt.Fprintln(r.stdout, "")

	_, _ = fmt.Fprintln(r.stdout, backupCRYAMLHeader)
	_, _ = fmt.Fprintln(r.stdout, "  Would write OpenCHAMIControlPlane YAML to "+filepath.Join(r.outputDir, backupClusterYAMLFile))

	r.printManualSteps(nil)
	return nil
}

// fetchCluster loads the OpenCHAMIControlPlane from --namespace.
func (r *backupRunner) fetchCluster(ctx context.Context, c client.Client) (*openchamiv1alpha1.OpenCHAMIControlPlane, error) {
	cp := &openchamiv1alpha1.OpenCHAMIControlPlane{}
	key := types.NamespacedName{Namespace: r.namespace, Name: r.clusterName}
	if err := c.Get(ctx, key, cp); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("OpenCHAMIControlPlane %q not found in namespace %q", r.clusterName, r.namespace)
		}
		return nil, fmt.Errorf("getting OpenCHAMIControlPlane %s/%s: %w", r.namespace, r.clusterName, err)
	}
	return cp, nil
}

// writeClusterYAML marshals the cluster to YAML and writes it to
// <outputDir>/cluster.yaml. Returns the path written.
func (r *backupRunner) writeClusterYAML(cp *openchamiv1alpha1.OpenCHAMIControlPlane) (string, error) {
	// Strip the managedFields and resourceVersion noise — the result should
	// be re-applyable by `kubectl apply -f`.
	cp.ManagedFields = nil
	cp.ResourceVersion = ""
	cp.UID = ""

	out, err := yaml.Marshal(cp)
	if err != nil {
		return "", fmt.Errorf("marshalling cluster YAML: %w", err)
	}
	path := filepath.Join(r.outputDir, backupClusterYAMLFile)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return path, nil
}

// createCNPGBackup creates a cnpgv1.Backup CR in the cluster's namespace
// targeting the conventional CNPG cluster. Returns the created Backup so
// the summary can reference its name.
func (r *backupRunner) createCNPGBackup(ctx context.Context, c client.Client) (*cnpgv1.Backup, error) {
	clusterNS := backupCNPGNamePrefix + r.clusterName
	cnpgClusterName := backupCNPGNamePrefix + r.clusterName + backupCNPGClusterSuffix
	backupName := r.clusterName + "-backup-" + time.Now().UTC().Format(backupTimestampFormat)

	bk := &cnpgv1.Backup{
		TypeMeta: metav1.TypeMeta{
			APIVersion: backupCNPGAPIVersion,
			Kind:       backupCNPGKindBackup,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      backupName,
			Namespace: clusterNS,
			Labels: map[string]string{
				"openchami.org/cluster":   r.clusterName,
				"openchami.org/triggered": "ochami-admin-backup",
			},
		},
		Spec: cnpgv1.BackupSpec{
			Cluster: cnpgv1.LocalObjectReference{Name: cnpgClusterName},
			Method:  cnpgv1.BackupMethodBarmanObjectStore,
		},
	}
	if err := c.Create(ctx, bk); err != nil {
		return nil, fmt.Errorf("creating cnpg Backup %s/%s: %w", clusterNS, backupName, err)
	}
	return bk, nil
}

// printManualSteps writes copy-paste-ready commands for the steps that this
// implementation does not automate. cluster may be nil (dry-run case) — the
// cluster argument is only consulted for fields that are not yet known from
// flags. Today we use only flag-derived state, so it is currently advisory.
func (r *backupRunner) printManualSteps(_ *openchamiv1alpha1.OpenCHAMIControlPlane) {
	clusterNS := backupCNPGNamePrefix + r.clusterName
	tokensmithPVC := backupCNPGNamePrefix + r.clusterName + "-tokensmith-keys"
	raftSnapshotPath := filepath.Join(r.outputDir, "vault-raft.snap")
	pvcSnapshotPath := filepath.Join(r.outputDir, "tokensmith-keys.tar.gz")

	_, _ = fmt.Fprintln(r.stderr, "")
	_, _ = fmt.Fprintln(r.stderr, backupTokensmithHeader)
	_, _ = fmt.Fprintln(r.stderr, "# Preferred (storage class supports CSI snapshots):")
	_, _ = fmt.Fprintf(r.stderr, "kubectl apply -n %s -f - <<EOF\n", clusterNS)
	_, _ = fmt.Fprintln(r.stderr, "apiVersion: snapshot.storage.k8s.io/v1")
	_, _ = fmt.Fprintln(r.stderr, "kind: VolumeSnapshot")
	_, _ = fmt.Fprintln(r.stderr, "metadata:")
	_, _ = fmt.Fprintf(r.stderr, "  name: %s-snap-%s\n", tokensmithPVC, time.Now().UTC().Format(backupTimestampFormat))
	_, _ = fmt.Fprintln(r.stderr, "spec:")
	_, _ = fmt.Fprintln(r.stderr, "  source:")
	_, _ = fmt.Fprintf(r.stderr, "    persistentVolumeClaimName: %s\n", tokensmithPVC)
	_, _ = fmt.Fprintln(r.stderr, "EOF")
	_, _ = fmt.Fprintln(r.stderr, "")
	_, _ = fmt.Fprintln(r.stderr, "# Fallback (no CSI snapshots — copy the keys directory out of the running pod):")
	_, _ = fmt.Fprintf(r.stderr, "kubectl -n %s exec deploy/tokensmith -- tar czf - /var/lib/tokensmith/keys > %s\n", clusterNS, pvcSnapshotPath)
	_, _ = fmt.Fprintln(r.stderr, "")

	_, _ = fmt.Fprintln(r.stderr, backupVaultRaftHeader)
	_, _ = fmt.Fprintln(r.stderr, "# Run from a host that can reach the Vault API. Requires a token with sys/storage/raft/snapshot capability.")
	_, _ = fmt.Fprintf(r.stderr, "VAULT_ADDR=%s VAULT_TOKEN=%s vault operator raft snapshot save %s\n", r.vaultAddr, r.vaultToken, raftSnapshotPath)
	_, _ = fmt.Fprintln(r.stderr, "")
	_, _ = fmt.Fprintln(r.stderr, "# Then upload all artifacts in the output directory to your backup bucket:")
	_, _ = fmt.Fprintf(r.stderr, "aws --endpoint-url %s s3 cp %s/ s3://%s/%s/ --recursive\n", r.s3Endpoint, r.outputDir, r.s3Bucket, r.clusterName)
	_, _ = fmt.Fprintln(r.stderr, "")
}

// printSummary writes the final stdout summary describing what was done.
func (r *backupRunner) printSummary(yamlPath string, bk *cnpgv1.Backup) {
	_, _ = fmt.Fprintln(r.stdout, "ochami-admin backup complete")
	_, _ = fmt.Fprintln(r.stdout, "  cluster:           "+r.clusterName)
	_, _ = fmt.Fprintln(r.stdout, "  output directory:  "+r.outputDir)
	_, _ = fmt.Fprintln(r.stdout, "")
	_, _ = fmt.Fprintln(r.stdout, "Automated:")
	_, _ = fmt.Fprintf(r.stdout, "  - %s -> Backup %s/%s (CNPG controller is performing the upload)\n",
		backupCNPGHeader, bk.Namespace, bk.Name)
	_, _ = fmt.Fprintf(r.stdout, "  - %s -> %s\n", backupCRYAMLHeader, yamlPath)
	_, _ = fmt.Fprintln(r.stdout, "")
	_, _ = fmt.Fprintln(r.stdout, "Pending (manual; commands printed on stderr):")
	_, _ = fmt.Fprintln(r.stdout, "  - "+backupTokensmithHeader)
	_, _ = fmt.Fprintln(r.stdout, "  - "+backupVaultRaftHeader)
	_, _ = fmt.Fprintln(r.stdout, "")
	_, _ = fmt.Fprintln(r.stdout, "After completing the manual steps, upload the entire output directory to your backup bucket.")
}

// backupClientForKubeconfig builds a controller-runtime client using the
// supplied kubeconfig path (or standard discovery if empty). The returned
// client knows about openchami types and CNPG types so the backup steps
// can address them by typed object.
func backupClientForKubeconfig(kubeconfig string) (client.Client, error) {
	if kubeconfig != "" {
		// controller-runtime/config honours $KUBECONFIG; setting the env
		// var keeps the rest of the discovery chain (in-cluster etc.) the
		// same as the no-flag case.
		if err := os.Setenv("KUBECONFIG", kubeconfig); err != nil {
			return nil, fmt.Errorf("setting KUBECONFIG: %w", err)
		}
	}
	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering client-go scheme: %w", err)
	}
	if err := openchamiv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering openchami scheme: %w", err)
	}
	if err := cnpgv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering cnpg scheme: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("constructing kubernetes client: %w", err)
	}
	return c, nil
}
