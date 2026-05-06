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

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/yaml"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

// restore.go-scoped constants. Prefixed with `restore` to avoid collision with
// other `internal/admin` files written by parallel sub-agents.
const (
	restoreDefaultNamespace      = "default"
	restoreClusterFile           = "cluster.yaml"
	restoreNamespacePrefix       = "openchami-"
	restoreFieldOwner            = "ochami-admin"
	restoreManualHeader          = "==== MANUAL STEPS REQUIRED ===="
	restoreVaultSectionHeader    = "-- Vault raft restore --"
	restorePVCSectionHeader      = "-- PVC restore (tokensmith key material) --"
	restoreCNPGSectionHeader     = "-- CNPG database recovery --"
	restoreManualFooter          = "==== END MANUAL STEPS ===="
	restoreFlagClusterName       = "--cluster-name"
	restoreFlagForce             = "--force"
	restoreNamespaceExistsErrFmt = "namespace %q already exists; pass " + restoreFlagForce + " to overwrite"
	restoreClusterNameMismatch   = "backup cluster-name %q does not match " + restoreFlagClusterName + " %q; pass " + restoreFlagForce + " to override"
)

// restoreOpts holds flag values for the `restore` sub-command. Kept unexported
// to avoid colliding with peer files.
type restoreOpts struct {
	clusterName string
	namespace   string
	kubeconfig  string

	inputPrefix string

	vaultAddr  string
	vaultToken string

	force  bool
	dryRun bool
}

// restoreRunner couples flag values with output writers so tests can inject
// captured buffers. Mirrors the backup sub-agent's runner shape.
type restoreRunner struct {
	opts   *restoreOpts
	stdout io.Writer
	stderr io.Writer
}

// RestoreCmd returns the `ochami-admin restore` command. The command performs
// the safely-automatable steps of a restore (CR re-apply with pre-flight
// checks) and prints copy-paste instructions for the manual steps that
// remain (Vault raft snapshot, PVC restore, CNPG recovery).
func RestoreCmd() *cobra.Command {
	o := &restoreOpts{}
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Restore cluster infrastructure from a backup snapshot",
		Long: "Re-apply an OpenCHAMICluster manifest from a backup directory and " +
			"emit copy-paste-ready instructions for the manual restore steps " +
			"(Vault raft restore, PVC restore from VolumeSnapshot, CNPG recovery).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			r := &restoreRunner{
				opts:   o,
				stdout: cmd.OutOrStdout(),
				stderr: cmd.ErrOrStderr(),
			}

			if err := r.validate(); err != nil {
				return err
			}

			// Production path: build a real controller-runtime client. Tests
			// bypass this by calling r.run directly with a fake client.
			cfg, err := ctrlconfig.GetConfigWithContext("")
			if err != nil {
				return fmt.Errorf("loading kubeconfig: %w", err)
			}
			scheme := runtime.NewScheme()
			if err := clientgoscheme.AddToScheme(scheme); err != nil {
				return fmt.Errorf("registering core scheme: %w", err)
			}
			if err := openchamiv1alpha1.AddToScheme(scheme); err != nil {
				return fmt.Errorf("registering openchami scheme: %w", err)
			}
			c, err := client.New(cfg, client.Options{Scheme: scheme})
			if err != nil {
				return fmt.Errorf("constructing client: %w", err)
			}
			return r.run(cmd.Context(), c)
		},
	}
	o.bindFlags(cmd.Flags())
	return cmd
}

// bindFlags wires flag values into o. Names follow phase-14-cli.md.
func (o *restoreOpts) bindFlags(f *pflag.FlagSet) {
	f.StringVar(&o.clusterName, "cluster-name", "", "Cluster name (required)")
	f.StringVar(&o.namespace, "namespace", restoreDefaultNamespace, "Namespace where the OpenCHAMICluster CR will be re-applied")
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "Path to kubeconfig (optional; falls back to KUBECONFIG/in-cluster)")

	f.StringVar(&o.inputPrefix, "input-prefix", "", "Local directory containing cluster.yaml from `ochami-admin backup` (required)")

	f.StringVar(&o.vaultAddr, "vault-addr", "", "Vault address; emitted in the manual raft restore instructions")
	f.StringVar(&o.vaultToken, "vault-token", "", "Vault token; emitted in the manual raft restore instructions")

	f.BoolVar(&o.force, "force", false, "Proceed even when the target namespace already exists or the backup cluster-name does not match")
	f.BoolVar(&o.dryRun, "dry-run", false, "Print the planned cluster manifest to stdout without contacting Kubernetes")
}

// validate enforces the documented flag constraints. Exposed via run() so
// tests can call it directly on populated opts.
func (r *restoreRunner) validate() error {
	if r.opts.clusterName == "" {
		return fmt.Errorf("--cluster-name is required")
	}
	if r.opts.inputPrefix == "" {
		return fmt.Errorf("--input-prefix is required")
	}
	return nil
}

// run executes the restore flow against the supplied client. Tests inject a
// fake client; production constructs a real one in RunE.
func (r *restoreRunner) run(ctx context.Context, c client.Client) error {
	if err := r.validate(); err != nil {
		return err
	}

	cluster, rawYAML, err := restoreLoadBackup(r.opts.inputPrefix)
	if err != nil {
		return err
	}

	if cluster.Spec.ClusterName != r.opts.clusterName {
		if !r.opts.force {
			return fmt.Errorf(restoreClusterNameMismatch, cluster.Spec.ClusterName, r.opts.clusterName)
		}
		_, _ = fmt.Fprintf(r.stderr, "warning: backup cluster-name %q does not match --cluster-name %q; proceeding because --force is set\n",
			cluster.Spec.ClusterName, r.opts.clusterName)
	}

	// Ensure the CR lands in the requested namespace, irrespective of what
	// the YAML on disk records.
	cluster.Namespace = r.opts.namespace

	if r.opts.dryRun {
		_, _ = fmt.Fprintln(r.stderr, "dry-run: no resources will be modified.")
		if _, err := r.stdout.Write(rawYAML); err != nil {
			return fmt.Errorf("writing dry-run manifest: %w", err)
		}
		r.printManualSteps(cluster.Spec.ClusterName)
		return nil
	}

	// Pre-flight: refuse to clobber an existing per-cluster namespace unless
	// --force is set. The operator's controller will create it on first
	// reconcile if absent, so absence is the expected happy-path.
	targetNs := restoreNamespacePrefix + cluster.Spec.ClusterName
	if err := r.preflightNamespace(ctx, c, targetNs); err != nil {
		return err
	}

	// Server-side apply the CR. The operator's controller will reconcile
	// from this point forward — namespace creation, RBAC, child resources
	// all flow from a successful apply. The new client.Apply API requires
	// a generated ApplyConfiguration which we don't have for our CRD; the
	// rest of the codebase uses client.Patch+client.Apply via Patch.
	if err := c.Patch(ctx, cluster, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(restoreFieldOwner)); err != nil {
		return fmt.Errorf("server-side applying OpenCHAMICluster %q: %w",
			cluster.Spec.ClusterName, err)
	}

	r.printManualSteps(cluster.Spec.ClusterName)
	r.printSummary(cluster.Spec.ClusterName, targetNs)
	return nil
}

// preflightNamespace returns nil if the target namespace is absent or --force
// is set; otherwise an instructive error.
func (r *restoreRunner) preflightNamespace(ctx context.Context, c client.Client, ns string) error {
	got := &corev1.Namespace{}
	err := c.Get(ctx, types.NamespacedName{Name: ns}, got)
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("checking namespace %q: %w", ns, err)
	}
	if !r.opts.force {
		return fmt.Errorf(restoreNamespaceExistsErrFmt, ns)
	}
	_, _ = fmt.Fprintf(r.stderr, "warning: namespace %q already exists; proceeding because --force is set\n", ns)
	return nil
}

// printManualSteps writes copy-paste instructions for the steps a human must
// perform out-of-band to complete a restore.
func (r *restoreRunner) printManualSteps(clusterName string) {
	addr := r.opts.vaultAddr
	if addr == "" {
		addr = "<set --vault-addr or VAULT_ADDR>"
	}
	token := r.opts.vaultToken
	if token == "" {
		token = "<set --vault-token or VAULT_TOKEN>"
	}
	prefix := r.opts.inputPrefix

	_, _ = fmt.Fprintln(r.stderr)
	_, _ = fmt.Fprintln(r.stderr, restoreManualHeader)

	_, _ = fmt.Fprintln(r.stderr, restoreVaultSectionHeader)
	_, _ = fmt.Fprintln(r.stderr, "Vault is external to this operator and must be restored by an administrator.")
	_, _ = fmt.Fprintln(r.stderr, "Run on a host with the vault CLI installed:")
	_, _ = fmt.Fprintf(r.stderr, "  export VAULT_ADDR=%s\n", addr)
	_, _ = fmt.Fprintf(r.stderr, "  export VAULT_TOKEN=%s\n", token)
	_, _ = fmt.Fprintf(r.stderr, "  vault operator raft snapshot restore %s\n",
		filepath.Join(prefix, "vault-raft.snap"))
	_, _ = fmt.Fprintln(r.stderr)

	_, _ = fmt.Fprintln(r.stderr, restorePVCSectionHeader)
	_, _ = fmt.Fprintln(r.stderr, "Recreate the tokensmith PVC from the VolumeSnapshot recorded by `ochami-admin backup`.")
	_, _ = fmt.Fprintln(r.stderr, "If your CSI driver supports VolumeSnapshot, apply the saved snapshot manifest:")
	_, _ = fmt.Fprintf(r.stderr, "  kubectl apply -n %s%s -f %s\n",
		restoreNamespacePrefix, clusterName,
		filepath.Join(prefix, "tokensmith-pvc-snapshot.yaml"))
	_, _ = fmt.Fprintln(r.stderr, "Otherwise restore the data with `kubectl cp` from the directory:")
	_, _ = fmt.Fprintf(r.stderr, "  kubectl cp %s %s%s/<tokensmith-pod>:/var/lib/tokensmith\n",
		filepath.Join(prefix, "tokensmith-data"),
		restoreNamespacePrefix, clusterName)
	_, _ = fmt.Fprintln(r.stderr)

	_, _ = fmt.Fprintln(r.stderr, restoreCNPGSectionHeader)
	_, _ = fmt.Fprintln(r.stderr, "CloudNativePG recovery is performed by re-creating the Cluster CR with a")
	_, _ = fmt.Fprintln(r.stderr, "spec.bootstrap.recovery block pointing at the backup object name.")
	_, _ = fmt.Fprintln(r.stderr, "See: https://cloudnative-pg.io/documentation/current/recovery/")
	_, _ = fmt.Fprintln(r.stderr, "The operator's database reconciler will adopt the recovered Cluster on")
	_, _ = fmt.Fprintln(r.stderr, "the next reconcile loop.")
	_, _ = fmt.Fprintln(r.stderr)

	_, _ = fmt.Fprintln(r.stderr, restoreManualFooter)
}

// printSummary writes a final automated/manual breakdown to stdout.
func (r *restoreRunner) printSummary(clusterName, ns string) {
	_, _ = fmt.Fprintf(r.stdout, "Restore of cluster %q queued.\n", clusterName)
	_, _ = fmt.Fprintln(r.stdout, "Automated:")
	_, _ = fmt.Fprintf(r.stdout, "  - Server-side applied OpenCHAMICluster CR to namespace %q.\n", r.opts.namespace)
	_, _ = fmt.Fprintf(r.stdout, "  - Verified per-cluster namespace pre-conditions (%s).\n", ns)
	_, _ = fmt.Fprintln(r.stdout, "Manual (see stderr for details):")
	_, _ = fmt.Fprintln(r.stdout, "  - Vault raft snapshot restore.")
	_, _ = fmt.Fprintln(r.stdout, "  - tokensmith PVC restore from VolumeSnapshot.")
	_, _ = fmt.Fprintln(r.stdout, "  - CNPG database recovery via spec.bootstrap.recovery.")
}

// restoreLoadBackup reads <prefix>/cluster.yaml and parses it. Returns the
// parsed cluster object, the raw YAML bytes (for dry-run echo), and any error.
func restoreLoadBackup(prefix string) (*openchamiv1alpha1.OpenCHAMICluster, []byte, error) {
	path := filepath.Join(prefix, restoreClusterFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading backup file %s: %w", path, err)
	}
	cluster := &openchamiv1alpha1.OpenCHAMICluster{}
	if err := yaml.Unmarshal(data, cluster); err != nil {
		return nil, nil, fmt.Errorf("parsing backup file %s: %w", path, err)
	}
	if cluster.Spec.ClusterName == "" {
		return nil, nil, fmt.Errorf("backup file %s is missing spec.clusterName", path)
	}
	// SSA requires apiVersion+kind to be present on the unstructured wire
	// payload. Force them so a backup written without typemeta still applies.
	cluster.APIVersion = openchamiv1alpha1.GroupVersion.String()
	cluster.Kind = "OpenCHAMICluster"
	// Strip status; SSA on a non-status field manager would otherwise fight
	// the operator for ownership.
	cluster.Status = openchamiv1alpha1.OpenCHAMIClusterStatus{}
	// Strip resourceVersion so SSA does not optimistic-lock against a stale
	// generation.
	cluster.ResourceVersion = ""
	return cluster, data, nil
}
