// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"testing"

	egv1alpha1 "github.com/envoyproxy/gateway/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
)

func TestGatewayReconciler_AwaitsCertificates(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	r := &GatewayReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while awaiting certificates, got %+v", res)
	}

	gw := &gwapiv1.Gateway{}
	getErr := c.Get(context.Background(), types.NamespacedName{
		Namespace: ClusterNamespace(cluster), Name: gatewayName,
	}, gw)
	if !apierrors.IsNotFound(getErr) {
		t.Errorf("expected Gateway absent before certs valid, got err=%v", getErr)
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionGatewayReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonAwaitingCertificate {
		t.Fatalf("expected GatewayReady=False/AwaitingCertificate, got %+v", cond)
	}
}

// reconcileWithCertsValid runs the GatewayReconciler against a cluster that
// has CertificatesValid=True pre-set. It returns the resulting fake client so
// individual tests can assert on the produced objects.
func reconcileWithCertsValid(t *testing.T, cluster *openchamiv1alpha1.OpenCHAMICluster) client.Client {
	t.Helper()
	scheme := newScheme(t)
	apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:   conditions.ConditionCertificatesValid,
		Status: metav1.ConditionTrue,
		Reason: conditions.ReasonReady,
	})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
	r := &GatewayReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cluster); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return c
}

func assertHTTPListener(t *testing.T, l gwapiv1.Listener) {
	t.Helper()
	if l.Port != 80 {
		t.Errorf("http listener port=%d want 80", l.Port)
	}
}

func assertHTTPSListener(t *testing.T, l gwapiv1.Listener, wantSecret string) {
	t.Helper()
	if l.Port != 443 {
		t.Errorf("https listener port=%d want 443", l.Port)
	}
	if l.TLS == nil || len(l.TLS.CertificateRefs) != 1 {
		t.Errorf("https listener missing TLS cert ref")
		return
	}
	if string(l.TLS.CertificateRefs[0].Name) != wantSecret {
		t.Errorf("https cert ref=%q want %q",
			l.TLS.CertificateRefs[0].Name, wantSecret)
	}
}

func TestGatewayReconciler_AppliesGatewayAndListeners(t *testing.T) {
	cluster := newCluster("alpha")
	c := reconcileWithCertsValid(t, cluster)

	gw := &gwapiv1.Gateway{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ClusterNamespace(cluster), Name: gatewayName,
	}, gw); err != nil {
		t.Fatalf("getting Gateway: %v", err)
	}
	if string(gw.Spec.GatewayClassName) != "envoy" {
		t.Errorf("GatewayClassName=%q want envoy", gw.Spec.GatewayClassName)
	}
	if len(gw.Spec.Listeners) != 2 {
		t.Fatalf("expected 2 listeners (http+https), got %d", len(gw.Spec.Listeners))
	}
	for _, l := range gw.Spec.Listeners {
		switch l.Protocol {
		case gwapiv1.HTTPProtocolType:
			assertHTTPListener(t, l)
		case gwapiv1.HTTPSProtocolType:
			assertHTTPSListener(t, l, GatewayTLSSecretName(cluster))
		}
	}
}

func TestGatewayReconciler_AppliesAllHTTPRoutes(t *testing.T) {
	cluster := newCluster("alpha")
	c := reconcileWithCertsValid(t, cluster)
	for _, name := range []string{
		routeHTTPRedirect, routeSMD, routeTokensmith,
		routeBootService, routeMetadataPublic, routeMetadataAdmin,
	} {
		hr := &gwapiv1.HTTPRoute{}
		if err := c.Get(context.Background(), types.NamespacedName{
			Namespace: ClusterNamespace(cluster), Name: name,
		}, hr); err != nil {
			t.Errorf("missing HTTPRoute %q: %v", name, err)
		}
	}
}

func TestGatewayReconciler_AppliesAllSecurityPolicies(t *testing.T) {
	cluster := newCluster("alpha")
	c := reconcileWithCertsValid(t, cluster)
	wantURI := "http://tokensmith.openchami-alpha.svc.cluster.local:8080/.well-known/jwks.json"
	for _, name := range []string{policyJWTSMD, policyJWTBoot, policyJWTMetaAdmin} {
		sp := &egv1alpha1.SecurityPolicy{}
		if err := c.Get(context.Background(), types.NamespacedName{
			Namespace: ClusterNamespace(cluster), Name: name,
		}, sp); err != nil {
			t.Fatalf("missing SecurityPolicy %q: %v", name, err)
		}
		if sp.Spec.JWT == nil || len(sp.Spec.JWT.Providers) != 1 {
			t.Fatalf("SecurityPolicy %q missing JWT provider", name)
		}
		if got := sp.Spec.JWT.Providers[0].RemoteJWKS.URI; got != wantURI {
			t.Errorf("SecurityPolicy %q JWKS URI=%q want %q", name, got, wantURI)
		}
	}
}

func TestGatewayReconciler_AppliesSMDRateLimit(t *testing.T) {
	cluster := newCluster("alpha")
	c := reconcileWithCertsValid(t, cluster)

	btp := &egv1alpha1.BackendTrafficPolicy{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ClusterNamespace(cluster), Name: policySMDRateLimit,
	}, btp); err != nil {
		t.Fatalf("missing BackendTrafficPolicy %q: %v", policySMDRateLimit, err)
	}
	if btp.Spec.RateLimit == nil || btp.Spec.RateLimit.Global == nil ||
		len(btp.Spec.RateLimit.Global.Rules) != 1 {
		t.Fatalf("BackendTrafficPolicy missing global rate-limit rule")
	}
	rule := btp.Spec.RateLimit.Global.Rules[0]
	if rule.Limit.Requests != rateLimitRequests || rule.Limit.Unit != egv1alpha1.RateLimitUnitMinute {
		t.Errorf("rate-limit value=%+v want %d/Minute", rule.Limit, rateLimitRequests)
	}
	if len(rule.ClientSelectors) != 1 || len(rule.ClientSelectors[0].Headers) != 1 ||
		rule.ClientSelectors[0].Headers[0].Name != rateLimitHeaderName {
		t.Errorf("rate-limit selectors=%+v want header %s", rule.ClientSelectors, rateLimitHeaderName)
	}
}

func TestGatewayReconciler_NotProgrammedWithoutEnvoyGateway(t *testing.T) {
	cluster := newCluster("alpha")
	_ = reconcileWithCertsValid(t, cluster)
	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionGatewayReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonNotProgrammed {
		t.Fatalf("expected GatewayReady=False/NotProgrammed before Envoy Gateway, got %+v", cond)
	}
}

func TestGatewayReconciler_ReadyWhenProgrammed(t *testing.T) {
	scheme := newScheme(t)
	cluster := newCluster("alpha")
	apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:   conditions.ConditionCertificatesValid,
		Status: metav1.ConditionTrue,
		Reason: conditions.ReasonReady,
	})

	// Pre-fabricate a Gateway with Programmed=True in its status to simulate
	// what Envoy Gateway would do once it has accepted our spec.
	preGW := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gatewayName,
			Namespace: ClusterNamespace(cluster),
		},
		Status: gwapiv1.GatewayStatus{
			Conditions: []metav1.Condition{{
				Type:               string(gwapiv1.GatewayConditionProgrammed),
				Status:             metav1.ConditionTrue,
				Reason:             "Programmed",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&gwapiv1.Gateway{}).
		WithObjects(preGW).
		Build()

	r := &GatewayReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cluster)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when Programmed=True, got %+v", res)
	}

	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, conditions.ConditionGatewayReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != conditions.ReasonReady {
		t.Fatalf("expected GatewayReady=True/Ready, got %+v", cond)
	}
}

func TestGatewayReconciler_TwoClustersIsolated(t *testing.T) {
	scheme := newScheme(t)
	rec := record.NewFakeRecorder(20)

	for _, name := range []string{testClusterRed, testClusterBlue} {
		cluster := newCluster(name)
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:   conditions.ConditionCertificatesValid,
			Status: metav1.ConditionTrue,
			Reason: conditions.ReasonReady,
		})
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()
		r := &GatewayReconciler{Client: c, Recorder: rec}
		if _, err := r.Reconcile(context.Background(), cluster); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}

		gw := &gwapiv1.Gateway{}
		if err := c.Get(context.Background(), types.NamespacedName{
			Namespace: ClusterNamespace(cluster), Name: gatewayName,
		}, gw); err != nil {
			t.Fatalf("getting cluster %s gateway: %v", name, err)
		}
		if gw.Namespace != "openchami-"+name {
			t.Errorf("cluster %s gateway in namespace %q want openchami-%s",
				name, gw.Namespace, name)
		}
		if len(gw.Spec.Listeners) == 0 {
			t.Fatalf("cluster %s gateway has no listeners", name)
		}
		if hn := gw.Spec.Listeners[0].Hostname; hn == nil || string(*hn) != name+".test.local" {
			t.Errorf("cluster %s hostname=%v want %s.test.local", name, hn, name)
		}

		// Each SecurityPolicy points at this cluster's own tokensmith service.
		sp := &egv1alpha1.SecurityPolicy{}
		if err := c.Get(context.Background(), types.NamespacedName{
			Namespace: ClusterNamespace(cluster), Name: policyJWTSMD,
		}, sp); err != nil {
			t.Fatalf("getting cluster %s SecurityPolicy: %v", name, err)
		}
		wantURI := "http://tokensmith.openchami-" + name + ".svc.cluster.local:8080/.well-known/jwks.json"
		if got := sp.Spec.JWT.Providers[0].RemoteJWKS.URI; got != wantURI {
			t.Errorf("cluster %s JWKS URI=%q want %q", name, got, wantURI)
		}
	}
}
