// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package admin

import (
	"fmt"
	"io"
	"os"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"sigs.k8s.io/yaml"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/reconcilers"
)

// describe.go-scoped constants. Prefixed with `describe` to avoid collision
// with the other `internal/admin` files written by parallel sub-agents.
const (
	describeStdinSentinel = "-"
	describeNoNamespace   = "(cluster-scoped)"
	describeEmptyNote     = "(no objects)"
)

// describeOpts holds every flag value for the `describe` sub-command.
type describeOpts struct {
	file        string
	showDetails bool
}

// describeNamedSub pairs a SubReconciler with the human-readable section name
// used in the printed output. The order in which these are listed below
// determines the order they appear in the dry-run.
type describeNamedSub struct {
	Name string
	Sub  reconcilers.SubReconciler
}

// describeSubs returns the canonical ordered list of sub-reconcilers used for
// the dry-run output. The order mirrors the controller's reconcileAll subs
// slice so admins can map describe output back to reconcile order.
//
// All listed reconcilers' Describe() methods are safe to call on a
// zero-initialised struct: none dereference Client, Recorder, VaultClient, or
// S3Client. This is enforced by the SubReconciler contract documented in
// internal/reconcilers/reconciler.go ("Must not contact any external service").
func describeSubs() []describeNamedSub {
	return []describeNamedSub{
		{"Namespace", &reconcilers.NamespaceReconciler{}},
		{"RBAC", &reconcilers.RBACReconciler{}},
		{"Vault", &reconcilers.VaultReconciler{}},
		{"Bucket", &reconcilers.BucketReconciler{}},
		{"Database", &reconcilers.DatabaseReconciler{}},
		{"SMD", &reconcilers.SMDReconciler{}},
		{"Tokensmith", &reconcilers.TokensmithReconciler{}},
		{"Boot Service", &reconcilers.BootServiceReconciler{}},
		{"Metadata Service", &reconcilers.MetadataServiceReconciler{}},
		{"Network Probe", &reconcilers.NetworkProbeReconciler{}},
		{"CoreDHCP", &reconcilers.CoreDHCPReconciler{}},
		{"Magellan", &reconcilers.MagellanReconciler{}},
		{"Certificates", &reconcilers.CertificatesReconciler{}},
		{"Gateway", &reconcilers.GatewayReconciler{}},
		{"Network Policies", &reconcilers.NetworkPoliciesReconciler{}},
		{"Topology", &reconcilers.TopologyReconciler{}},
		{"ServiceMonitor", &reconcilers.ServiceMonitorReconciler{}},
		{"Log Bucket", &reconcilers.LogBucketReconciler{}},
		{"Funicular", &reconcilers.FunicularReconciler{}},
	}
}

// DescribeCmd returns the `ochami-admin describe` command.
//
// describe loads an OpenCHAMIControlPlane CR YAML from disk (or stdin) and prints
// a human-readable rendering of every Kubernetes object the operator would
// apply, grouped by sub-reconciler. No Kubernetes connection is required.
func DescribeCmd() *cobra.Command {
	o := &describeOpts{}
	cmd := &cobra.Command{
		Use:   "describe",
		Short: "Dry-run view of what the operator would apply for a given cluster manifest",
		Long: "Dry-run view: reads an OpenCHAMIControlPlane manifest and prints the " +
			"Kubernetes objects each sub-reconciler would apply, in apply order, " +
			"grouped by reconciler. Does not connect to a Kubernetes cluster.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.run(cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	o.bindFlags(cmd.Flags())
	return cmd
}

func (o *describeOpts) bindFlags(f *pflag.FlagSet) {
	f.StringVarP(&o.file, "file", "f", "",
		"Path to OpenCHAMIControlPlane manifest YAML (use '-' to read from stdin)")
	f.BoolVar(&o.showDetails, "show-details", false,
		"Render extra per-object details (env var names, ports, secret references)")
	_ = cobra.MarkFlagRequired(f, "file")
}

// run loads the manifest, walks every sub-reconciler's Describe(), and emits
// the formatted output to w.
func (o *describeOpts) run(in io.Reader, out io.Writer) error {
	if o.file == "" {
		return fmt.Errorf("--file is required")
	}

	data, err := describeReadInput(in, o.file)
	if err != nil {
		return err
	}

	cp := &openchamiv1alpha1.OpenCHAMIControlPlane{}
	if err := yaml.Unmarshal(data, cp); err != nil {
		return fmt.Errorf("parsing manifest: %w", err)
	}
	if cp.Spec.ClusterName == "" {
		return fmt.Errorf("manifest is missing spec.clusterName")
	}

	return describeRender(out, cp, o.showDetails)
}

// describeReadInput returns the raw bytes of the manifest. When file is "-"
// it reads from in (stdin); otherwise it opens the named file from disk.
func describeReadInput(in io.Reader, file string) ([]byte, error) {
	if file == describeStdinSentinel {
		data, err := io.ReadAll(in)
		if err != nil {
			return nil, fmt.Errorf("reading manifest from stdin: %w", err)
		}
		return data, nil
	}
	data, err := os.ReadFile(file) //nolint:gosec // user-supplied manifest path is intentional.
	if err != nil {
		return nil, fmt.Errorf("reading manifest %s: %w", file, err)
	}
	return data, nil
}

// describeRender writes the formatted dry-run report for cp.
func describeRender(w io.Writer, cp *openchamiv1alpha1.OpenCHAMIControlPlane, details bool) error {
	ns := reconcilers.ControlPlaneNamespace(cp)
	if _, err := fmt.Fprintf(w, "=== Control Plane: %s ===\n", cp.Spec.ClusterName); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Domain:    %s\n", cp.Spec.Domain); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Namespace: %s\n\n", ns); err != nil {
		return err
	}
	// Spec.Notes is free-form markdown. Render verbatim so admins reading
	// `ochami-admin describe` see site context the author wrote down.
	if notes := strings.TrimSpace(cp.Spec.Notes); notes != "" {
		if _, err := fmt.Fprintf(w, "== Notes ==\n%s\n\n", notes); err != nil {
			return err
		}
	}

	total := 0
	for _, named := range describeSubs() {
		objs, err := named.Sub.Describe(cp)
		if err != nil {
			return fmt.Errorf("%s: describe: %w", named.Name, err)
		}
		describeRenderSection(w, named.Name, objs, details)
		total += len(objs)
	}

	if _, err := fmt.Fprintf(w, "Total: %d Kubernetes objects.\n", total); err != nil {
		return err
	}
	return nil
}

// describeRenderSection prints the header and one line per object.
func describeRenderSection(w io.Writer, name string, objs []client.Object, details bool) {
	_, _ = fmt.Fprintf(w, "== %s ==\n", name)
	if len(objs) == 0 {
		// We can't know from the empty slice whether this is "disabled" vs
		// "nothing to apply". Both rendering choices satisfy the documented
		// describe-disabled contract; emit the neutral marker so admins see
		// the section was considered but produced nothing.
		_, _ = fmt.Fprintf(w, "  %s\n\n", describeEmptyNote)
		return
	}
	for _, obj := range objs {
		_, _ = fmt.Fprintln(w, describeFormatObject(obj, details))
	}
	_, _ = fmt.Fprintln(w)
}

// describeFormatObject returns a single-line summary for obj. When details is
// true the line is followed by indented sub-lines describing env vars, ports,
// and secret references found in the object (when applicable).
func describeFormatObject(obj client.Object, details bool) string {
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	if kind == "" {
		// Some Describe() implementations build typed objects without setting
		// TypeMeta. Fall back to the Go type name so output stays readable.
		kind = describeFallbackKind(obj)
	}
	ns := obj.GetNamespace()
	if ns == "" {
		ns = describeNoNamespace
	}
	head := fmt.Sprintf("%s/%s/%s", kind, ns, obj.GetName())

	tail := describeShortTail(obj)
	line := head
	if tail != "" {
		line = fmt.Sprintf("%-60s %s", head, tail)
	}

	if !details {
		return line
	}

	extras := describeRenderDetails(obj)
	if extras == "" {
		return line
	}
	return line + "\n" + extras
}

// describeFallbackKind returns the leaf type name for obj when TypeMeta is
// unset. Trims pointer prefix and package path so e.g.
// "*v1.Deployment" becomes "Deployment".
func describeFallbackKind(obj client.Object) string {
	t := fmt.Sprintf("%T", obj)
	if i := strings.LastIndex(t, "."); i >= 0 {
		t = t[i+1:]
	}
	return t
}

// describeShortTail returns a brief inline summary suitable for the right side
// of the head line. Keeps each kind's most operationally relevant fields
// visible without --show-details.
func describeShortTail(obj client.Object) string {
	switch o := obj.(type) {
	case *appsv1.Deployment:
		replicas := int32(1)
		if o.Spec.Replicas != nil {
			replicas = *o.Spec.Replicas
		}
		image := describePrimaryImage(o.Spec.Template.Spec.Containers)
		if image == "" {
			return fmt.Sprintf("(replicas: %d)", replicas)
		}
		return fmt.Sprintf("(replicas: %d, image: %s)", replicas, image)
	case *appsv1.DaemonSet:
		image := describePrimaryImage(o.Spec.Template.Spec.Containers)
		if image == "" {
			return ""
		}
		return fmt.Sprintf("(image: %s)", image)
	case *corev1.Service:
		ports := make([]string, 0, len(o.Spec.Ports))
		for _, p := range o.Spec.Ports {
			ports = append(ports, fmt.Sprintf("%d", p.Port))
		}
		if len(ports) == 0 {
			return ""
		}
		return fmt.Sprintf("(port: %s)", strings.Join(ports, ","))
	}
	return ""
}

// describePrimaryImage returns the image string of the first container, or ""
// when the container list is empty.
func describePrimaryImage(containers []corev1.Container) string {
	if len(containers) == 0 {
		return ""
	}
	return containers[0].Image
}

// describeRenderDetails returns indented detail lines for kinds that carry
// pod templates (env vars, ports, secret refs). Returns "" when the object
// has no useful details to render.
func describeRenderDetails(obj client.Object) string {
	var containers []corev1.Container
	switch o := obj.(type) {
	case *appsv1.Deployment:
		containers = o.Spec.Template.Spec.Containers
	case *appsv1.DaemonSet:
		containers = o.Spec.Template.Spec.Containers
	}
	if len(containers) == 0 {
		return ""
	}

	var b strings.Builder
	for _, c := range containers {
		fmt.Fprintf(&b, "    container: %s\n", c.Name)

		if envNames := describeRenderEnv(c.Env); envNames != "" {
			fmt.Fprintf(&b, "      env: %s\n", envNames)
		}

		if secretRefs := describeRenderSecretRefs(c.Env); secretRefs != "" {
			fmt.Fprintf(&b, "      secretRefs: %s\n", secretRefs)
		}

		if ports := describeRenderPorts(c.Ports); ports != "" {
			fmt.Fprintf(&b, "      ports: %s\n", ports)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// describeRenderEnv returns a comma-separated list of EnvVar names, or "" if
// the slice is empty.
func describeRenderEnv(env []corev1.EnvVar) string {
	if len(env) == 0 {
		return ""
	}
	names := make([]string, 0, len(env))
	for _, e := range env {
		names = append(names, e.Name)
	}
	return strings.Join(names, ",")
}

// describeRenderSecretRefs walks env vars looking for Secret-backed sources
// and returns "secretName/key,..." for each. Returns "" if none.
func describeRenderSecretRefs(env []corev1.EnvVar) string {
	var refs []string
	for _, e := range env {
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			continue
		}
		ref := e.ValueFrom.SecretKeyRef
		refs = append(refs, fmt.Sprintf("%s/%s", ref.Name, ref.Key))
	}
	if len(refs) == 0 {
		return ""
	}
	return strings.Join(refs, ",")
}

// describeRenderPorts returns "name:port/proto,..." for each container port.
// Returns "" when the slice is empty.
func describeRenderPorts(ports []corev1.ContainerPort) string {
	if len(ports) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		proto := string(p.Protocol)
		if proto == "" {
			proto = "TCP"
		}
		name := p.Name
		if name == "" {
			name = "-"
		}
		parts = append(parts, fmt.Sprintf("%s:%d/%s", name, p.ContainerPort, proto))
	}
	return strings.Join(parts, ",")
}
