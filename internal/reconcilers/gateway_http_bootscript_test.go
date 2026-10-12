// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

// Issue #69: GET /boot/v1/bootscript must be served on the plain-HTTP
// listener (iPXE builds that don't trust a private gateway CA can't follow
// the 301 to HTTPS), while every other HTTP request keeps redirecting.

func TestGatewayReconciler_BootScriptHTTPRouteDefaults(t *testing.T) {
	cp := newControlPlane("alpha")
	c := reconcileWithCertsValid(t, cp)

	hr := getHTTPRoute(t, c, cp, routeBootPublicHTTP)
	got := publicRoutePathsOn(t, hr, listenerHTTP, ServiceBootService, bootServicePort)
	if !reflect.DeepEqual(got, []string{pathBootScript}) {
		t.Errorf("boot-service-public-http paths=%v want only [%s]", got, pathBootScript)
	}
	if len(hr.Spec.Hostnames) != 1 || string(hr.Spec.Hostnames[0]) != cp.Spec.Domain {
		t.Errorf("boot-service-public-http hostnames=%v want [%s]", hr.Spec.Hostnames, cp.Spec.Domain)
	}

	// The HTTPS public route is unchanged and still carries every default.
	httpsPaths := publicRoutePaths(t, getHTTPRoute(t, c, cp, routeBootPublic), ServiceBootService, bootServicePort)
	if want := sortedCopy(defaultBootPublicPaths); !reflect.DeepEqual(httpsPaths, want) {
		t.Errorf("boot-service-public paths=%v want %v", httpsPaths, want)
	}
}

// The catch-all redirect must stay exactly as it was: attached to the HTTP
// listener only, no matches (implicit PathPrefix "/"), a single 301 to https.
func TestGatewayReconciler_HTTPRedirectUnchanged(t *testing.T) {
	cp := newControlPlane("alpha")
	c := reconcileWithCertsValid(t, cp)

	hr := getHTTPRoute(t, c, cp, routeHTTPRedirect)
	if len(hr.Spec.ParentRefs) != 1 || hr.Spec.ParentRefs[0].SectionName == nil ||
		string(*hr.Spec.ParentRefs[0].SectionName) != listenerHTTP {
		t.Fatalf("http-redirect parentRefs=%+v want only the http listener", hr.Spec.ParentRefs)
	}
	if len(hr.Spec.Rules) != 1 {
		t.Fatalf("http-redirect: want 1 rule, got %d", len(hr.Spec.Rules))
	}
	rule := hr.Spec.Rules[0]
	if len(rule.Matches) != 0 || len(rule.BackendRefs) != 0 {
		t.Errorf("http-redirect must be a catch-all with no backends, got matches=%+v backends=%+v",
			rule.Matches, rule.BackendRefs)
	}
	if len(rule.Filters) != 1 || rule.Filters[0].Type != gwapiv1.HTTPRouteFilterRequestRedirect ||
		rule.Filters[0].RequestRedirect == nil {
		t.Fatalf("http-redirect filters=%+v want a single RequestRedirect", rule.Filters)
	}
	rr := rule.Filters[0].RequestRedirect
	if rr.Scheme == nil || *rr.Scheme != "https" || rr.StatusCode == nil || *rr.StatusCode != 301 {
		t.Errorf("http-redirect redirect=%+v want 301 → https", rr)
	}
}

// --- Gateway API HTTPRoute match precedence model -------------------------
//
// A deliberately small, test-side model of the Gateway API spec's
// cross-route match precedence for one listener + hostname:
//
//  1. Exact path match
//  2. PathPrefix match with the most characters
//  3. Method match
//  4. Most header matches
//  5. Most query-param matches
//
// A rule with no matches is an implicit PathPrefix "/". Remaining ties
// (creation timestamp, namespace/name) never arise in these assertions;
// the model fails the test instead of guessing if one does.

type routeCandidate struct {
	route  string
	exact  bool
	prefix int
	method bool
	hdrs   int
	query  int
}

func (a routeCandidate) beats(b routeCandidate) bool {
	switch {
	case a.exact != b.exact:
		return a.exact
	case a.prefix != b.prefix:
		return a.prefix > b.prefix
	case a.method != b.method:
		return a.method
	case a.hdrs != b.hdrs:
		return a.hdrs > b.hdrs
	default:
		return a.query > b.query
	}
}

func pathPrefixMatches(prefix, path string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return true
	}
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// candidateFor returns the precedence key of match m for (method, path),
// or ok=false when m doesn't match the request at all.
func candidateFor(route string, m *gwapiv1.HTTPRouteMatch, method, path string) (routeCandidate, bool) {
	c := routeCandidate{route: route}
	if m == nil { // implicit PathPrefix "/"
		return c, true
	}
	if len(m.Headers) != 0 || len(m.QueryParams) != 0 {
		// None of the operator's routes use these; requests in the table
		// carry none, so such a match could never be selected.
		return c, false
	}
	pType, pVal := gwapiv1.PathMatchPathPrefix, "/"
	if m.Path != nil {
		if m.Path.Type != nil {
			pType = *m.Path.Type
		}
		if m.Path.Value != nil {
			pVal = *m.Path.Value
		}
	}
	switch pType {
	case gwapiv1.PathMatchExact:
		if path != pVal {
			return c, false
		}
		c.exact = true
	case gwapiv1.PathMatchPathPrefix:
		if !pathPrefixMatches(pVal, path) {
			return c, false
		}
		c.prefix = len(strings.TrimSuffix(pVal, "/"))
	default:
		return c, false
	}
	if m.Method != nil {
		if string(*m.Method) != method {
			return c, false
		}
		c.method = true
	}
	return c, true
}

func attachedTo(hr *gwapiv1.HTTPRoute, listener string) bool {
	for _, p := range hr.Spec.ParentRefs {
		if string(p.Name) == gatewayName && p.SectionName != nil && string(*p.SectionName) == listener {
			return true
		}
	}
	return false
}

// selectRoute returns the name of the HTTPRoute that would serve
// (listener, method, path), or "" when nothing matches.
func selectRoute(t *testing.T, routes []gwapiv1.HTTPRoute, listener, method, path string) string {
	t.Helper()
	var best *routeCandidate
	tie := false
	for i := range routes {
		hr := &routes[i]
		if !attachedTo(hr, listener) {
			continue
		}
		for _, rule := range hr.Spec.Rules {
			matches := make([]*gwapiv1.HTTPRouteMatch, 0, len(rule.Matches))
			for j := range rule.Matches {
				matches = append(matches, &rule.Matches[j])
			}
			if len(matches) == 0 {
				matches = append(matches, nil)
			}
			for _, m := range matches {
				c, ok := candidateFor(hr.Name, m, method, path)
				if !ok {
					continue
				}
				switch {
				case best == nil || c.beats(*best):
					cc := c
					best, tie = &cc, false
				case !best.beats(c) && best.route != c.route:
					tie = true
				}
			}
		}
	}
	if best == nil {
		return ""
	}
	if tie {
		t.Fatalf("%s %s on %s: ambiguous precedence tie involving %q", method, path, listener, best.route)
	}
	return best.route
}

func listHTTPRoutes(t *testing.T, c client.Client, cp *openchamiv1alpha1.OpenCHAMIControlPlane) []gwapiv1.HTTPRoute {
	t.Helper()
	list := &gwapiv1.HTTPRouteList{}
	if err := c.List(context.Background(), list, client.InNamespace(ControlPlaneNamespace(cp))); err != nil {
		t.Fatalf("listing HTTPRoutes: %v", err)
	}
	return list.Items
}

// The HTTP bootscript route must out-rank the catch-all redirect for
// GET /boot/v1/bootscript only; everything else on :80 still redirects,
// and HTTPS routing is unchanged.
func TestGatewayReconciler_BootScriptHTTPPrecedence(t *testing.T) {
	cp := newControlPlane("alpha")
	c := reconcileWithCertsValid(t, cp)
	routes := listHTTPRoutes(t, c, cp)

	get, post := string(gwapiv1.HTTPMethodGet), string(gwapiv1.HTTPMethodPost)
	cases := []struct {
		listener, method, path, want string
	}{
		{listenerHTTP, get, pathBootScript, routeBootPublicHTTP},
		{listenerHTTP, string(gwapiv1.HTTPMethodHead), pathBootScript, routeHTTPRedirect},
		{listenerHTTP, post, pathBootScript, routeHTTPRedirect},
		{listenerHTTP, get, pathBootScript + "/x", routeHTTPRedirect},
		{listenerHTTP, get, pathBootScript + "x", routeHTTPRedirect},
		{listenerHTTP, get, bootServiceHealthPath, routeHTTPRedirect},
		{listenerHTTP, get, "/boot/v1/service/version", routeHTTPRedirect},
		{listenerHTTP, get, "/boot/v1/bootconfigurations", routeHTTPRedirect},
		{listenerHTTP, get, pathBootAdmin + "/nodes", routeHTTPRedirect},
		{listenerHTTP, get, testPathSMDComponents, routeHTTPRedirect},
		{listenerHTTP, get, pathMetadataPrefix + "/x", routeHTTPRedirect},
		{listenerHTTP, get, pathTokensmithJWKS, routeHTTPRedirect},
		{listenerHTTP, get, "/", routeHTTPRedirect},
		{listenerHTTPS, get, pathBootScript, routeBootPublic},
		{listenerHTTPS, post, pathBootScript, routeBootService},
		{listenerHTTPS, get, bootServiceHealthPath, routeBootPublic},
		{listenerHTTPS, get, "/boot/v1/bootconfigurations", routeBootService},
		{listenerHTTPS, get, pathBootAdmin + "/nodes", routeBootAdmin},
		{listenerHTTPS, get, testPathSMDComponents, routeSMDPublic},
		{listenerHTTPS, get, pathMetadataAdmin + "/groups", routeMetadataAdmin},
	}
	for _, tc := range cases {
		if got := selectRoute(t, routes, tc.listener, tc.method, tc.path); got != tc.want {
			t.Errorf("%s %s on %s → %q, want %q", tc.method, tc.path, tc.listener, got, tc.want)
		}
	}
}

// Only the boot-script route and the redirect may ever attach to the
// plain-HTTP listener; nothing JWT-gated or admin-facing goes plaintext.
func TestGatewayReconciler_HTTPListenerOnlyRedirectAndBootScript(t *testing.T) {
	cp := newControlPlane("alpha")
	c := reconcileWithCertsValid(t, cp)
	var onHTTP []string
	routes := listHTTPRoutes(t, c, cp)
	for i := range routes {
		hr := &routes[i]
		if attachedTo(hr, listenerHTTP) {
			onHTTP = append(onHTTP, hr.Name)
		}
	}
	want := sortedCopy([]string{routeHTTPRedirect, routeBootPublicHTTP})
	if got := sortedCopy(onHTTP); !reflect.DeepEqual(got, want) {
		t.Errorf("routes on http listener=%v want %v", got, want)
	}
}

func TestGatewayReconciler_BootScriptHTTPRouteAbsent(t *testing.T) {
	off := false
	ext := "https://boot.example.org"
	cases := []struct {
		name         string
		mutate       func(cp *openchamiv1alpha1.OpenCHAMIControlPlane)
		wantHTTPSPub bool
	}{
		{"httpBootScript=false", func(cp *openchamiv1alpha1.OpenCHAMIControlPlane) {
			cp.Spec.Services.BootService.HTTPBootScript = &off
		}, true},
		{"publicRoutes disabled", func(cp *openchamiv1alpha1.OpenCHAMIControlPlane) {
			cp.Spec.Services.BootService.PublicRoutes.Enabled = &off
		}, false},
		{"paths override omits bootscript", func(cp *openchamiv1alpha1.OpenCHAMIControlPlane) {
			cp.Spec.Services.BootService.PublicRoutes.Paths = []string{bootServiceHealthPath}
		}, true},
		{"boot-service disabled", func(cp *openchamiv1alpha1.OpenCHAMIControlPlane) {
			cp.Spec.Services.BootService.Enabled = false
		}, false},
		{"boot-service external", func(cp *openchamiv1alpha1.OpenCHAMIControlPlane) {
			cp.Spec.Services.BootService.Enabled = false
			cp.Spec.Services.BootService.ExternalEndpoint = &ext
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := newControlPlane("alpha")
			tc.mutate(cp)
			c := reconcileWithCertsValid(t, cp)
			assertHTTPRouteAbsent(t, c, cp, routeBootPublicHTTP)
			if tc.wantHTTPSPub {
				_ = getHTTPRoute(t, c, cp, routeBootPublic)
			}
			// The catch-all redirect is always there, so plaintext requests
			// still get upgraded rather than 404ing.
			_ = getHTTPRoute(t, c, cp, routeHTTPRedirect)
			if got := selectRoute(t, listHTTPRoutes(t, c, cp), listenerHTTP,
				string(gwapiv1.HTTPMethodGet), pathBootScript); got != routeHTTPRedirect {
				t.Errorf("GET %s on http → %q, want %q", pathBootScript, got, routeHTTPRedirect)
			}
		})
	}
}

// A paths override that still lists the boot script keeps it on HTTP.
func TestGatewayReconciler_BootScriptHTTPRouteWithCustomPaths(t *testing.T) {
	cp := newControlPlane("alpha")
	cp.Spec.Services.BootService.PublicRoutes.Paths = []string{bootServiceHealthPath, pathBootScript}
	c := reconcileWithCertsValid(t, cp)
	got := publicRoutePathsOn(t, getHTTPRoute(t, c, cp, routeBootPublicHTTP),
		listenerHTTP, ServiceBootService, bootServicePort)
	if !reflect.DeepEqual(got, []string{pathBootScript}) {
		t.Errorf("boot-service-public-http paths=%v want only [%s]", got, pathBootScript)
	}
}

// Toggling httpBootScript off after the route was published must delete
// it (SSA never prunes), and a further reconcile is idempotent.
func TestGatewayReconciler_BootScriptHTTPRouteToggledOffIsDeleted(t *testing.T) {
	cp := newControlPlane("alpha")
	c := reconcileWithCertsValid(t, cp)
	_ = getHTTPRoute(t, c, cp, routeBootPublicHTTP)

	off := false
	cp.Spec.Services.BootService.HTTPBootScript = &off
	r := &GatewayReconciler{Client: c, Recorder: record.NewFakeRecorder(10)}
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	assertHTTPRouteAbsent(t, c, cp, routeBootPublicHTTP)
	_ = getHTTPRoute(t, c, cp, routeBootPublic)
	_ = getHTTPRoute(t, c, cp, routeHTTPRedirect)

	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("third reconcile: %v", err)
	}
	assertHTTPRouteAbsent(t, c, cp, routeBootPublicHTTP)

	// Turning it back on re-creates it.
	on := true
	cp.Spec.Services.BootService.HTTPBootScript = &on
	if _, err := r.Reconcile(context.Background(), cp); err != nil {
		t.Fatalf("fourth reconcile: %v", err)
	}
	_ = getHTTPRoute(t, c, cp, routeBootPublicHTTP)
}

func TestBootServiceSpec_HTTPBootScriptEnabled(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name string
		v    *bool
		want bool
	}{
		{"nil defaults to enabled", nil, true},
		{"true", &on, true},
		{"false", &off, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := openchamiv1alpha1.BootServiceSpec{HTTPBootScript: tc.v}
			if got := spec.HTTPBootScriptEnabled(); got != tc.want {
				t.Errorf("HTTPBootScriptEnabled()=%v want %v", got, tc.want)
			}
		})
	}
}
