// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func metadataEnvByName(envs []corev1.EnvVar, name string) (corev1.EnvVar, bool) {
	for _, e := range envs {
		if e.Name == name {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

func TestMetadataServiceReconciler_DisabledSkips(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.MetadataService.Enabled = false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &MetadataServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when disabled, got %+v", res)
	}

	dep := &appsv1.Deployment{}
	err = c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      ServiceMetadataService,
	}, dep)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected no Deployment when disabled, got err=%v", err)
	}
}

func TestMetadataServiceReconciler_AppliesAllResources(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.MetadataService.Enabled = true
	cp.Spec.Services.MetadataService.Replicas = 2
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &MetadataServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ns := ControlPlaneNamespace(cp)

	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ns, Name: ServiceMetadataService,
	}, dep); err != nil {
		t.Fatalf("getting Deployment: %v", err)
	}
	if dep.Name != "metadata-service" {
		t.Errorf("expected Deployment name metadata-service, got %s", dep.Name)
	}
	if len(dep.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(dep.Spec.Template.Spec.Containers))
	}
	container := dep.Spec.Template.Spec.Containers[0]
	if len(container.Ports) != 1 || container.Ports[0].ContainerPort != 8081 {
		t.Errorf("expected container port 8081, got %+v", container.Ports)
	}

	// SMD_URL is read by the metadata-service binary via direct os.Getenv.
	smdURLEnv, ok := metadataEnvByName(container.Env, "SMD_URL")
	if !ok {
		t.Fatalf("SMD_URL env var not found")
	}
	wantSMD := "http://smd.openchami-alpha.svc.cluster.local:27779"
	if smdURLEnv.Value != wantSMD {
		t.Errorf("expected SMD_URL=%q, got %q", wantSMD, smdURLEnv.Value)
	}

	// Post-#8 fabrica refactor: the binary takes flags rather than
	// OCHAMI_METADATA_* env vars. Port and data-dir are passed via
	// container args so the deployment is explicit and independent of
	// any viper alias bugs in the upstream binary.
	wantArgs := []string{"serve", "--port", "8081", "--data-dir", metadataServiceDataDir}
	if len(container.Args) != len(wantArgs) {
		t.Fatalf("expected container args %v, got %v", wantArgs, container.Args)
	}
	for i, a := range wantArgs {
		if container.Args[i] != a {
			t.Errorf("container args[%d]: expected %q, got %q", i, a, container.Args[i])
		}
	}

	// /data must be a writable emptyDir mount so the binary can create
	// its file backend (default --data-dir=/data) under
	// readOnlyRootFilesystem.
	var dataMountFound bool
	for _, vm := range container.VolumeMounts {
		if vm.MountPath == metadataServiceDataDir {
			dataMountFound = true
		}
	}
	if !dataMountFound {
		t.Errorf("expected a writable volume mount at /data, got %+v", container.VolumeMounts)
	}

	// Service
	svc := &corev1.Service{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ns, Name: ServiceMetadataService,
	}, svc); err != nil {
		t.Fatalf("getting Service: %v", err)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 8081 {
		t.Errorf("expected Service port 8081, got %+v", svc.Spec.Ports)
	}
	// Default (PreserveClientIP=false): operator-managed metadata-service
	// is a ClusterIP with no externalTrafficPolicy override; gateway-routed
	// traffic carries the original client IP via X-Forwarded-For headers.
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("expected default Type=ClusterIP, got %q", svc.Spec.Type)
	}
	if svc.Spec.ExternalTrafficPolicy != "" {
		t.Errorf("expected default ExternalTrafficPolicy unset, got %q", svc.Spec.ExternalTrafficPolicy)
	}

	// PDB
	pdb := &policyv1.PodDisruptionBudget{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ns, Name: ServiceMetadataService,
	}, pdb); err != nil {
		t.Fatalf("getting PDB: %v", err)
	}
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Errorf("expected PDB MinAvailable=1, got %+v", pdb.Spec.MinAvailable)
	}
}

func TestMetadataServiceReconciler_RequeuesUntilAvailable(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.MetadataService.Enabled = true
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &MetadataServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while not available, got %+v", res)
	}

	got, ok := cp.Status.Services[ServiceMetadataService]
	if !ok {
		t.Fatalf("expected metadata-service in status.services")
	}
	if got.Ready {
		t.Errorf("expected Ready=false while AvailableReplicas=0, got Ready=true")
	}
}

func TestMetadataServiceReconciler_ReadyWhenAvailable(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.MetadataService.Enabled = true

	// Pre-create a Deployment so we can stamp status.AvailableReplicas=1.
	preDep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceMetadataService,
			Namespace: ControlPlaneNamespace(cp),
		},
		Status: appsv1.DeploymentStatus{AvailableReplicas: 1},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cp).
		WithStatusSubresource(&appsv1.Deployment{}).
		WithObjects(preDep).
		Build()

	r := &MetadataServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when ready, got %+v", res)
	}

	got, ok := cp.Status.Services[ServiceMetadataService]
	if !ok {
		t.Fatalf("expected metadata-service in status.services")
	}
	if !got.Ready {
		t.Errorf("expected Ready=true with AvailableReplicas=1, got Ready=false")
	}
	wantEndpoint := "http://metadata-service.openchami-alpha.svc.cluster.local:8081"
	if got.Endpoint != wantEndpoint {
		t.Errorf("expected endpoint=%q, got %q", wantEndpoint, got.Endpoint)
	}
}

// TestMetadataServiceReconciler_TokensmithEnv guards the env-var
// wiring that lets metadata-service initialize with a bootstrap token
// and authenticate to SMD as `sub=metadata-service` for node/group
// reads.
//
// metadata-service v0.1.x reads SMD endpoint config via bare os.Getenv
// (no viper prefix), so the operator wires SMD_URL, TOKENSMITH_URL,
// TOKENSMITH_TARGET_SERVICE, TOKENSMITH_BOOTSTRAP_TOKEN (SecretKeyRef,
// Optional=true so the pod can come up before the operator has minted
// the first token), and JWKS_URL. Pin these names so the PR build
// can adopt the same env shape verbatim.
func TestMetadataServiceReconciler_TokensmithEnv(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.MetadataService.Enabled = true
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &MetadataServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      ServiceMetadataService,
	}, dep); err != nil {
		t.Fatalf("getting metadata-service Deployment: %v", err)
	}
	env := dep.Spec.Template.Spec.Containers[0].Env

	tsURL, ok := metadataEnvByName(env, "TOKENSMITH_URL")
	if !ok || tsURL.Value != ServiceURL(cp, ServiceTokensmith) {
		t.Errorf("TOKENSMITH_URL expected %q, got %+v",
			ServiceURL(cp, ServiceTokensmith), tsURL)
	}
	tgt, _ := metadataEnvByName(env, "TOKENSMITH_TARGET_SERVICE")
	if tgt.Value != bootstrapTokenAudience {
		t.Errorf("TOKENSMITH_TARGET_SERVICE expected %q, got %q", bootstrapTokenAudience, tgt.Value)
	}

	tok, ok := metadataEnvByName(env, "TOKENSMITH_BOOTSTRAP_TOKEN")
	if !ok {
		t.Fatalf("TOKENSMITH_BOOTSTRAP_TOKEN env var missing — metadata-service " +
			"won't be able to exchange for an aud=hsm JWT")
	}
	if tok.ValueFrom == nil || tok.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("TOKENSMITH_BOOTSTRAP_TOKEN missing SecretKeyRef: %+v", tok)
	}
	wantSecret := SecretName(cp, SuffixMetadataServiceBootstr)
	if tok.ValueFrom.SecretKeyRef.Name != wantSecret {
		t.Errorf("bootstrap-token secret name expected %q, got %q",
			wantSecret, tok.ValueFrom.SecretKeyRef.Name)
	}
	if tok.ValueFrom.SecretKeyRef.Key != BootstrapTokenKey {
		t.Errorf("bootstrap-token secret key expected %q, got %q",
			BootstrapTokenKey, tok.ValueFrom.SecretKeyRef.Key)
	}
	if tok.ValueFrom.SecretKeyRef.Optional == nil || !*tok.ValueFrom.SecretKeyRef.Optional {
		t.Errorf("bootstrap-token SecretKeyRef must be Optional=true so the " +
			"Deployment can come up before tokensmith has minted the first token")
	}
	// Critical isolation guarantee: metadata-service must NOT mount
	// boot-service's bootstrap-token Secret. Different `sub` claim,
	// different scopes — sharing the token across services would
	// either over-permission metadata-service or break boot-service.
	if tok.ValueFrom.SecretKeyRef.Name == SecretName(cp, SuffixBootServiceBootstr) {
		t.Errorf("metadata-service must NOT mount boot-service's bootstrap-token "+
			"Secret %q — each service needs its own subject-scoped token",
			SecretName(cp, SuffixBootServiceBootstr))
	}

	jwks, _ := metadataEnvByName(env, "JWKS_URL")
	wantJWKS := ServiceURL(cp, ServiceTokensmith) + "/.well-known/jwks.json"
	if jwks.Value != wantJWKS {
		t.Errorf("JWKS_URL expected %q, got %q", wantJWKS, jwks.Value)
	}
}

// TestMetadataServiceReconciler_PreserveClientIP asserts that opting into
// PreserveClientIP flips the Service to NodePort with
// externalTrafficPolicy=Local — kube-deploy's pattern for letting
// metadata-service identify a node by its source IP when no WireGuard
// tunnel network is in front of the cluster.
func TestMetadataServiceReconciler_PreserveClientIP(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.MetadataService.Enabled = true
	cp.Spec.Services.MetadataService.PreserveClientIP = true
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &MetadataServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	svc := &corev1.Service{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      ServiceMetadataService,
	}, svc); err != nil {
		t.Fatalf("getting Service: %v", err)
	}
	if svc.Spec.Type != corev1.ServiceTypeNodePort {
		t.Errorf("expected Type=NodePort with PreserveClientIP, got %q", svc.Spec.Type)
	}
	if svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		t.Errorf("expected ExternalTrafficPolicy=Local with PreserveClientIP, got %q", svc.Spec.ExternalTrafficPolicy)
	}
}

func TestMetadataServiceReconciler_TwoClustersIsolated(t *testing.T) {
	scheme := newScheme(t)
	for _, name := range []string{testControlPlaneRed, testControlPlaneBlue} {
		cp := newControlPlane(name)
		cp.Spec.Services.MetadataService.Enabled = true
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
		r := &MetadataServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
		if _, err := r.Reconcile(context.Background(), cp); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}

		dep := &appsv1.Deployment{}
		if err := c.Get(context.Background(), types.NamespacedName{
			Namespace: "openchami-" + name,
			Name:      ServiceMetadataService,
		}, dep); err != nil {
			t.Fatalf("getting Deployment for %s: %v", name, err)
		}
		if dep.Namespace != "openchami-"+name {
			t.Errorf("expected namespace openchami-%s, got %s", name, dep.Namespace)
		}

		container := dep.Spec.Template.Spec.Containers[0]
		smdURLEnv, ok := metadataEnvByName(container.Env, "SMD_URL")
		if !ok {
			t.Fatalf("SMD_URL env var not found for %s", name)
		}
		wantSMD := "http://smd.openchami-" + name + ".svc.cluster.local:27779"
		if smdURLEnv.Value != wantSMD {
			t.Errorf("expected SMD_URL=%q for %s, got %q", wantSMD, name, smdURLEnv.Value)
		}
	}
}
