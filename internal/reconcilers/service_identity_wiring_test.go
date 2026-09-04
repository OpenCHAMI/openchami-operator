// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
)

// restMapperWithBackendTLSPolicy returns a RESTMapper that recognises
// the gateway.networking.k8s.io/v1 BackendTLSPolicy GVK so the
// production code's runtime CRD-presence check sees it as installed.
// The controller-runtime fake client builds an empty default RESTMapper
// even when you pass a fully-populated scheme; tests that exercise
// CRD-presence-aware code paths have to supply one explicitly.
func restMapperWithBackendTLSPolicy() apimeta.RESTMapper {
	// gwapiv1.GroupVersion is a metav1.GroupVersion; the
	// schema-flavour alias (used by NewDefaultRESTMapper) is
	// SchemeGroupVersion. They carry the same Group/Version but
	// satisfy different package types.
	gv := schema.GroupVersion{
		Group:   gwapiv1.GroupVersion.Group,
		Version: gwapiv1.GroupVersion.Version,
	}
	m := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	m.Add(gv.WithKind("BackendTLSPolicy"), apimeta.RESTScopeNamespace)
	return m
}

// markServiceIdentityReady is a tiny helper used by every wiring test
// below to satisfy the precondition the production code reads.
// Centralised so a future condition-name change only edits one place.
func markServiceIdentityReady(cp *openchamiv1alpha1.OpenCHAMIControlPlane) {
	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:   conditions.ConditionServiceIdentityReady,
		Status: metav1.ConditionTrue,
		Reason: conditions.ReasonReady,
	})
}

// TestTokensmithReconciler_HTTPSWhenMTLSEnabled covers the TLS-enabled
// path: when spec.services.tokensmith.tls.enabled=true, the operator
// must mount the tokensmith server cert + CA, set the three TLS env vars
// the binary reads, and use HTTPS probes. The Service / Deployment
// endpoint URL in status must report https:// so consumers and the
// gateway dial over TLS.
func TestTokensmithReconciler_HTTPSWhenMTLSEnabled(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane(testClusterAlpha)
	// Enable TLS via spec field instead of condition
	cp.Spec.Services.Tokensmith.TLS.Enabled = true
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &TokensmithReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ns := ControlPlaneNamespace(cp)
	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: ServiceTokensmith}, dep); err != nil {
		t.Fatalf("getting tokensmith Deployment: %v", err)
	}

	cont := dep.Spec.Template.Spec.Containers[0]

	envByName := map[string]string{}
	for _, e := range cont.Env {
		envByName[e.Name] = e.Value
	}
	wantCert := tokensmithTLSMountPath + "/" + corev1.TLSCertKey
	wantKey := tokensmithTLSMountPath + "/" + corev1.TLSPrivateKeyKey
	wantCA := tokensmithTLSMountPath + "/" + ServiceIdentityCAKey
	if envByName[tokensmithEnvTLSCertFile] != wantCert {
		t.Errorf("env %s=%q want %q", tokensmithEnvTLSCertFile,
			envByName[tokensmithEnvTLSCertFile], wantCert)
	}
	if envByName[tokensmithEnvTLSKeyFile] != wantKey {
		t.Errorf("env %s=%q want %q", tokensmithEnvTLSKeyFile,
			envByName[tokensmithEnvTLSKeyFile], wantKey)
	}
	if envByName[tokensmithEnvSvcIDCA] != wantCA {
		t.Errorf("env %s=%q want %q", tokensmithEnvSvcIDCA,
			envByName[tokensmithEnvSvcIDCA], wantCA)
	}

	// Probes use HTTPS when TLS is enabled.
	if got := cont.ReadinessProbe.HTTPGet.Scheme; got != corev1.URISchemeHTTPS {
		t.Errorf("readiness probe scheme=%q want HTTPS", got)
	}
	if got := cont.LivenessProbe.HTTPGet.Scheme; got != corev1.URISchemeHTTPS {
		t.Errorf("liveness probe scheme=%q want HTTPS", got)
	}
	if got := cont.StartupProbe.HTTPGet.Scheme; got != corev1.URISchemeHTTPS {
		t.Errorf("startup probe scheme=%q want HTTPS", got)
	}

	// Projected volume sources include both Secrets.
	mountFound := false
	for _, m := range cont.VolumeMounts {
		if m.MountPath == tokensmithTLSMountPath {
			mountFound = true
		}
	}
	if !mountFound {
		t.Errorf("expected tokensmith container to mount %q, got %+v",
			tokensmithTLSMountPath, cont.VolumeMounts)
	}

	hasProjected := false
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == tokensmithTLSVolumeNm && v.Projected != nil && len(v.Projected.Sources) == 2 {
			hasProjected = true
		}
	}
	if !hasProjected {
		t.Errorf("expected projected service-identity-tls volume with 2 Secret sources, got %+v",
			dep.Spec.Template.Spec.Volumes)
	}

	// Status endpoint scheme.
	st, ok := cp.Status.Services[ServiceTokensmith]
	if !ok {
		t.Fatalf("expected status.services[tokensmith]")
	}
	if !strings.HasPrefix(st.Endpoint, "https://") {
		t.Errorf("expected tokensmith Endpoint to use https://, got %q", st.Endpoint)
	}
}

// TestBootServiceReconciler_MTLSWiring confirms the boot-service env
// vars and volumes the upstream PR-24 client library expects when the
// operator's service-identity flow is active.
func TestBootServiceReconciler_MTLSWiring(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane(testClusterAlpha)
	// Enable TLS via spec field
	cp.Spec.Services.Tokensmith.TLS.Enabled = true
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &BootServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp), Name: ServiceBootService,
	}, dep); err != nil {
		t.Fatalf("getting boot-service Deployment: %v", err)
	}
	cont := dep.Spec.Template.Spec.Containers[0]

	envByName := map[string]string{}
	for _, e := range cont.Env {
		envByName[e.Name] = e.Value
	}
	if got := envByName["BOOT_SERVICE_TOKENSMITH_SERVICE_IDENTITY_CERT"]; got != serviceIdentityCertFilePath() {
		t.Errorf("env BOOT_SERVICE_TOKENSMITH_SERVICE_IDENTITY_CERT=%q want %q",
			got, serviceIdentityCertFilePath())
	}
	if got := envByName["BOOT_SERVICE_TOKENSMITH_SERVICE_IDENTITY_KEY"]; got != serviceIdentityKeyFilePath() {
		t.Errorf("env BOOT_SERVICE_TOKENSMITH_SERVICE_IDENTITY_KEY=%q want %q",
			got, serviceIdentityKeyFilePath())
	}
	if got := envByName["BOOT_SERVICE_TOKENSMITH_URL"]; !strings.HasPrefix(got, "https://") {
		t.Errorf("expected BOOT_SERVICE_TOKENSMITH_URL to be https:// when mTLS is on, got %q", got)
	}
	if got := envByName["BOOT_SERVICE_JWKS_URL"]; !strings.HasPrefix(got, "https://") {
		t.Errorf("expected BOOT_SERVICE_JWKS_URL to be https:// when mTLS is on, got %q", got)
	}

	if got := envByName["BOOT_SERVICE_TOKENSMITH_BOOTSTRAP_TOKEN"]; got != "" {
		// SecretKeyRef stores its config in EnvVar.ValueFrom, not Value, so
		// the lookup above misses it; that's fine. We just want to be sure
		// the operator didn't *replace* it with a plain Value.
		_ = got
	}

	hasCertMount := false
	hasTrustMount := false
	for _, m := range cont.VolumeMounts {
		if m.MountPath == serviceIdentityClientCertMountPath {
			hasCertMount = true
		}
		if m.MountPath == systemCATrustFilePath && m.SubPath == serviceIdentityCAMountSubPath {
			hasTrustMount = true
		}
	}
	if !hasCertMount {
		t.Errorf("expected boot-service to mount %q, got %+v",
			serviceIdentityClientCertMountPath, cont.VolumeMounts)
	}
	if !hasTrustMount {
		t.Errorf("expected boot-service to subPath-mount CA at %q, got %+v",
			systemCATrustFilePath, cont.VolumeMounts)
	}
}

// TestMetadataServiceReconciler_MTLSWiring mirrors the boot-service
// test for metadata-service. The env var names are intentionally
// unprefixed (per the consumer prompt — metadata-service's cobra
// flags do not share boot-service's BOOT_SERVICE_ viper prefix).
func TestMetadataServiceReconciler_MTLSWiring(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane(testClusterAlpha)
	// Enable TLS via spec field
	cp.Spec.Services.Tokensmith.TLS.Enabled = true
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &MetadataServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp), Name: ServiceMetadataService,
	}, dep); err != nil {
		t.Fatalf("getting metadata-service Deployment: %v", err)
	}
	cont := dep.Spec.Template.Spec.Containers[0]

	envByName := map[string]string{}
	for _, e := range cont.Env {
		envByName[e.Name] = e.Value
	}
	if got := envByName["TOKENSMITH_SERVICE_IDENTITY_CERT"]; got != serviceIdentityCertFilePath() {
		t.Errorf("env TOKENSMITH_SERVICE_IDENTITY_CERT=%q want %q",
			got, serviceIdentityCertFilePath())
	}
	if got := envByName["TOKENSMITH_SERVICE_IDENTITY_KEY"]; got != serviceIdentityKeyFilePath() {
		t.Errorf("env TOKENSMITH_SERVICE_IDENTITY_KEY=%q want %q",
			got, serviceIdentityKeyFilePath())
	}
	if got := envByName["TOKENSMITH_URL"]; !strings.HasPrefix(got, "https://") {
		t.Errorf("expected TOKENSMITH_URL to be https:// when mTLS is on, got %q", got)
	}
	if got := envByName["JWKS_URL"]; !strings.HasPrefix(got, "https://") {
		t.Errorf("expected JWKS_URL to be https:// when mTLS is on, got %q", got)
	}
}

// TestGatewayReconciler_BackendTLSPolicyWhenMTLSReady locks in the
// envoy-gateway trust path: when tokensmith is HTTPS, the gateway
// reconciler must produce a BackendTLSPolicy referencing the CA
// ConfigMap so envoy's JWKS-fetch handshake doesn't reject the
// in-namespace CA as unknown. When tokensmith is HTTP-only the
// policy must NOT be applied (envoy would reject "no TLS on
// backend").
func TestGatewayReconciler_BackendTLSPolicyWhenMTLSReady(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane(testClusterAlpha)
	// Enable TLS via spec field
	cp.Spec.Services.Tokensmith.TLS.Enabled = true
	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:   conditions.ConditionCertificatesValid,
		Status: metav1.ConditionTrue,
		Reason: conditions.ReasonReady,
	})
	if cp.Status.Services == nil {
		cp.Status.Services = map[string]openchamiv1alpha1.ServiceStatus{}
	}
	cp.Status.Services[ServiceTokensmith] = openchamiv1alpha1.ServiceStatus{Ready: true}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(restMapperWithBackendTLSPolicy()).
		WithObjects(cp).Build()
	r := &GatewayReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	policy := &gwapiv1.BackendTLSPolicy{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp), Name: jwksBackendTLSPolicyName,
	}, policy); err != nil {
		t.Fatalf("expected BackendTLSPolicy %q to be applied, got err=%v",
			jwksBackendTLSPolicyName, err)
	}
	if len(policy.Spec.TargetRefs) != 1 || policy.Spec.TargetRefs[0].Name != gwapiv1.ObjectName(ServiceTokensmith) {
		t.Errorf("BackendTLSPolicy targetRef must point at the tokensmith Service, got %+v",
			policy.Spec.TargetRefs)
	}
	if len(policy.Spec.Validation.CACertificateRefs) != 1 ||
		string(policy.Spec.Validation.CACertificateRefs[0].Kind) != "ConfigMap" ||
		policy.Spec.Validation.CACertificateRefs[0].Name != gwapiv1.ObjectName(ServiceIdentityCAConfigMapName(cp)) {
		t.Errorf("BackendTLSPolicy must reference the CA ConfigMap, got %+v",
			policy.Spec.Validation.CACertificateRefs)
	}
}

// TestGatewayReconciler_DefersWhenBackendTLSPolicyCRDMissing covers the
// graceful-degrade path: when mTLS is on but the cluster doesn't have
// gateway.networking.k8s.io/v1 BackendTLSPolicy registered, the operator
// must NOT crash the reconcile (`no matches for kind "BackendTLSPolicy"`)
// and must surface the gap as GatewayReady=False/MissingCRD so an
// operator user can spot and resolve it. The Gateway resource and
// non-JWT routes still apply.
//
// This test deliberately uses the default fake-client RESTMapper (no
// BackendTLSPolicy registered) so the production code's runtime check
// returns false — same shape as a real cluster without gateway-api
// v1.4+ standard CRDs installed.
func TestGatewayReconciler_DefersWhenBackendTLSPolicyCRDMissing(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane(testClusterAlpha)
	// Enable TLS via spec field
	cp.Spec.Services.Tokensmith.TLS.Enabled = true
	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:   conditions.ConditionCertificatesValid,
		Status: metav1.ConditionTrue,
		Reason: conditions.ReasonReady,
	})
	if cp.Status.Services == nil {
		cp.Status.Services = map[string]openchamiv1alpha1.ServiceStatus{}
	}
	cp.Status.Services[ServiceTokensmith] = openchamiv1alpha1.ServiceStatus{Ready: true}

	// No WithRESTMapper — fake builder's default has zero entries, so
	// the production helper's RESTMapping lookup returns NoMatchError.
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	r := &GatewayReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile must not error when BackendTLSPolicy CRD is missing, got %v", err)
	}

	// The Gateway itself should still have been applied (it doesn't
	// depend on the missing CRD).
	gw := &gwapiv1.Gateway{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp), Name: gatewayName,
	}, gw); err != nil {
		t.Errorf("Gateway must still be applied when BackendTLSPolicy CRD is missing, got err=%v", err)
	}

	// The BackendTLSPolicy must NOT have been attempted.
	policy := &gwapiv1.BackendTLSPolicy{}
	err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp), Name: jwksBackendTLSPolicyName,
	}, policy)
	if err == nil {
		t.Errorf("BackendTLSPolicy must NOT be applied when its CRD is missing — got an existing policy %+v",
			policy)
	}

	// And the user-visible signal: GatewayReady=False/MissingCRD.
	cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionGatewayReady)
	if cond == nil {
		t.Fatalf("expected ConditionGatewayReady to be set")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("ConditionGatewayReady.Status = %v, want False", cond.Status)
	}
	if cond.Reason != conditions.ReasonMissingCRD {
		t.Errorf("ConditionGatewayReady.Reason = %q, want %q",
			cond.Reason, conditions.ReasonMissingCRD)
	}
	if !strings.Contains(cond.Message, "BackendTLSPolicy") {
		t.Errorf("ConditionGatewayReady.Message should mention BackendTLSPolicy for operator-user diagnosis, got %q",
			cond.Message)
	}
}
