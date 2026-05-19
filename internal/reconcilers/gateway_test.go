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
	cp := newControlPlane("alpha")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()

	r := &GatewayReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while awaiting certificates, got %+v", res)
	}

	gw := &gwapiv1.Gateway{}
	getErr := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp), Name: gatewayName,
	}, gw)
	if !apierrors.IsNotFound(getErr) {
		t.Errorf("expected Gateway absent before certs valid, got err=%v", getErr)
	}

	cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionGatewayReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonAwaitingCertificate {
		t.Fatalf("expected GatewayReady=False/AwaitingCertificate, got %+v", cond)
	}
}

// reconcileWithCertsValid runs the GatewayReconciler against a cluster
// that has both prerequisites met: CertificatesValid=True (gate added
// in phase 7) and tokensmith Ready=true (gate added when we discovered
// envoy's JWT filter poisons itself if SecurityPolicies are applied
// before JWKS is reachable — see ReasonAwaitingTokensmith).
//
// Tests that want to exercise the *unready-tokensmith* defer path use
// their own setup; this helper deliberately satisfies both gates so
// existing tests continue to assert against the full route + policy
// set.
func reconcileWithCertsValid(t *testing.T, cp *openchamiv1alpha1.OpenCHAMIControlPlane) client.Client {
	t.Helper()
	scheme := newScheme(t)
	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:   conditions.ConditionCertificatesValid,
		Status: metav1.ConditionTrue,
		Reason: conditions.ReasonReady,
	})
	if cp.Status.Services == nil {
		cp.Status.Services = map[string]openchamiv1alpha1.ServiceStatus{}
	}
	cp.Status.Services[ServiceTokensmith] = openchamiv1alpha1.ServiceStatus{Ready: true}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	r := &GatewayReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
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
	cp := newControlPlane("alpha")
	c := reconcileWithCertsValid(t, cp)

	gw := &gwapiv1.Gateway{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp), Name: gatewayName,
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
			assertHTTPSListener(t, l, GatewayTLSSecretName(cp))
		}
	}
}

func TestGatewayReconciler_AppliesAllHTTPRoutes(t *testing.T) {
	cp := newControlPlane("alpha")
	c := reconcileWithCertsValid(t, cp)
	for _, name := range []string{
		routeHTTPRedirect, routeSMD, routeTokensmith,
		routeBootService, routeBootAdmin,
		routeMetadataPublic, routeMetadataAdmin,
	} {
		hr := &gwapiv1.HTTPRoute{}
		if err := c.Get(context.Background(), types.NamespacedName{
			Namespace: ControlPlaneNamespace(cp), Name: name,
		}, hr); err != nil {
			t.Errorf("missing HTTPRoute %q: %v", name, err)
		}
	}
}

func TestGatewayReconciler_AppliesAllSecurityPolicies(t *testing.T) {
	cp := newControlPlane("alpha")
	c := reconcileWithCertsValid(t, cp)
	wantURI := "http://tokensmith.openchami-alpha.svc.cluster.local:8080/.well-known/jwks.json"
	for _, name := range []string{policyJWTSMD, policyJWTBoot, policyJWTBootAdmin, policyJWTMetaAdmin} {
		sp := &egv1alpha1.SecurityPolicy{}
		if err := c.Get(context.Background(), types.NamespacedName{
			Namespace: ControlPlaneNamespace(cp), Name: name,
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
	cp := newControlPlane("alpha")
	c := reconcileWithCertsValid(t, cp)

	btp := &egv1alpha1.BackendTrafficPolicy{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp), Name: policySMDRateLimit,
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
	cp := newControlPlane("alpha")
	_ = reconcileWithCertsValid(t, cp)
	cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionGatewayReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != conditions.ReasonNotProgrammed {
		t.Fatalf("expected GatewayReady=False/NotProgrammed before Envoy Gateway, got %+v", cond)
	}
}

func TestGatewayReconciler_ReadyWhenProgrammed(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:   conditions.ConditionCertificatesValid,
		Status: metav1.ConditionTrue,
		Reason: conditions.ReasonReady,
	})
	// Tokensmith Ready unblocks the JWT-gated routes / SecurityPolicies.
	// Without this, the reconciler would correctly defer them and we'd
	// be asserting on the partial state — see
	// TestGatewayReconciler_DefersJWTRoutesWhenTokensmithNotReady.
	cp.Status.Services = map[string]openchamiv1alpha1.ServiceStatus{
		ServiceTokensmith: {Ready: true},
	}

	// Pre-fabricate a Gateway with Programmed=True in its status to simulate
	// what Envoy Gateway would do once it has accepted our spec.
	preGW := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gatewayName,
			Namespace: ControlPlaneNamespace(cp),
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
		WithObjects(cp).
		WithStatusSubresource(&gwapiv1.Gateway{}).
		WithObjects(preGW).
		Build()

	r := &GatewayReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue when Programmed=True, got %+v", res)
	}

	cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionGatewayReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != conditions.ReasonReady {
		t.Fatalf("expected GatewayReady=True/Ready, got %+v", cond)
	}
	// .status.gateway must be populated when the Gateway is Programmed
	// so clients (smoke test, ochami-admin) can construct request URLs
	// without poking the cluster topology themselves. The URL scheme
	// is always https because the HTTP listener 301-redirects to HTTPS;
	// publishing http:// in status would always cost an extra round-trip.
	gw := cp.Status.Gateway
	if gw == nil {
		t.Fatalf("expected .status.gateway to be populated when Programmed=True")
	}
	wantURL := "https://" + cp.Spec.Domain
	if gw.URL != wantURL {
		t.Errorf("expected URL=%q, got %q", wantURL, gw.URL)
	}
	for _, key := range []string{
		statusRouteSMD, statusRouteBootService, statusRouteBootAdmin,
		statusRouteMetadataService, statusRouteMetadataAdmin,
		statusRouteTokensmithJWKS, statusRouteTokensmithToken,
	} {
		if _, ok := gw.Routes[key]; !ok {
			t.Errorf("expected .status.gateway.routes[%q] to be present", key)
		}
	}
	// Sanity-check the most-used path values against the HTTPRoute
	// definitions higher up in this file — these are what callers
	// actually concatenate onto URL.
	if gw.Routes[statusRouteSMD] != pathSMDPrefix {
		t.Errorf("routes[smd]: expected %q, got %q", pathSMDPrefix, gw.Routes[statusRouteSMD])
	}
	if gw.Routes[statusRouteBootService] != pathBootPrefix {
		t.Errorf("routes[boot-service]: expected %q, got %q",
			pathBootPrefix, gw.Routes[statusRouteBootService])
	}
	if gw.Routes[statusRouteTokensmithToken] != pathTokensmithToken {
		t.Errorf("routes[tokensmith-token]: expected %q, got %q",
			pathTokensmithToken, gw.Routes[statusRouteTokensmithToken])
	}
}

// TestGatewayReconciler_StatusGatewayClearedOnRegression guards a
// subtle UX bug: if Envoy Gateway flips Programmed=True back to False
// (operator restart, controller migration, GatewayClass change),
// .status.gateway must be cleared. Leaving stale URL/Routes around
// would mislead clients into hitting an endpoint that no longer
// routes — silent failures are worse than a missing field.
func TestGatewayReconciler_StatusGatewayClearedOnRegression(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:   conditions.ConditionCertificatesValid,
		Status: metav1.ConditionTrue,
		Reason: conditions.ReasonReady,
	})
	// Tokensmith Ready so we exercise the Gateway-Programmed=False
	// path, not the AwaitingTokensmith short-circuit (covered by its
	// own test below).
	cp.Status.Services = map[string]openchamiv1alpha1.ServiceStatus{
		ServiceTokensmith: {Ready: true},
	}
	// Simulate a prior successful reconcile: status.gateway populated.
	cp.Status.Gateway = &openchamiv1alpha1.GatewayStatus{
		URL:    "https://" + cp.Spec.Domain,
		Routes: map[string]string{statusRouteSMD: "/hsm"},
	}

	// Now stand up a Gateway whose status doesn't include Programmed=True
	// (e.g. Envoy Gateway just restarted and hasn't observed the spec yet).
	preGW := &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gatewayName,
			Namespace: ControlPlaneNamespace(cp),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cp).
		WithStatusSubresource(&gwapiv1.Gateway{}).
		WithObjects(preGW).
		Build()

	r := &GatewayReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cp.Status.Gateway != nil {
		t.Errorf("expected .status.gateway to be cleared when Programmed=False, "+
			"got %+v", cp.Status.Gateway)
	}
}

// TestGatewayReconciler_DefersJWTRoutesWhenTokensmithNotReady locks
// in the workaround for an envoy JWT-filter bug we hit in dev: when
// SecurityPolicies referencing `RemoteJwks.URI=<tokensmith>/.well-known/jwks.json`
// are applied BEFORE tokensmith can serve that URL, envoy's async
// JWKS fetcher fails at startup, caches the failure, and every
// JWT-gated request thereafter returns 500 with `direct_response`.
//
// The reconciler defends by deferring JWT-gated HTTPRoutes + their
// SecurityPolicies until cp.Status.Services[tokensmith].Ready == true.
// Always-safe resources (the Gateway itself, HTTP→HTTPS redirect, the
// tokensmith route that *serves* JWKS, and the unauthenticated
// metadata public route) still land in this phase — otherwise the
// chicken-and-egg never resolves.
func TestGatewayReconciler_DefersJWTRoutesWhenTokensmithNotReady(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:   conditions.ConditionCertificatesValid,
		Status: metav1.ConditionTrue,
		Reason: conditions.ReasonReady,
	})
	// Tokensmith intentionally NOT marked Ready.

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	r := &GatewayReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	res, err := r.Reconcile(context.Background(), cp)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while waiting on tokensmith, got %+v", res)
	}

	// Condition should call out the deferral by reason — not the
	// generic NotProgrammed used when waiting on envoy itself.
	cond := apimeta.FindStatusCondition(cp.Status.Conditions, conditions.ConditionGatewayReady)
	if cond == nil || cond.Status != metav1.ConditionFalse ||
		cond.Reason != conditions.ReasonAwaitingTokensmith {
		t.Errorf("expected GatewayReady=False/AwaitingTokensmith, got %+v", cond)
	}
	if cp.Status.Gateway != nil {
		t.Errorf("expected .status.gateway nil while JWT routes are deferred, got %+v", cp.Status.Gateway)
	}

	ns := ControlPlaneNamespace(cp)
	mustExist := func(kind, name string, obj client.Object) {
		t.Helper()
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, obj); err != nil {
			t.Errorf("expected %s %q to be applied during deferral, got: %v", kind, name, err)
		}
	}
	mustBeMissing := func(kind, name string, obj client.Object) {
		t.Helper()
		err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, obj)
		if !apierrors.IsNotFound(err) {
			t.Errorf("expected %s %q to be DEFERRED until tokensmith Ready, got err=%v",
				kind, name, err)
		}
	}

	// Always-safe set is present.
	mustExist("Gateway", gatewayName, &gwapiv1.Gateway{})
	mustExist("HTTPRoute", routeHTTPRedirect, &gwapiv1.HTTPRoute{})
	mustExist("HTTPRoute", routeTokensmith, &gwapiv1.HTTPRoute{})
	mustExist("HTTPRoute", routeMetadataPublic, &gwapiv1.HTTPRoute{})

	// JWT-gated set is absent.
	for _, name := range []string{routeSMD, routeBootService, routeBootAdmin, routeMetadataAdmin} {
		mustBeMissing("HTTPRoute", name, &gwapiv1.HTTPRoute{})
	}
	for _, name := range []string{policyJWTSMD, policyJWTBoot, policyJWTBootAdmin, policyJWTMetaAdmin} {
		mustBeMissing("SecurityPolicy", name, &egv1alpha1.SecurityPolicy{})
	}
}

func TestGatewayReconciler_TwoClustersIsolated(t *testing.T) {
	scheme := newScheme(t)
	rec := record.NewFakeRecorder(20)

	for _, name := range []string{testControlPlaneRed, testControlPlaneBlue} {
		cp := newControlPlane(name)
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:   conditions.ConditionCertificatesValid,
			Status: metav1.ConditionTrue,
			Reason: conditions.ReasonReady,
		})
		// Tokensmith Ready so the JWT SecurityPolicies (asserted on
		// below) actually get applied per cluster.
		cp.Status.Services = map[string]openchamiv1alpha1.ServiceStatus{
			ServiceTokensmith: {Ready: true},
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
		r := &GatewayReconciler{Client: c, Recorder: rec}
		if _, err := r.Reconcile(context.Background(), cp); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}

		gw := &gwapiv1.Gateway{}
		if err := c.Get(context.Background(), types.NamespacedName{
			Namespace: ControlPlaneNamespace(cp), Name: gatewayName,
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
			Namespace: ControlPlaneNamespace(cp), Name: policyJWTSMD,
		}, sp); err != nil {
			t.Fatalf("getting cluster %s SecurityPolicy: %v", name, err)
		}
		wantURI := "http://tokensmith.openchami-" + name + ".svc.cluster.local:8080/.well-known/jwks.json"
		if got := sp.Spec.JWT.Providers[0].RemoteJWKS.URI; got != wantURI {
			t.Errorf("cluster %s JWKS URI=%q want %q", name, got, wantURI)
		}
	}
}
