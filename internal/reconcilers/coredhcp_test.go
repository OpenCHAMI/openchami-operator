// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
)

func TestCoreDHCPReconciler_DisabledSkips(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.CoreDHCP.Enabled = false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &CoreDHCPReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when disabled, got %+v", res)
	}

	ds := &appsv1.DaemonSet{}
	getErr := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      ServiceCoreDHCP,
	}, ds)
	if !apierrors.IsNotFound(getErr) {
		t.Errorf("expected coredhcp DaemonSet to be absent when disabled, got err=%v", getErr)
	}
}

func TestCoreDHCPReconciler_WaitsForProbe(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.CoreDHCP.Enabled = true
	cp.Spec.NetworkProbe.Enabled = true
	// Pre-set NetworkProbeReady=False on the cluster.
	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:    conditions.ConditionNetworkProbeReady,
		Status:  metav1.ConditionFalse,
		Reason:  conditions.ReasonProvisioning,
		Message: "probe still spinning up",
	})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	r := &CoreDHCPReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while waiting for probe, got %+v", res)
	}

	ds := &appsv1.DaemonSet{}
	getErr := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      ServiceCoreDHCP,
	}, ds)
	if !apierrors.IsNotFound(getErr) {
		t.Errorf("expected coredhcp DaemonSet to be absent while waiting for probe, got err=%v", getErr)
	}

	cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionDHCPReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonWaitingForProbe {
		t.Fatalf("expected DHCPReady=False/WaitingForNetworkProbe, got %+v", cond)
	}
}

// assertCoreDHCPRunsAsRoot verifies both the container- and pod-level security
// contexts force coredhcp to run as root (UID 0). See issue #21: the coresmd
// image is not setcap'd, so the added network caps only survive as root.
func assertCoreDHCPRunsAsRoot(t *testing.T, ds *appsv1.DaemonSet, container corev1.Container) {
	t.Helper()
	if container.SecurityContext.RunAsNonRoot == nil || *container.SecurityContext.RunAsNonRoot {
		t.Errorf("expected container runAsNonRoot=false, got %+v",
			container.SecurityContext.RunAsNonRoot)
	}
	if container.SecurityContext.RunAsUser == nil || *container.SecurityContext.RunAsUser != 0 {
		t.Errorf("expected container runAsUser=0, got %+v",
			container.SecurityContext.RunAsUser)
	}
	podSC := ds.Spec.Template.Spec.SecurityContext
	if podSC == nil {
		t.Fatalf("expected pod securityContext")
	}
	if podSC.RunAsNonRoot == nil || *podSC.RunAsNonRoot {
		t.Errorf("expected pod runAsNonRoot=false, got %+v", podSC.RunAsNonRoot)
	}
	if podSC.RunAsUser == nil || *podSC.RunAsUser != 0 {
		t.Errorf("expected pod runAsUser=0, got %+v", podSC.RunAsUser)
	}
}

func TestCoreDHCPReconciler_AppliesDaemonSet(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.CoreDHCP = openchamiv1alpha1.CoreDHCPSpec{
		Enabled:      true,
		NodeSelector: map[string]string{testNodeRoleKey: testNodeRoleDHCP},
		LeaseRanges: []openchamiv1alpha1.DHCPLeaseRange{{
			Subnet: testProvisionSubnet,
			Start:  testLeaseRangeStartLarge,
			End:    testLeaseRangeEndLarge,
		}},
		UnknownLeaseDuration: "5m",
		KnownLeaseDuration:   "1h",
	}
	cp.Spec.NetworkProbe.Enabled = false

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	r := &CoreDHCPReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ds := &appsv1.DaemonSet{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      ServiceCoreDHCP,
	}, ds); err != nil {
		t.Fatalf("getting coredhcp DaemonSet: %v", err)
	}

	if got := ds.Spec.Template.Spec.NodeSelector; got[testNodeRoleKey] != testNodeRoleDHCP {
		t.Errorf("expected nodeSelector node-role=dhcp, got %+v", got)
	}
	if !ds.Spec.Template.Spec.HostNetwork {
		t.Errorf("expected HostNetwork=true")
	}
	if ds.Spec.Template.Spec.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
		t.Errorf("expected DNSPolicy=ClusterFirstWithHostNet, got %s", ds.Spec.Template.Spec.DNSPolicy)
	}
	if ds.Spec.Template.Spec.PriorityClassName != testPriorityClass {
		t.Errorf("expected priority=system-node-critical, got %q", ds.Spec.Template.Spec.PriorityClassName)
	}
	if ds.Spec.Template.Spec.ServiceAccountName != ServiceCoreDHCP {
		t.Errorf("expected SA=%q, got %q", ServiceCoreDHCP, ds.Spec.Template.Spec.ServiceAccountName)
	}

	if len(ds.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(ds.Spec.Template.Spec.Containers))
	}
	container := ds.Spec.Template.Spec.Containers[0]
	if container.SecurityContext == nil || container.SecurityContext.Capabilities == nil {
		t.Fatalf("expected container securityContext with capabilities")
	}
	for _, want := range []string{"NET_BIND_SERVICE", "NET_RAW", "NET_ADMIN"} {
		if !slices.Contains(container.SecurityContext.Capabilities.Add, corev1.Capability(want)) {
			t.Errorf("expected %s in caps.add, got %+v",
				want, container.SecurityContext.Capabilities.Add)
		}
	}
	if !slices.Contains(container.SecurityContext.Capabilities.Drop, "ALL") {
		t.Errorf("expected ALL in caps.drop, got %+v",
			container.SecurityContext.Capabilities.Drop)
	}
	// coredhcp must run as root (UID 0) so the added network caps survive the
	// exec; the coresmd image is not setcap'd (see issue #21).
	assertCoreDHCPRunsAsRoot(t, ds, container)
	ports := map[string]corev1.ContainerPort{}
	for _, p := range container.Ports {
		ports[p.Name] = p
	}
	for name, want := range map[string]int32{coreDHCPPortName: 67, coreDHCPTFTPPortName: 69} {
		p, ok := ports[name]
		if !ok || p.ContainerPort != want || p.HostPort != want || p.Protocol != corev1.ProtocolUDP {
			t.Errorf("expected UDP port %s=%d with matching hostPort, got %+v", name, want, container.Ports)
		}
	}

	// The unconsumed generator env vars must be gone; NODE_NAME stays.
	envs := map[string]corev1.EnvVar{}
	for _, e := range container.Env {
		envs[e.Name] = e
	}
	for _, gone := range []string{"LEASE_RANGES_JSON", "UNKNOWN_LEASE_DURATION", "KNOWN_LEASE_DURATION", "CLUSTER_NAME"} {
		if _, ok := envs[gone]; ok {
			t.Errorf("unexpected env %s on coredhcp container", gone)
		}
	}
	if _, ok := envs["NODE_NAME"]; !ok {
		t.Errorf("expected NODE_NAME env")
	}

	// Gateway TLS ca.crt mounted at /root_ca/root_ca.crt, optional.
	assertGatewayCAMounted(t, cp, ds, container)

	if ds.Spec.Template.Annotations[coreDHCPConfigHashAnnotation] == "" {
		t.Errorf("expected %s annotation on pod template", coreDHCPConfigHashAnnotation)
	}

	if container.Lifecycle == nil || container.Lifecycle.PreStop == nil {
		t.Errorf("expected PreStop lifecycle hook")
	}
}

func TestCoreDHCPReconciler_ReadyWhenAvailable(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.CoreDHCP = openchamiv1alpha1.CoreDHCPSpec{
		Enabled:      true,
		NodeSelector: map[string]string{testNodeRoleKey: testNodeRoleDHCP},
		LeaseRanges: []openchamiv1alpha1.DHCPLeaseRange{{
			Subnet: testProvisionSubnet,
			Start:  testLeaseRangeStartLarge,
			End:    testLeaseRangeEndLarge,
		}},
	}
	cp.Spec.NetworkProbe.Enabled = false

	existingDS := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceCoreDHCP,
			Namespace: ControlPlaneNamespace(cp),
		},
		Status: appsv1.DaemonSetStatus{NumberReady: 1},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cp).
		WithStatusSubresource(&appsv1.DaemonSet{}).
		WithObjects(existingDS).
		Build()
	r := &CoreDHCPReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when ready, got %+v", res)
	}

	cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionDHCPReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected DHCPReady=True, got %+v", cond)
	}
}

func TestCoreDHCPReconciler_UsesProbeNodeSelector(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.Services.CoreDHCP.Enabled = true
	cp.Spec.Services.CoreDHCP.LeaseRanges = []openchamiv1alpha1.DHCPLeaseRange{{
		Subnet: testProvisionSubnet,
		Start:  testLeaseRangeStartLarge,
		End:    testLeaseRangeEndLarge,
	}}
	cp.Spec.NetworkProbe.Enabled = true
	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:   conditions.ConditionNetworkProbeReady,
		Status: metav1.ConditionTrue,
		Reason: conditions.ReasonReady,
	})

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	r := &CoreDHCPReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ds := &appsv1.DaemonSet{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp),
		Name:      ServiceCoreDHCP,
	}, ds); err != nil {
		t.Fatalf("getting coredhcp DaemonSet: %v", err)
	}

	wantKey := fmt.Sprintf(probeNetworkReadyLabelFmt, "alpha", probeTypeProvision)
	got := ds.Spec.Template.Spec.NodeSelector
	if got[wantKey] != testProbeLabelTrue {
		t.Errorf("expected nodeSelector %s=true, got %+v", wantKey, got)
	}
}

const (
	testUserDHCPConfigMap = "site-coredhcp-config"
	testUserDHCPConfig    = "server4:\n  listen:\n    - \"%eno1\"\n  plugins:\n    - server_id: 172.16.0.254\n"
)

// assertGatewayCAMounted verifies the gateway TLS Secret's ca.crt is
// projected (optionally) to /root_ca/root_ca.crt for coresmd's ca_cert=.
func assertGatewayCAMounted(t *testing.T, cp *openchamiv1alpha1.OpenCHAMIControlPlane, ds *appsv1.DaemonSet, container corev1.Container) {
	t.Helper()
	var vol *corev1.Volume
	for i := range ds.Spec.Template.Spec.Volumes {
		if ds.Spec.Template.Spec.Volumes[i].Name == coreDHCPCAVolume {
			vol = &ds.Spec.Template.Spec.Volumes[i]
		}
	}
	if vol == nil || vol.Secret == nil {
		t.Fatalf("expected %s Secret volume, got %+v", coreDHCPCAVolume, ds.Spec.Template.Spec.Volumes)
	}
	if vol.Secret.SecretName != GatewayTLSSecretName(cp) {
		t.Errorf("CA volume secretName=%q want %q", vol.Secret.SecretName, GatewayTLSSecretName(cp))
	}
	if vol.Secret.Optional == nil || !*vol.Secret.Optional {
		t.Errorf("CA volume must be optional (ca.crt absent for ACME issuers)")
	}
	if len(vol.Secret.Items) != 1 || vol.Secret.Items[0].Key != "ca.crt" || vol.Secret.Items[0].Path != "root_ca.crt" {
		t.Errorf("CA volume items=%+v, want ca.crt -> root_ca.crt", vol.Secret.Items)
	}
	found := false
	for _, m := range container.VolumeMounts {
		if m.Name == coreDHCPCAVolume {
			found = true
			if m.MountPath != "/root_ca" || !m.ReadOnly {
				t.Errorf("CA mount=%+v, want read-only /root_ca", m)
			}
		}
	}
	if !found {
		t.Errorf("expected %s volumeMount", coreDHCPCAVolume)
	}
}

// newConfigMapRefControlPlane returns a probe-less control plane whose
// CoreDHCP config comes from a user ConfigMap in the CR's namespace.
func newConfigMapRefControlPlane() *openchamiv1alpha1.OpenCHAMIControlPlane {
	cp := newControlPlane("alpha")
	cp.Spec.NetworkProbe.Enabled = false
	cp.Spec.Services.CoreDHCP = openchamiv1alpha1.CoreDHCPSpec{
		Enabled:      true,
		NodeSelector: map[string]string{testNodeRoleKey: testNodeRoleDHCP},
		ConfigMapRef: &openchamiv1alpha1.CoreDHCPConfigMapRef{Name: testUserDHCPConfigMap},
	}
	return cp
}

func userDHCPConfigMap(cp *openchamiv1alpha1.OpenCHAMIControlPlane, key, data string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testUserDHCPConfigMap, Namespace: cp.Namespace},
		Data:       map[string]string{key: data},
	}
}

func getMirroredDHCPConfig(t *testing.T, c client.Client, cp *openchamiv1alpha1.OpenCHAMIControlPlane) *corev1.ConfigMap {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp), Name: coreDHCPConfigMapName,
	}, cm); err != nil {
		t.Fatalf("getting mirrored coredhcp ConfigMap: %v", err)
	}
	return cm
}

func getDHCPDaemonSet(t *testing.T, c client.Client, cp *openchamiv1alpha1.OpenCHAMIControlPlane) *appsv1.DaemonSet {
	t.Helper()
	ds := &appsv1.DaemonSet{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp), Name: ServiceCoreDHCP,
	}, ds); err != nil {
		t.Fatalf("getting coredhcp DaemonSet: %v", err)
	}
	return ds
}

func TestCoreDHCPReconciler_ConfigMapRefMirrorsUserConfig(t *testing.T) {
	scheme := newScheme(t)
	cp := newConfigMapRefControlPlane()
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cp, userDHCPConfigMap(cp, "config.yml", testUserDHCPConfig)).Build()
	r := &CoreDHCPReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cm := getMirroredDHCPConfig(t, c, cp)
	if got := cm.Data[coreDHCPConfigKey]; got != testUserDHCPConfig {
		t.Errorf("mirrored config=%q, want user content verbatim %q", got, testUserDHCPConfig)
	}
	wantSource := "default/" + testUserDHCPConfigMap + ":config.yml"
	if got := cm.Annotations[coreDHCPConfigSourceAnnotation]; got != wantSource {
		t.Errorf("source annotation=%q want %q", got, wantSource)
	}

	ds := getDHCPDaemonSet(t, c, cp)
	if got, want := ds.Spec.Template.Annotations[coreDHCPConfigHashAnnotation], coreDHCPConfigHash(testUserDHCPConfig); got != want {
		t.Errorf("config hash=%q want %q", got, want)
	}
	// The DaemonSet still mounts the operator-managed ConfigMap, not the
	// user's one (which lives in a different namespace).
	for _, v := range ds.Spec.Template.Spec.Volumes {
		if v.Name == coreDHCPConfigVolume && (v.ConfigMap == nil || v.ConfigMap.Name != coreDHCPConfigMapName) {
			t.Errorf("config volume must reference %q, got %+v", coreDHCPConfigMapName, v)
		}
	}
}

func TestCoreDHCPReconciler_ConfigMapRefCustomKey(t *testing.T) {
	scheme := newScheme(t)
	cp := newConfigMapRefControlPlane()
	cp.Spec.Services.CoreDHCP.ConfigMapRef.Key = "coredhcp.yaml"
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cp, userDHCPConfigMap(cp, "coredhcp.yaml", testUserDHCPConfig)).Build()
	r := &CoreDHCPReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Always mounted as config.yml regardless of the source key.
	if got := getMirroredDHCPConfig(t, c, cp).Data[coreDHCPConfigKey]; got != testUserDHCPConfig {
		t.Errorf("mirrored config=%q", got)
	}
}

func TestCoreDHCPReconciler_ConfigMapRefMissing(t *testing.T) {
	cases := map[string][]client.Object{
		"configmap absent": nil,
		"key absent":       {userDHCPConfigMap(newConfigMapRefControlPlane(), "other.yml", testUserDHCPConfig)},
		"key empty":        {userDHCPConfigMap(newConfigMapRefControlPlane(), "config.yml", "")},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			scheme := newScheme(t)
			cp := newConfigMapRefControlPlane()
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).WithObjects(extra...).Build()
			rec := record.NewFakeRecorder(10)
			r := &CoreDHCPReconciler{Client: c, Recorder: rec}
			res, err := r.Reconcile(context.Background(), cp)
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if res.RequeueAfter == 0 {
				t.Errorf("expected requeue while ConfigMap is missing")
			}
			cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionDHCPReady)
			if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonConfigMapNotFound {
				t.Fatalf("expected DHCPReady=False/ConfigMapNotFound, got %+v", cond)
			}
			select {
			case ev := <-rec.Events:
				if !strings.Contains(ev, conditions.ReasonConfigMapNotFound) || !strings.Contains(ev, "Runbook:") {
					t.Errorf("unexpected event %q", ev)
				}
			default:
				t.Errorf("expected a warning event")
			}
			err = c.Get(context.Background(), types.NamespacedName{
				Namespace: ControlPlaneNamespace(cp), Name: ServiceCoreDHCP,
			}, &appsv1.DaemonSet{})
			if !apierrors.IsNotFound(err) {
				t.Errorf("DaemonSet must not be applied without a config, got err=%v", err)
			}
		})
	}
}

// A config edit must change the pod-template hash so the DaemonSet rolls:
// coredhcp only reads its config at startup.
func TestCoreDHCPReconciler_ConfigChangeRollsPods(t *testing.T) {
	scheme := newScheme(t)
	cp := newConfigMapRefControlPlane()
	user := userDHCPConfigMap(cp, "config.yml", testUserDHCPConfig)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp, user).Build()
	r := &CoreDHCPReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	before := getDHCPDaemonSet(t, c, cp).Spec.Template.Annotations[coreDHCPConfigHashAnnotation]

	// Idempotent: same content, same hash.
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if again := getDHCPDaemonSet(t, c, cp).Spec.Template.Annotations[coreDHCPConfigHashAnnotation]; again != before {
		t.Errorf("hash changed without a config change: %q -> %q", before, again)
	}

	updated := testUserDHCPConfig + "    - dns: 172.16.0.254\n"
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(user), user); err != nil {
		t.Fatalf("get user cm: %v", err)
	}
	user.Data["config.yml"] = updated
	if err := c.Update(context.Background(), user); err != nil {
		t.Fatalf("update user cm: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	after := getDHCPDaemonSet(t, c, cp).Spec.Template.Annotations[coreDHCPConfigHashAnnotation]
	if after == before {
		t.Errorf("expected config hash to change after user config edit")
	}
	if got := getMirroredDHCPConfig(t, c, cp).Data[coreDHCPConfigKey]; got != updated {
		t.Errorf("mirrored config not updated: %q", got)
	}
}

func TestCoreDHCPReconciler_GeneratedModeWithoutLeaseRanges(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	cp.Spec.NetworkProbe.Enabled = false
	cp.Spec.Services.CoreDHCP = openchamiv1alpha1.CoreDHCPSpec{
		Enabled:      true,
		NodeSelector: map[string]string{testNodeRoleKey: testNodeRoleDHCP},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	r := &CoreDHCPReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile should not hard-fail on a spec problem: %v", err)
	}
	cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionDHCPReady)
	if cond == nil || cond.Reason != conditions.ReasonInvalidConfig {
		t.Fatalf("expected DHCPReady reason InvalidConfig, got %+v", cond)
	}
}

func TestCoreDHCPReconciler_DescribeWithConfigMapRef(t *testing.T) {
	cp := newConfigMapRefControlPlane()
	objs, err := (&CoreDHCPReconciler{}).Describe(cp)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("expected ConfigMap + DaemonSet, got %d objects", len(objs))
	}
	cm, ok := objs[0].(*corev1.ConfigMap)
	if !ok {
		t.Fatalf("expected first object ConfigMap, got %T", objs[0])
	}
	if !strings.Contains(cm.Annotations[coreDHCPConfigSourceAnnotation], testUserDHCPConfigMap) {
		t.Errorf("describe ConfigMap should name the source, got %+v", cm.Annotations)
	}
}
