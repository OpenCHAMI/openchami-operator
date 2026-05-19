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

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

func newBootServiceCluster(name string) *openchamiv1alpha1.OpenCHAMIControlPlane {
	cp := newControlPlane(name)
	cp.Spec.Services.BootService.Enabled = true
	cp.Spec.Services.BootService.Replicas = 2
	return cp
}

func bootServiceEnvByName(envs []corev1.EnvVar, name string) (corev1.EnvVar, bool) {
	for _, e := range envs {
		if e.Name == name {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

func TestBootServiceReconciler_DisabledSkips(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.BootService.Enabled = false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &BootServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
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
		Name:      ServiceBootService,
	}, dep)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected boot-service Deployment to be absent, got err=%v", err)
	}
}

func TestBootServiceReconciler_AppliesAllResources(t *testing.T) {
	scheme := newScheme(t)
	cp := newBootServiceCluster("alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &BootServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ns := ControlPlaneNamespace(cp)

	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: ServiceBootService}, dep); err != nil {
		t.Fatalf("getting boot-service Deployment: %v", err)
	}
	if len(dep.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(dep.Spec.Template.Spec.Containers))
	}
	cont := dep.Spec.Template.Spec.Containers[0]
	if cont.Name != ServiceBootService {
		t.Errorf("expected container name %q, got %q", ServiceBootService, cont.Name)
	}
	if len(cont.Ports) != 1 || cont.Ports[0].ContainerPort != 27778 {
		t.Errorf("expected single port 27778, got %+v", cont.Ports)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 2 {
		t.Errorf("expected replicas=2, got %v", dep.Spec.Replicas)
	}
	if dep.Spec.Template.Spec.ServiceAccountName != ServiceBootService {
		t.Errorf("expected ServiceAccountName=%q, got %q", ServiceBootService, dep.Spec.Template.Spec.ServiceAccountName)
	}

	dbpass, ok := bootServiceEnvByName(cont.Env, "BOOT_SERVICE_DBPASS")
	if !ok {
		t.Fatalf("BOOT_SERVICE_DBPASS env var missing")
	}
	if dbpass.ValueFrom == nil || dbpass.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("BOOT_SERVICE_DBPASS missing SecretKeyRef")
	}
	if dbpass.ValueFrom.SecretKeyRef.Name != SecretName(cp, SuffixBootServiceDB) {
		t.Errorf("BOOT_SERVICE_DBPASS expected secret %q, got %q",
			SecretName(cp, SuffixBootServiceDB), dbpass.ValueFrom.SecretKeyRef.Name)
	}
	if dbpass.ValueFrom.SecretKeyRef.Key != VaultKeyDBPassword {
		t.Errorf("BOOT_SERVICE_DBPASS expected key %q, got %q",
			VaultKeyDBPassword, dbpass.ValueFrom.SecretKeyRef.Key)
	}

	dbname, _ := bootServiceEnvByName(cont.Env, "BOOT_SERVICE_DBNAME")
	if dbname.Value != bootServiceDBName {
		t.Errorf("BOOT_SERVICE_DBNAME expected %q, got %q", bootServiceDBName, dbname.Value)
	}

	s3ak, ok := bootServiceEnvByName(cont.Env, "BOOT_SERVICE_S3_ACCESS_KEY")
	if !ok || s3ak.ValueFrom == nil || s3ak.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("BOOT_SERVICE_S3_ACCESS_KEY missing or has no SecretKeyRef: %+v", s3ak)
	}
	if s3ak.ValueFrom.SecretKeyRef.Name != SecretName(cp, SuffixS3Credentials) {
		t.Errorf("BOOT_SERVICE_S3_ACCESS_KEY expected secret %q, got %q",
			SecretName(cp, SuffixS3Credentials), s3ak.ValueFrom.SecretKeyRef.Name)
	}
	if s3ak.ValueFrom.SecretKeyRef.Key != s3AccessKeyKey {
		t.Errorf("BOOT_SERVICE_S3_ACCESS_KEY expected key %q, got %q", s3AccessKeyKey, s3ak.ValueFrom.SecretKeyRef.Key)
	}

	s3bucket, _ := bootServiceEnvByName(cont.Env, "BOOT_SERVICE_S3_BUCKET")
	if s3bucket.Value != BootBucketName(cp) {
		t.Errorf("BOOT_SERVICE_S3_BUCKET expected %q, got %q", BootBucketName(cp), s3bucket.Value)
	}

	svc := &corev1.Service{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: ServiceBootService}, svc); err != nil {
		t.Fatalf("getting boot-service Service: %v", err)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 27778 {
		t.Errorf("expected service port 27778, got %+v", svc.Spec.Ports)
	}

	pdb := &policyv1.PodDisruptionBudget{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: ServiceBootService}, pdb); err != nil {
		t.Fatalf("getting boot-service PDB: %v", err)
	}
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Errorf("expected PDB MinAvailable=1, got %+v", pdb.Spec.MinAvailable)
	}
}

// TestBootServiceReconciler_HSMAndTokensmithEnv guards the env-var
// names the boot-service binary actually reads.
//
// boot-service v0.1.5 binds its config via viper with the
// `BOOT_SERVICE_` prefix and the mapstructure tag `hsm_url` — so the
// HSM URL must arrive as BOOT_SERVICE_HSM_URL. The operator previously
// set BOOT_SERVICE_SMD_URL, which the binary silently ignored
// (`hsm=false` in the startup log was the only symptom). Pin both the
// HSM env name and the tokensmith bootstrap-token wiring so a future
// rename can't reintroduce the silent-misconfig regression.
func TestBootServiceReconciler_HSMAndTokensmithEnv(t *testing.T) {
	scheme := newScheme(t)
	cp := newBootServiceCluster("alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &BootServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      ServiceBootService,
	}, dep); err != nil {
		t.Fatalf("getting boot-service Deployment: %v", err)
	}
	env := dep.Spec.Template.Spec.Containers[0].Env

	hsmURL, ok := bootServiceEnvByName(env, "BOOT_SERVICE_HSM_URL")
	if !ok {
		t.Fatalf("BOOT_SERVICE_HSM_URL missing — boot-service won't enable HSM sync")
	}
	if hsmURL.Value != ServiceURL(cp, ServiceSMD) {
		t.Errorf("BOOT_SERVICE_HSM_URL expected %q, got %q", ServiceURL(cp, ServiceSMD), hsmURL.Value)
	}
	if _, present := bootServiceEnvByName(env, "BOOT_SERVICE_SMD_URL"); present {
		t.Errorf("BOOT_SERVICE_SMD_URL must NOT be set — the binary reads BOOT_SERVICE_HSM_URL")
	}

	// boot-service v0.1.5 gates the outbound tokensmith service-token
	// exchange behind enable_auth — without ENABLE_AUTH=true the
	// BOOT_SERVICE_TOKENSMITH_* env vars below are ignored, HSM calls
	// go out anonymous, and SMD's auth-gated reads (group membership)
	// return 401. Inbound auth is a no-op so iPXE bootscript fetches
	// remain unauthenticated.
	authEnabled, _ := bootServiceEnvByName(env, "BOOT_SERVICE_ENABLE_AUTH")
	if authEnabled.Value != "true" { //nolint:goconst // env value, not a label
		t.Errorf("BOOT_SERVICE_ENABLE_AUTH must be \"true\" or boot-service will " +
			"skip the tokensmith exchange and SMD reads will be anonymous (HTTP 401 " +
			"on auth-gated endpoints)")
	}

	tsURL, ok := bootServiceEnvByName(env, "BOOT_SERVICE_TOKENSMITH_URL")
	if !ok || tsURL.Value != ServiceURL(cp, ServiceTokensmith) {
		t.Errorf("BOOT_SERVICE_TOKENSMITH_URL expected %q, got %+v",
			ServiceURL(cp, ServiceTokensmith), tsURL)
	}
	tgt, _ := bootServiceEnvByName(env, "BOOT_SERVICE_TOKENSMITH_TARGET_SERVICE")
	if tgt.Value != bootstrapTokenAudience {
		t.Errorf("BOOT_SERVICE_TOKENSMITH_TARGET_SERVICE expected %q, got %q", bootstrapTokenAudience, tgt.Value)
	}

	tok, ok := bootServiceEnvByName(env, "BOOT_SERVICE_TOKENSMITH_BOOTSTRAP_TOKEN")
	if !ok {
		t.Fatalf("BOOT_SERVICE_TOKENSMITH_BOOTSTRAP_TOKEN missing")
	}
	if tok.ValueFrom == nil || tok.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("BOOT_SERVICE_TOKENSMITH_BOOTSTRAP_TOKEN missing SecretKeyRef: %+v", tok)
	}
	wantSecret := SecretName(cp, SuffixBootServiceBootstr)
	if tok.ValueFrom.SecretKeyRef.Name != wantSecret {
		t.Errorf("bootstrap-token secret name expected %q, got %q",
			wantSecret, tok.ValueFrom.SecretKeyRef.Name)
	}
	if tok.ValueFrom.SecretKeyRef.Key != BootstrapTokenKey {
		t.Errorf("bootstrap-token secret key expected %q, got %q",
			BootstrapTokenKey, tok.ValueFrom.SecretKeyRef.Key)
	}
	if tok.ValueFrom.SecretKeyRef.Optional == nil || !*tok.ValueFrom.SecretKeyRef.Optional {
		t.Errorf("bootstrap-token SecretKeyRef must be Optional=true so the Deployment " +
			"can come up before the operator has minted the token")
	}
	// Critical isolation guarantee: boot-service must NOT mount
	// metadata-service's bootstrap-token Secret. Bootstrap tokens are
	// single-use and subject-scoped; aliasing across services would
	// either over-permission one or wedge both into CrashLoopBackOff
	// with "already consumed". The symmetric assertion lives in
	// metadata_service_test.go:TestMetadataServiceReconciler_TokensmithEnv.
	if tok.ValueFrom.SecretKeyRef.Name == SecretName(cp, SuffixMetadataServiceBootstr) {
		t.Errorf("boot-service must NOT mount metadata-service's bootstrap-token "+
			"Secret %q — each service needs its own subject-scoped token",
			SecretName(cp, SuffixMetadataServiceBootstr))
	}
}

func TestBootServiceReconciler_RequeuesUntilAvailable(t *testing.T) {
	scheme := newScheme(t)
	cp := newBootServiceCluster("alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &BootServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while not available, got %+v", res)
	}
	st, ok := cp.Status.Services[ServiceBootService]
	if !ok {
		t.Fatalf("expected status.Services[%q] to be set", ServiceBootService)
	}
	if st.Ready {
		t.Errorf("expected Ready=false, got true")
	}
}

func TestBootServiceReconciler_ReadyWhenAvailable(t *testing.T) {
	scheme := newScheme(t)
	cp := newBootServiceCluster("alpha")

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceBootService,
			Namespace: ControlPlaneNamespace(cp),
		},
		Status: appsv1.DeploymentStatus{AvailableReplicas: 1},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cp).
		WithStatusSubresource(&appsv1.Deployment{}).
		WithObjects(dep).
		Build()

	r := &BootServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when ready, got %+v", res)
	}
	st, ok := cp.Status.Services[ServiceBootService]
	if !ok {
		t.Fatalf("expected status.Services[%q] to be set", ServiceBootService)
	}
	if !st.Ready {
		t.Errorf("expected Ready=true, got false (msg=%q)", st.Message)
	}
	if st.Endpoint == "" {
		t.Errorf("expected non-empty endpoint")
	}
}

func TestBootServiceReconciler_TwoClustersIsolated(t *testing.T) {
	scheme := newScheme(t)
	for _, name := range []string{testControlPlaneRed, testControlPlaneBlue} {
		cp := newBootServiceCluster(name)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
		r := &BootServiceReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
		if _, err := r.Reconcile(context.Background(), cp); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}

		ns := "openchami-" + name
		dep := &appsv1.Deployment{}
		if err := c.Get(context.Background(), types.NamespacedName{
			Namespace: ns,
			Name:      ServiceBootService,
		}, dep); err != nil {
			t.Fatalf("getting boot-service Deployment for %s: %v", name, err)
		}
		if dep.Namespace != ns {
			t.Errorf("expected namespace %q, got %q", ns, dep.Namespace)
		}

		cont := dep.Spec.Template.Spec.Containers[0]
		dbpass, _ := bootServiceEnvByName(cont.Env, "BOOT_SERVICE_DBPASS")
		expectedDBSecret := "openchami-" + name + "-" + SuffixBootServiceDB
		if dbpass.ValueFrom.SecretKeyRef.Name != expectedDBSecret {
			t.Errorf("[%s] expected DB secret %q, got %q", name, expectedDBSecret,
				dbpass.ValueFrom.SecretKeyRef.Name)
		}

		s3ak, _ := bootServiceEnvByName(cont.Env, "BOOT_SERVICE_S3_ACCESS_KEY")
		expectedS3Secret := "openchami-" + name + "-" + SuffixS3Credentials
		if s3ak.ValueFrom.SecretKeyRef.Name != expectedS3Secret {
			t.Errorf("[%s] expected S3 secret %q, got %q", name, expectedS3Secret,
				s3ak.ValueFrom.SecretKeyRef.Name)
		}

		dbhost, _ := bootServiceEnvByName(cont.Env, "BOOT_SERVICE_DBHOST")
		expectedHost := "openchami-" + name + "-postgres-rw.openchami-" + name + ".svc.cluster.local"
		if dbhost.Value != expectedHost {
			t.Errorf("[%s] expected DBHOST %q, got %q", name, expectedHost, dbhost.Value)
		}
	}
}
