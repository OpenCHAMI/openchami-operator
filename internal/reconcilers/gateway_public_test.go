// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"reflect"
	"slices"
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

const (
	testPathSMDComponents = "/hsm/v2/State/Components"
	testPathBootScript    = "/boot/v1/bootscript"
	testPathA             = "/hsm/a"
	testPathB             = "/hsm/b"
	testPathX             = "/hsm/x"
)

func getHTTPRoute(t *testing.T, c client.Client, cp *openchamiv1alpha1.OpenCHAMIControlPlane, name string) *gwapiv1.HTTPRoute {
	t.Helper()
	hr := &gwapiv1.HTTPRoute{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp), Name: name,
	}, hr); err != nil {
		t.Fatalf("getting HTTPRoute %q: %v", name, err)
	}
	return hr
}

func assertHTTPRouteAbsent(t *testing.T, c client.Client, cp *openchamiv1alpha1.OpenCHAMIControlPlane, name string) {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp), Name: name,
	}, &gwapiv1.HTTPRoute{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected HTTPRoute %q absent, got err=%v", name, err)
	}
}

// publicRoutePaths asserts the structural security contract of a public
// route — single rule, every match Exact + GET, single backend — and
// returns the matched paths.
func publicRoutePaths(t *testing.T, hr *gwapiv1.HTTPRoute, wantBackend string, wantPort int32) []string {
	t.Helper()
	if len(hr.Spec.Rules) != 1 {
		t.Fatalf("route %q: want exactly 1 rule, got %d", hr.Name, len(hr.Spec.Rules))
	}
	rule := hr.Spec.Rules[0]
	if len(rule.Filters) != 0 {
		t.Errorf("route %q: public route must not rewrite/redirect, got filters %+v", hr.Name, rule.Filters)
	}
	if len(rule.BackendRefs) != 1 {
		t.Fatalf("route %q: want 1 backendRef, got %d", hr.Name, len(rule.BackendRefs))
	}
	ref := rule.BackendRefs[0]
	if string(ref.Name) != wantBackend || ref.Port == nil || *ref.Port != wantPort {
		t.Errorf("route %q: backend=%s:%v want %s:%d", hr.Name, ref.Name, ref.Port, wantBackend, wantPort)
	}
	if len(hr.Spec.ParentRefs) != 1 || hr.Spec.ParentRefs[0].SectionName == nil ||
		string(*hr.Spec.ParentRefs[0].SectionName) != listenerHTTPS {
		t.Errorf("route %q: must attach only to the HTTPS listener, got %+v", hr.Name, hr.Spec.ParentRefs)
	}
	var paths []string
	for i, m := range rule.Matches {
		if m.Method == nil || *m.Method != gwapiv1.HTTPMethodGet {
			t.Errorf("route %q match[%d]: method=%v, public routes must be GET-only", hr.Name, i, m.Method)
		}
		if m.Path == nil || m.Path.Type == nil || *m.Path.Type != gwapiv1.PathMatchExact || m.Path.Value == nil {
			t.Errorf("route %q match[%d]: path=%+v, public routes must use Exact matches", hr.Name, i, m.Path)
			continue
		}
		if len(m.Headers) != 0 || len(m.QueryParams) != 0 {
			t.Errorf("route %q match[%d]: unexpected header/query matchers", hr.Name, i)
		}
		paths = append(paths, *m.Path.Value)
	}
	return paths
}

func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

func TestGatewayReconciler_SMDPublicRouteDefaults(t *testing.T) {
	cp := newControlPlane("alpha")
	c := reconcileWithCertsValid(t, cp)

	got := publicRoutePaths(t, getHTTPRoute(t, c, cp, routeSMDPublic), ServiceSMD, smdPort)
	if want := sortedCopy(defaultSMDPublicPaths); !reflect.DeepEqual(got, want) {
		t.Errorf("smd-public paths=%v want %v", got, want)
	}
	// The issue #65 reproducer must be covered.
	if !slices.Contains(got, testPathSMDComponents) {
		t.Errorf("smd-public must include GET /hsm/v2/State/Components (issue #65)")
	}

	// The protected prefix route is untouched and still JWT-gated.
	smd := getHTTPRoute(t, c, cp, routeSMD)
	if len(smd.Spec.Rules) != 1 || len(smd.Spec.Rules[0].Matches) != 1 ||
		*smd.Spec.Rules[0].Matches[0].Path.Type != gwapiv1.PathMatchPathPrefix ||
		*smd.Spec.Rules[0].Matches[0].Path.Value != pathSMDPrefix {
		t.Errorf("smd protected route changed unexpectedly: %+v", smd.Spec.Rules)
	}
}

func TestGatewayReconciler_BootPublicRouteDefaults(t *testing.T) {
	cp := newControlPlane("alpha")
	c := reconcileWithCertsValid(t, cp)

	got := publicRoutePaths(t, getHTTPRoute(t, c, cp, routeBootPublic), ServiceBootService, bootServicePort)
	if want := sortedCopy(defaultBootPublicPaths); !reflect.DeepEqual(got, want) {
		t.Errorf("boot-service-public paths=%v want %v", got, want)
	}
}

// No SecurityPolicy may target a public route — that's the whole point —
// and the JWT policies must still target the protected routes.
func TestGatewayReconciler_PublicRoutesHaveNoSecurityPolicy(t *testing.T) {
	cp := newControlPlane("alpha")
	c := reconcileWithCertsValid(t, cp)

	list := &egv1alpha1.SecurityPolicyList{}
	if err := c.List(context.Background(), list, client.InNamespace(ControlPlaneNamespace(cp))); err != nil {
		t.Fatalf("listing SecurityPolicies: %v", err)
	}
	targets := map[string]string{}
	for _, sp := range list.Items {
		for _, ref := range sp.Spec.TargetRefs {
			targets[string(ref.Name)] = sp.Name
		}
	}
	for _, public := range []string{routeSMDPublic, routeBootPublic} {
		if sp, ok := targets[public]; ok {
			t.Errorf("SecurityPolicy %q targets public route %q", sp, public)
		}
	}
	for route, policy := range map[string]string{
		routeSMD: policyJWTSMD, routeBootService: policyJWTBoot,
	} {
		if targets[route] != policy {
			t.Errorf("protected route %q targeted by %q, want %q", route, targets[route], policy)
		}
	}
}

func TestGatewayReconciler_PublicRoutesCustomPaths(t *testing.T) {
	cp := newControlPlane("alpha")
	cp.Spec.Services.SMD.PublicRoutes.Paths = []string{
		"/hsm/v2/service/ready",
		testPathSMDComponents,
		testPathSMDComponents, // duplicate — must collapse
		testPathBootScript,    // wrong prefix — must be dropped
	}
	cp.Spec.Services.BootService.PublicRoutes.Paths = []string{testPathBootScript}
	c := reconcileWithCertsValid(t, cp)

	got := publicRoutePaths(t, getHTTPRoute(t, c, cp, routeSMDPublic), ServiceSMD, smdPort)
	want := []string{testPathSMDComponents, "/hsm/v2/service/ready"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("smd-public custom paths=%v want %v", got, want)
	}
	got = publicRoutePaths(t, getHTTPRoute(t, c, cp, routeBootPublic), ServiceBootService, bootServicePort)
	if !reflect.DeepEqual(got, []string{testPathBootScript}) {
		t.Errorf("boot-service-public custom paths=%v", got)
	}
}

// Public routes carry no JWT, so they must be published before
// tokensmith is Ready — the same rule the metadata public route follows.
func TestGatewayReconciler_PublicRoutesAppliedBeforeTokensmith(t *testing.T) {
	scheme := newScheme(t)
	cp := newControlPlane("alpha")
	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:   conditions.ConditionCertificatesValid,
		Status: metav1.ConditionTrue,
		Reason: conditions.ReasonReady,
	})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).Build()
	r := &GatewayReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	_ = getHTTPRoute(t, c, cp, routeSMDPublic)
	_ = getHTTPRoute(t, c, cp, routeBootPublic)
	assertHTTPRouteAbsent(t, c, cp, routeSMD)
	assertHTTPRouteAbsent(t, c, cp, routeBootService)
}

// Disabling a public route after it was published must remove it — SSA
// alone would leave the stale unauthenticated route in place.
func TestGatewayReconciler_PublicRouteDisabledIsDeleted(t *testing.T) {
	cp := newControlPlane("alpha")
	c := reconcileWithCertsValid(t, cp)
	_ = getHTTPRoute(t, c, cp, routeSMDPublic)
	_ = getHTTPRoute(t, c, cp, routeBootPublic)

	disabled := false
	cp.Spec.Services.SMD.PublicRoutes.Enabled = &disabled
	cp.Spec.Services.BootService.Enabled = false

	r := &GatewayReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	assertHTTPRouteAbsent(t, c, cp, routeSMDPublic)
	assertHTTPRouteAbsent(t, c, cp, routeBootPublic)
	// The protected SMD route must survive.
	_ = getHTTPRoute(t, c, cp, routeSMD)

	// Reconcile is idempotent once the routes are gone (NotFound is fine).
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("third reconcile: %v", err)
	}
}

func TestGatewayReconciler_PublicRouteSkippedForExternalService(t *testing.T) {
	cp := newControlPlane("alpha")
	ext := "https://smd.example.org"
	cp.Spec.Services.SMD.Enabled = false
	cp.Spec.Services.SMD.ExternalEndpoint = &ext
	c := reconcileWithCertsValid(t, cp)
	assertHTTPRouteAbsent(t, c, cp, routeSMDPublic)
	_ = getHTTPRoute(t, c, cp, routeBootPublic)
}

func TestGatewayReconciler_DescribeIncludesPublicRoutes(t *testing.T) {
	cp := newControlPlane("alpha")
	objs, err := (&GatewayReconciler{}).Describe(cp)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	found := map[string]bool{}
	for _, o := range objs {
		if _, ok := o.(*gwapiv1.HTTPRoute); ok {
			found[o.GetName()] = true
		}
	}
	for _, name := range []string{routeSMDPublic, routeBootPublic} {
		if !found[name] {
			t.Errorf("Describe() missing HTTPRoute %q", name)
		}
	}
}

func TestResolvePublicPaths(t *testing.T) {
	off := false
	on := true
	defaults := []string{testPathB, testPathA}
	cases := []struct {
		name string
		spec openchamiv1alpha1.PublicRoutesSpec
		want []string
	}{
		{"nil enabled uses defaults", openchamiv1alpha1.PublicRoutesSpec{}, []string{testPathA, testPathB}},
		{"explicit enabled uses defaults", openchamiv1alpha1.PublicRoutesSpec{Enabled: &on}, []string{testPathA, testPathB}},
		{"disabled", openchamiv1alpha1.PublicRoutesSpec{Enabled: &off, Paths: []string{testPathX}}, nil},
		{"override", openchamiv1alpha1.PublicRoutesSpec{Paths: []string{testPathX}}, []string{testPathX}},
		{"prefix itself rejected", openchamiv1alpha1.PublicRoutesSpec{Paths: []string{"/hsm"}}, nil},
		{"lookalike prefix rejected", openchamiv1alpha1.PublicRoutesSpec{Paths: []string{"/hsmfoo/x"}}, nil},
		{"all invalid yields nil", openchamiv1alpha1.PublicRoutesSpec{Paths: []string{testPathBootScript}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolvePublicPaths(tc.spec, pathSMDPrefix, defaults)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
	// Defaults must not be mutated by sorting.
	if defaults[0] != testPathB {
		t.Errorf("resolvePublicPaths mutated the defaults slice: %v", defaults)
	}
}
