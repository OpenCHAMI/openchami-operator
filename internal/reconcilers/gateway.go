// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"fmt"
	"time"

	egv1alpha1 "github.com/envoyproxy/gateway/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/logging"
)

const (
	gatewayRequeueAfter = 30 * time.Second

	// gatewayName is the canonical Gateway resource name in each cluster ns.
	gatewayName = "openchami-gateway"

	// Listener section names. SectionName values referenced from HTTPRoute
	// parentRefs to attach a route to a specific listener.
	listenerHTTP  = "http"
	listenerHTTPS = "https"

	// jwtProviderName is the common JWT provider name used by all
	// SecurityPolicy resources targeting the cluster's tokensmith JWKS.
	jwtProviderName = "tokensmith"

	// gatewayAPIVersion / kindGateway / kindHTTPRoute are reused as TypeMeta
	// strings on the produced objects (server-side apply requires them).
	gatewayAPIVersion = "gateway.networking.k8s.io/v1"
	kindGateway       = "Gateway"
	kindHTTPRoute     = "HTTPRoute"

	envoyGatewayAPIVersion       = "gateway.envoyproxy.io/v1alpha1"
	kindSecurityPolicy           = "SecurityPolicy"
	kindBackendTrafficPolicy     = "BackendTrafficPolicy"
	envoyGatewayPolicyTargetKind = "HTTPRoute"

	// HTTP route names. Constants ensure the SecurityPolicy and HTTPRoute
	// resources reference the same names.
	routeHTTPRedirect   = "http-redirect"
	routeSMD            = "smd"
	routeTokensmith     = "tokensmith"
	routeBootService    = "boot-service"
	routeBootAdmin      = "boot-admin"
	routeMetadataPublic = "metadata-public"
	routeMetadataAdmin  = "metadata-admin"
	policyJWTSMD        = "jwt-smd"
	policyJWTBoot       = "jwt-boot"
	policyJWTBootAdmin  = "jwt-boot-admin"
	policyJWTMetaAdmin  = "jwt-metadata-admin"
	policySMDRateLimit  = "smd-ratelimit"
	rateLimitHeaderName = "X-User-ID"
	rateLimitRequests   = uint(1000)

	// gatewayURLScheme is the canonical scheme for the operator's
	// public ingress URL. The HTTP listener 301-redirects to HTTPS, so
	// publishing http:// in .status.gateway.url would always cost an
	// extra round-trip on every smoke-test / client invocation.
	gatewayURLScheme = "https"

	// Status-route keys reported in .status.gateway.routes. These are
	// the contract clients (smoke test, ochami-admin, downstream apps)
	// rely on for path lookups — never rename without a CRD version
	// bump and migration. Values intentionally separate from the
	// HTTPRoute *resource* names above because callers care about the
	// service identity, not the kubernetes object name.
	statusRouteSMD             = "smd"
	statusRouteBootService     = "boot-service"
	statusRouteBootAdmin       = "boot-service-admin"
	statusRouteMetadataService = "metadata-service"
	statusRouteMetadataAdmin   = "metadata-service-admin"
	statusRouteTokensmithJWKS  = "tokensmith-jwks"
	statusRouteTokensmithToken = "tokensmith-token"

	// jwksBackendTLSPolicyName is the BackendTLSPolicy resource that
	// teaches envoy gateway to trust the per-cluster service-identity
	// CA when fetching tokensmith's JWKS over HTTPS. Without it,
	// envoy's RemoteJwks dial would fail with x509:
	// certificate signed by unknown authority once tokensmith
	// switches to its HTTPS listener.
	jwksBackendTLSPolicyName = "tokensmith-backend-tls"
	kindBackendTLSPolicy     = "BackendTLSPolicy"

	// Canonical URL paths the operator mounts on its Envoy Gateway.
	// Single source of truth shared by the HTTPRoute builders below,
	// the `.status.gateway.routes` map, and the topology ConfigMap —
	// keeping them aligned avoids subtle drift where a config map
	// advertises one path and the gateway serves another. If a path
	// moves, update it here and every consumer follows.
	pathSMDPrefix        = "/hsm"
	pathBootPrefix       = "/boot"
	pathBootAdmin        = "/admin/boot"
	pathMetadataPrefix   = "/cloud-init"
	pathMetadataAdmin    = pathMetadataPrefix + "/admin"
	pathTokensmithJWKS   = "/.well-known/jwks.json"
	pathTokensmithToken  = "/oauth/token"
	pathTokensmithHealth = "/health"
)

// gatewayStatusRoutes returns the canonical route-name → URL-path map
// the operator publishes in .status.gateway.routes. Single source of
// truth so HTTPRoute construction and status population stay in sync
// — if a path moves in one place it has to move here too.
//
// Services whose `ServiceDeployedInCluster` returns false (disabled or
// served by an externalEndpoint) are omitted: the operator's gateway
// does not proxy them. Clients should consult
// `.status.services[<svc>].endpoint` instead for those.
func gatewayStatusRoutes(cp *openchamiv1alpha1.OpenCHAMIControlPlane) map[string]string {
	routes := map[string]string{}
	if ServiceDeployedInCluster(cp, ServiceSMD) {
		routes[statusRouteSMD] = pathSMDPrefix
	}
	if ServiceDeployedInCluster(cp, ServiceBootService) {
		routes[statusRouteBootService] = pathBootPrefix
		routes[statusRouteBootAdmin] = pathBootAdmin
	}
	if ServiceDeployedInCluster(cp, ServiceMetadataService) {
		routes[statusRouteMetadataService] = pathMetadataPrefix
		routes[statusRouteMetadataAdmin] = pathMetadataAdmin
	}
	if ServiceDeployedInCluster(cp, ServiceTokensmith) {
		routes[statusRouteTokensmithJWKS] = pathTokensmithJWKS
		routes[statusRouteTokensmithToken] = pathTokensmithToken
	}
	return routes
}

// GatewayReconciler ensures the cluster's Envoy Gateway and the HTTPRoutes,
// SecurityPolicies, and BackendTrafficPolicies that govern access to the
// cluster's services. It does not deploy the Envoy data plane itself; that
// is the responsibility of the Envoy Gateway controller.
type GatewayReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

// Reconcile applies the Gateway and its routing/policy resources, then reports
// ConditionGatewayReady based on the Gateway's Programmed condition.
//
// Gating: the gateway is only applied once ConditionCertificatesValid is True,
// because the HTTPS listener's certificateRef points at the TLS Secret managed
// by the certificates reconciler. Without that Secret the gateway will never
// program its listeners.
func (r *GatewayReconciler) Reconcile(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cp, "gateway")

	if !apimeta.IsStatusConditionTrue(cp.Status.Conditions, conditions.ConditionCertificatesValid) {
		log.Info("waiting for certificate before applying gateway")
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionGatewayReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonAwaitingCertificate,
			Message:            "waiting for CertificatesValid before applying gateway",
			ObservedGeneration: cp.Generation,
		})
		return ctrl.Result{RequeueAfter: gatewayRequeueAfter}, nil
	}

	// Apply order matters: always-safe resources (Gateway, redirect,
	// tokensmith route, metadata public route) go first. JWT-gated
	// SecurityPolicies + their HTTPRoutes only follow once tokensmith
	// can serve JWKS — otherwise envoy's JWT filter caches the failed
	// RemoteJWKS fetch at startup and every JWT-gated request 500s
	// with `direct_response` until the proxy pod is restarted.
	objs := r.objectsAlways(cp)
	tokensmithReady := tokensmithJWKSReady(cp)
	if tokensmithReady {
		objs = append(objs, r.objectsRequiringTokensmith(cp)...)
	} else {
		log.Info("deferring JWT-gated routes until tokensmith is Ready")
	}
	for _, obj := range objs {
		oLog := logging.EnrichWithResource(log,
			obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName())
		oLog.Info("applying gateway resource")
		if err := r.Client.Patch(ctx, obj, client.Apply, //nolint:staticcheck // SSA via Patch
			client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
			return ctrl.Result{}, fmt.Errorf("applying %s/%s: %w",
				obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
		}
	}
	if !tokensmithReady {
		// Surface the deferral as a clear GatewayReady=False reason so
		// `kubectl get ocp` doesn't show a green-by-default cluster
		// while half the routes are missing. Programmed=True on the
		// Gateway resource itself can still be reached for the
		// listeners we did apply.
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionGatewayReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonAwaitingTokensmith,
			Message:            "deferring JWT-protected routes until tokensmith is Ready (envoy JWKS fetch would otherwise poison)",
			ObservedGeneration: cp.Generation,
		})
		cp.Status.Gateway = nil
		return ctrl.Result{RequeueAfter: gatewayRequeueAfter}, nil
	}

	current := &gwapiv1.Gateway{}
	getErr := r.Client.Get(ctx, types.NamespacedName{
		Namespace: ControlPlaneNamespace(cp), Name: gatewayName,
	}, current)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return ctrl.Result{}, fmt.Errorf("reading gateway status: %w", getErr)
	}

	programmed := false
	if getErr == nil {
		for _, c := range current.Status.Conditions {
			if c.Type == string(gwapiv1.GatewayConditionProgrammed) && c.Status == metav1.ConditionTrue {
				programmed = true
				break
			}
		}
	}

	if !programmed {
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionGatewayReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonNotProgrammed,
			Message:            "waiting for Gateway Programmed=True from Envoy Gateway",
			ObservedGeneration: cp.Generation,
		})
		// Don't keep stale gateway info — if Programmed flips back to
		// False (Envoy Gateway restart, gateway-class swap, etc.)
		// clients should see the URL go away rather than try to use
		// an endpoint that may not route.
		cp.Status.Gateway = nil
		return ctrl.Result{RequeueAfter: gatewayRequeueAfter}, nil
	}

	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionGatewayReady,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            "Envoy Gateway reports Programmed=True",
		ObservedGeneration: cp.Generation,
	})
	// Publish the canonical ingress URL + per-service path map so
	// clients (smoke tests, ochami-admin, downstream apps) can talk to
	// the cluster through one stable hostname instead of N
	// kubectl-port-forwards.
	cp.Status.Gateway = &openchamiv1alpha1.GatewayStatus{
		URL:    fmt.Sprintf("%s://%s", gatewayURLScheme, cp.Spec.Domain),
		Routes: gatewayStatusRoutes(cp),
	}
	return ctrl.Result{}, nil
}

// Describe returns the Kubernetes objects this reconciler would apply.
func (r *GatewayReconciler) Describe(cp *openchamiv1alpha1.OpenCHAMIControlPlane) ([]client.Object, error) {
	return r.objectsToApply(cp), nil
}

// objectsToApply returns the canonical Gateway, HTTPRoute, SecurityPolicy,
// and BackendTrafficPolicy set in deterministic apply order.
//
// HTTPRoute backends point at in-namespace Services that the operator
// produces. When a service has spec.services.<svc>.externalEndpoint set,
// the operator does not deploy that Service, so we skip the corresponding
// HTTPRoute and any policies that target it. Sites using externalEndpoint
// are expected to terminate routing for those services externally.
//
// Returns the full set: Describe() / static snapshots want everything;
// the Reconcile loop trims to objectsAlways() when tokensmith isn't
// Ready yet (see that helper for the rationale).
func (r *GatewayReconciler) objectsToApply(cp *openchamiv1alpha1.OpenCHAMIControlPlane) []client.Object {
	return append(r.objectsAlways(cp), r.objectsRequiringTokensmith(cp)...)
}

// objectsAlways returns the gateway resources that are safe to apply
// before tokensmith is Ready — listeners, HTTP→HTTPS redirect, and the
// tokensmith route itself (which has no JWT SecurityPolicy attached;
// it serves OIDC discovery / JWKS / token-exchange paths that MUST be
// reachable before any other service can authenticate).
//
// Metadata-service's *public* cloud-init route is also in this set: it
// has no SecurityPolicy and is intentionally unauthenticated (cloud-init
// runs on nodes that don't have a JWT to present at first boot).
func (r *GatewayReconciler) objectsAlways(cp *openchamiv1alpha1.OpenCHAMIControlPlane) []client.Object {
	objs := []client.Object{
		r.buildGateway(cp),
		r.buildHTTPRedirectRoute(cp),
	}
	if ServiceDeployedInCluster(cp, ServiceTokensmith) {
		objs = append(objs, r.buildTokensmithRoute(cp))
	}
	if ServiceDeployedInCluster(cp, ServiceMetadataService) {
		objs = append(objs, r.buildMetadataPublicRoute(cp))
	}
	return objs
}

// objectsRequiringTokensmith returns the gateway resources whose JWT
// SecurityPolicies — and the HTTPRoutes they target — must NOT be
// applied until tokensmith can serve JWKS. Envoy's JWT filter resolves
// `RemoteJwks.URI` at startup; if the URL 503s during that resolution,
// the filter caches the failure and every subsequent request to a
// JWT-gated route returns 500 with `response_code_details: direct_response`
// (we hit exactly this when tokensmith came up after envoy).
//
// Paired routes and policies are grouped together so a future caller
// can't accidentally apply one without the other.
//
// When tokensmith is in HTTPS mode (service-identity has issued the
// server cert), a BackendTLSPolicy is also included so envoy's
// JWKS-fetch TLS handshake validates against the in-namespace CA.
// Skipping it when tokensmith is still on HTTP avoids envoy rejecting
// the policy as "no matching backend listener TLS" — the policy is
// inert without a TLS backend.
func (r *GatewayReconciler) objectsRequiringTokensmith(cp *openchamiv1alpha1.OpenCHAMIControlPlane) []client.Object {
	var objs []client.Object
	if ServiceDeployedInCluster(cp, ServiceTokensmith) && tokensmithMTLSEnabled(cp) {
		objs = append(objs, r.buildJWKSBackendTLSPolicy(cp))
	}
	if ServiceDeployedInCluster(cp, ServiceSMD) {
		objs = append(objs,
			r.buildSMDRoute(cp),
			r.buildSecurityPolicy(cp, policyJWTSMD, routeSMD),
			r.buildSMDRateLimit(cp),
		)
	}
	if ServiceDeployedInCluster(cp, ServiceBootService) {
		objs = append(objs,
			r.buildBootServiceRoute(cp),
			r.buildBootAdminRoute(cp),
			r.buildSecurityPolicy(cp, policyJWTBoot, routeBootService),
			r.buildSecurityPolicy(cp, policyJWTBootAdmin, routeBootAdmin),
		)
	}
	if ServiceDeployedInCluster(cp, ServiceMetadataService) {
		objs = append(objs,
			r.buildMetadataAdminRoute(cp),
			r.buildSecurityPolicy(cp, policyJWTMetaAdmin, routeMetadataAdmin),
		)
	}
	return objs
}

// tokensmithJWKSReady returns true when JWT-gated routes are safe to
// publish: either tokensmith is operator-managed and has reached
// Ready=true in `cp.Status.Services[tokensmith]`, or tokensmith is
// served externally (where readiness is the site operator's problem
// and we have no Service to inspect).
func tokensmithJWKSReady(cp *openchamiv1alpha1.OpenCHAMIControlPlane) bool {
	if !ServiceDeployedInCluster(cp, ServiceTokensmith) {
		// External: nothing for the operator to gate on — assume reachable.
		return true
	}
	st, ok := cp.Status.Services[ServiceTokensmith]
	return ok && st.Ready
}

// gatewayLabels returns the canonical labels for gateway-managed resources.
func gatewayLabels(cp *openchamiv1alpha1.OpenCHAMIControlPlane) map[string]string {
	return map[string]string{
		labelAppName:   "gateway",
		labelAppInst:   "openchami-" + cp.Spec.ClusterName,
		labelManagedBy: managedByValue,
	}
}

// gatewayClassName returns the configured gatewayClass with a sensible default.
func gatewayClassName(cp *openchamiv1alpha1.OpenCHAMIControlPlane) string {
	if gc := cp.Spec.Networking.GatewayClass; gc != "" {
		return gc
	}
	return "envoy"
}

// jwksURL returns the JWKS URL for tokensmith — by default the in-cluster
// Service URL, or a site-provided externalEndpoint when set. SecurityPolicy
// resources reference this directly (rather than the gateway-fronted URL)
// to avoid a routing loop when tokensmith is operator-managed.
//
// Once service-identity has provisioned the tokensmith server cert and
// flipped the listener to HTTPS (tokensmithMTLSEnabled), the URL must
// use the https:// scheme — and envoy needs a BackendTLSPolicy
// targeting the tokensmith Service so its TLS handshake actually
// validates the in-namespace CA. See buildJWKSBackendTLSPolicy.
func jwksURL(cp *openchamiv1alpha1.OpenCHAMIControlPlane) string {
	if tokensmithMTLSEnabled(cp) {
		return TokensmithBaseURL(cp) + "/.well-known/jwks.json"
	}
	return ServiceURL(cp, ServiceTokensmith) + "/.well-known/jwks.json"
}

func (r *GatewayReconciler) buildGateway(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *gwapiv1.Gateway {
	hostname := gwapiv1.Hostname(cp.Spec.Domain)
	tlsMode := gwapiv1.TLSModeTerminate
	secretRef := gwapiv1.SecretObjectReference{
		Name: gwapiv1.ObjectName(GatewayTLSSecretName(cp)),
	}
	return &gwapiv1.Gateway{
		TypeMeta: metav1.TypeMeta{APIVersion: gatewayAPIVersion, Kind: kindGateway},
		ObjectMeta: metav1.ObjectMeta{
			Name:      gatewayName,
			Namespace: ControlPlaneNamespace(cp),
			Labels:    gatewayLabels(cp),
		},
		Spec: gwapiv1.GatewaySpec{
			GatewayClassName: gwapiv1.ObjectName(gatewayClassName(cp)),
			Listeners: []gwapiv1.Listener{
				{
					Name:     listenerHTTP,
					Hostname: &hostname,
					Port:     80,
					Protocol: gwapiv1.HTTPProtocolType,
				},
				{
					Name:     listenerHTTPS,
					Hostname: &hostname,
					Port:     443,
					Protocol: gwapiv1.HTTPSProtocolType,
					TLS: &gwapiv1.ListenerTLSConfig{
						Mode:            &tlsMode,
						CertificateRefs: []gwapiv1.SecretObjectReference{secretRef},
					},
				},
			},
		},
	}
}

// httpParentRef returns the parentRef pointing at the HTTP listener of the
// cluster gateway.
func httpParentRef(cp *openchamiv1alpha1.OpenCHAMIControlPlane) gwapiv1.ParentReference {
	section := gwapiv1.SectionName(listenerHTTP)
	ns := gwapiv1.Namespace(ControlPlaneNamespace(cp))
	return gwapiv1.ParentReference{
		Name:        gwapiv1.ObjectName(gatewayName),
		Namespace:   &ns,
		SectionName: &section,
	}
}

// httpsParentRef returns the parentRef pointing at the HTTPS listener of the
// cluster gateway.
func httpsParentRef(cp *openchamiv1alpha1.OpenCHAMIControlPlane) gwapiv1.ParentReference {
	section := gwapiv1.SectionName(listenerHTTPS)
	ns := gwapiv1.Namespace(ControlPlaneNamespace(cp))
	return gwapiv1.ParentReference{
		Name:        gwapiv1.ObjectName(gatewayName),
		Namespace:   &ns,
		SectionName: &section,
	}
}

func (r *GatewayReconciler) buildHTTPRedirectRoute(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *gwapiv1.HTTPRoute {
	hostname := gwapiv1.Hostname(cp.Spec.Domain)
	scheme := "https"
	statusCode := 301
	return &gwapiv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{APIVersion: gatewayAPIVersion, Kind: kindHTTPRoute},
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeHTTPRedirect,
			Namespace: ControlPlaneNamespace(cp),
			Labels:    gatewayLabels(cp),
		},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{
				ParentRefs: []gwapiv1.ParentReference{httpParentRef(cp)},
			},
			Hostnames: []gwapiv1.Hostname{hostname},
			Rules: []gwapiv1.HTTPRouteRule{{
				Filters: []gwapiv1.HTTPRouteFilter{{
					Type: gwapiv1.HTTPRouteFilterRequestRedirect,
					RequestRedirect: &gwapiv1.HTTPRequestRedirectFilter{
						Scheme:     &scheme,
						StatusCode: &statusCode,
					},
				}},
			}},
		},
	}
}

// pathPrefixRule builds a single rule that matches a given path prefix and
// forwards to the named in-namespace Service on the given port.
func pathPrefixRule(prefix, backend string, port int32) gwapiv1.HTTPRouteRule {
	matchType := gwapiv1.PathMatchPathPrefix
	prefixVal := prefix
	portNum := port
	return gwapiv1.HTTPRouteRule{
		Matches: []gwapiv1.HTTPRouteMatch{{
			Path: &gwapiv1.HTTPPathMatch{
				Type:  &matchType,
				Value: &prefixVal,
			},
		}},
		BackendRefs: []gwapiv1.HTTPBackendRef{{
			BackendRef: gwapiv1.BackendRef{
				BackendObjectReference: gwapiv1.BackendObjectReference{
					Name: gwapiv1.ObjectName(backend),
					Port: &portNum,
				},
			},
		}},
	}
}

// pathPrefixRewriteRule is pathPrefixRule + a URLRewrite filter that
// strips the matched prefix before forwarding. Used for admin paths
// where the operator exposes "/admin/<svc>/foo" through the gateway
// but the backend service only knows the bare "/foo" — boot-service
// and metadata-service both have CRUD APIs at their root that we want
// to JWT-gate and namespace under /admin/<svc> without rebuilding the
// service.
//
// The empty/missing ReplacePrefixMatch is intentional: gateway-api
// treats it as "rewrite the matched prefix to /". So /admin/boot/nodes
// → /nodes at the backend.
func pathPrefixRewriteRule(prefix, backend string, port int32) gwapiv1.HTTPRouteRule {
	matchType := gwapiv1.PathMatchPathPrefix
	prefixVal := prefix
	portNum := port
	rewrite := "/"
	return gwapiv1.HTTPRouteRule{
		Matches: []gwapiv1.HTTPRouteMatch{{
			Path: &gwapiv1.HTTPPathMatch{
				Type:  &matchType,
				Value: &prefixVal,
			},
		}},
		Filters: []gwapiv1.HTTPRouteFilter{{
			Type: gwapiv1.HTTPRouteFilterURLRewrite,
			URLRewrite: &gwapiv1.HTTPURLRewriteFilter{
				Path: &gwapiv1.HTTPPathModifier{
					Type:               gwapiv1.PrefixMatchHTTPPathModifier,
					ReplacePrefixMatch: &rewrite,
				},
			},
		}},
		BackendRefs: []gwapiv1.HTTPBackendRef{{
			BackendRef: gwapiv1.BackendRef{
				BackendObjectReference: gwapiv1.BackendObjectReference{
					Name: gwapiv1.ObjectName(backend),
					Port: &portNum,
				},
			},
		}},
	}
}

// exactPathRule builds a single rule that matches a given exact path and
// forwards to the named in-namespace Service on the given port.
func exactPathRule(path, backend string, port int32) gwapiv1.HTTPRouteRule {
	matchType := gwapiv1.PathMatchExact
	val := path
	portNum := port
	return gwapiv1.HTTPRouteRule{
		Matches: []gwapiv1.HTTPRouteMatch{{
			Path: &gwapiv1.HTTPPathMatch{
				Type:  &matchType,
				Value: &val,
			},
		}},
		BackendRefs: []gwapiv1.HTTPBackendRef{{
			BackendRef: gwapiv1.BackendRef{
				BackendObjectReference: gwapiv1.BackendObjectReference{
					Name: gwapiv1.ObjectName(backend),
					Port: &portNum,
				},
			},
		}},
	}
}

func (r *GatewayReconciler) buildSMDRoute(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *gwapiv1.HTTPRoute {
	hostname := gwapiv1.Hostname(cp.Spec.Domain)
	return &gwapiv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{APIVersion: gatewayAPIVersion, Kind: kindHTTPRoute},
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeSMD,
			Namespace: ControlPlaneNamespace(cp),
			Labels:    gatewayLabels(cp),
		},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{
				ParentRefs: []gwapiv1.ParentReference{httpsParentRef(cp)},
			},
			Hostnames: []gwapiv1.Hostname{hostname},
			Rules:     []gwapiv1.HTTPRouteRule{pathPrefixRule(pathSMDPrefix, ServiceSMD, smdPort)},
		},
	}
}

func (r *GatewayReconciler) buildTokensmithRoute(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *gwapiv1.HTTPRoute {
	hostname := gwapiv1.Hostname(cp.Spec.Domain)
	return &gwapiv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{APIVersion: gatewayAPIVersion, Kind: kindHTTPRoute},
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeTokensmith,
			Namespace: ControlPlaneNamespace(cp),
			Labels:    gatewayLabels(cp),
		},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{
				ParentRefs: []gwapiv1.ParentReference{httpsParentRef(cp)},
			},
			Hostnames: []gwapiv1.Hostname{hostname},
			Rules: []gwapiv1.HTTPRouteRule{
				exactPathRule(pathTokensmithJWKS, ServiceTokensmith, tokensmithPort),
				exactPathRule(pathTokensmithToken, ServiceTokensmith, tokensmithPort),
				exactPathRule(pathTokensmithHealth, ServiceTokensmith, tokensmithPort),
			},
		},
	}
}

func (r *GatewayReconciler) buildBootServiceRoute(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *gwapiv1.HTTPRoute {
	hostname := gwapiv1.Hostname(cp.Spec.Domain)
	return &gwapiv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{APIVersion: gatewayAPIVersion, Kind: kindHTTPRoute},
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeBootService,
			Namespace: ControlPlaneNamespace(cp),
			Labels:    gatewayLabels(cp),
		},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{
				ParentRefs: []gwapiv1.ParentReference{httpsParentRef(cp)},
			},
			Hostnames: []gwapiv1.Hostname{hostname},
			Rules:     []gwapiv1.HTTPRouteRule{pathPrefixRule(pathBootPrefix, ServiceBootService, bootServicePort)},
		},
	}
}

// buildBootAdminRoute exposes boot-service's admin CRUD endpoints
// (/nodes, /bootconfigurations, /instanceinfos siblings on the
// fabrica-shaped binary) under a JWT-gated `/admin/boot` prefix on the
// gateway. URL rewrite strips the prefix so the backend sees the bare
// path. This keeps the iPXE-facing /boot route (which has different
// auth requirements in production) separate from administrative
// manipulation of boot-service resources.
func (r *GatewayReconciler) buildBootAdminRoute(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *gwapiv1.HTTPRoute {
	hostname := gwapiv1.Hostname(cp.Spec.Domain)
	return &gwapiv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{APIVersion: gatewayAPIVersion, Kind: kindHTTPRoute},
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeBootAdmin,
			Namespace: ControlPlaneNamespace(cp),
			Labels:    gatewayLabels(cp),
		},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{
				ParentRefs: []gwapiv1.ParentReference{httpsParentRef(cp)},
			},
			Hostnames: []gwapiv1.Hostname{hostname},
			Rules:     []gwapiv1.HTTPRouteRule{pathPrefixRewriteRule(pathBootAdmin, ServiceBootService, bootServicePort)},
		},
	}
}

func (r *GatewayReconciler) buildMetadataPublicRoute(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *gwapiv1.HTTPRoute {
	hostname := gwapiv1.Hostname(cp.Spec.Domain)
	return &gwapiv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{APIVersion: gatewayAPIVersion, Kind: kindHTTPRoute},
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeMetadataPublic,
			Namespace: ControlPlaneNamespace(cp),
			Labels:    gatewayLabels(cp),
		},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{
				ParentRefs: []gwapiv1.ParentReference{httpsParentRef(cp)},
			},
			Hostnames: []gwapiv1.Hostname{hostname},
			Rules:     []gwapiv1.HTTPRouteRule{pathPrefixRule(pathMetadataPrefix, ServiceMetadataService, metadataServicePort)},
		},
	}
}

func (r *GatewayReconciler) buildMetadataAdminRoute(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *gwapiv1.HTTPRoute {
	hostname := gwapiv1.Hostname(cp.Spec.Domain)
	return &gwapiv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{APIVersion: gatewayAPIVersion, Kind: kindHTTPRoute},
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeMetadataAdmin,
			Namespace: ControlPlaneNamespace(cp),
			Labels:    gatewayLabels(cp),
		},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{
				ParentRefs: []gwapiv1.ParentReference{httpsParentRef(cp)},
			},
			Hostnames: []gwapiv1.Hostname{hostname},
			// URL-rewrite strips the /cloud-init/admin prefix so the
			// metadata-service backend (which serves fabrica CRUD at
			// /instanceinfos, /groups, etc., at its root) sees the
			// bare path. Without rewrite, every admin request would
			// 404 at the backend.
			Rules: []gwapiv1.HTTPRouteRule{pathPrefixRewriteRule(pathMetadataAdmin, ServiceMetadataService, metadataServicePort)},
		},
	}
}

// httpRouteTargetRef returns a SecurityPolicy/BackendTrafficPolicy targetRef
// pointing at an HTTPRoute in the same namespace.
func httpRouteTargetRef(routeName string) gwapiv1.LocalPolicyTargetReferenceWithSectionName {
	return gwapiv1.LocalPolicyTargetReferenceWithSectionName{
		LocalPolicyTargetReference: gwapiv1.LocalPolicyTargetReference{
			Group: gwapiv1.GroupName,
			Kind:  envoyGatewayPolicyTargetKind,
			Name:  gwapiv1.ObjectName(routeName),
		},
	}
}

func (r *GatewayReconciler) buildSecurityPolicy(cp *openchamiv1alpha1.OpenCHAMIControlPlane, name, route string) *egv1alpha1.SecurityPolicy {
	target := httpRouteTargetRef(route)
	return &egv1alpha1.SecurityPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: envoyGatewayAPIVersion, Kind: kindSecurityPolicy},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ControlPlaneNamespace(cp),
			Labels:    gatewayLabels(cp),
		},
		Spec: egv1alpha1.SecurityPolicySpec{
			PolicyTargetReferences: egv1alpha1.PolicyTargetReferences{
				TargetRefs: []gwapiv1.LocalPolicyTargetReferenceWithSectionName{target},
			},
			JWT: &egv1alpha1.JWT{
				Providers: []egv1alpha1.JWTProvider{{
					Name: jwtProviderName,
					RemoteJWKS: &egv1alpha1.RemoteJWKS{
						URI: jwksURL(cp),
					},
				}},
			},
		},
	}
}

// buildJWKSBackendTLSPolicy returns the BackendTLSPolicy that teaches
// envoy gateway to trust the per-cluster service-identity CA when
// dialing tokensmith for JWKS. Only applied when tokensmith is
// operator-managed AND mTLS is in effect; external tokensmith setups
// are the site operator's TLS-config problem (the publicly-accessible
// JWKS URL is expected to be served by a CA already in envoy's
// system trust store).
//
// The policy refers to a ConfigMap (Core support level: the
// service-identity reconciler mirrors the CA Secret's ca.crt into a
// same-named ConfigMap). Hostname matches the in-cluster Service DNS
// — that's both what envoy uses for SNI and what tokensmith's server
// cert advertises as a Subject Alternative Name.
func (r *GatewayReconciler) buildJWKSBackendTLSPolicy(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *gwapiv1.BackendTLSPolicy {
	hostname := gwapiv1.PreciseHostname(fmt.Sprintf("%s.%s.svc.cluster.local",
		ServiceTokensmith, ControlPlaneNamespace(cp)))
	section := gwapiv1.SectionName(tokensmithPortName)

	return &gwapiv1.BackendTLSPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: gatewayAPIVersion, Kind: kindBackendTLSPolicy},
		ObjectMeta: metav1.ObjectMeta{
			Name:      jwksBackendTLSPolicyName,
			Namespace: ControlPlaneNamespace(cp),
			Labels:    gatewayLabels(cp),
		},
		Spec: gwapiv1.BackendTLSPolicySpec{
			TargetRefs: []gwapiv1.LocalPolicyTargetReferenceWithSectionName{{
				LocalPolicyTargetReference: gwapiv1.LocalPolicyTargetReference{
					Group: "",
					Kind:  kindService,
					Name:  gwapiv1.ObjectName(ServiceTokensmith),
				},
				// Targets the named port on the Service. tokensmith
				// is single-port (tokensmithPortName), so this also
				// works as a no-op when sectionName-less, but being
				// explicit means a future second port can be added
				// without retargeting.
				SectionName: &section,
			}},
			Validation: gwapiv1.BackendTLSPolicyValidation{
				CACertificateRefs: []gwapiv1.LocalObjectReference{{
					Group: "",
					Kind:  "ConfigMap",
					Name:  gwapiv1.ObjectName(ServiceIdentityCAConfigMapName(cp)),
				}},
				Hostname: hostname,
			},
		},
	}
}

func (r *GatewayReconciler) buildSMDRateLimit(cp *openchamiv1alpha1.OpenCHAMIControlPlane) *egv1alpha1.BackendTrafficPolicy {
	target := httpRouteTargetRef(routeSMD)
	headerType := egv1alpha1.HeaderMatchDistinct
	rateLimitType := egv1alpha1.GlobalRateLimitType
	return &egv1alpha1.BackendTrafficPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: envoyGatewayAPIVersion, Kind: kindBackendTrafficPolicy},
		ObjectMeta: metav1.ObjectMeta{
			Name:      policySMDRateLimit,
			Namespace: ControlPlaneNamespace(cp),
			Labels:    gatewayLabels(cp),
		},
		Spec: egv1alpha1.BackendTrafficPolicySpec{
			PolicyTargetReferences: egv1alpha1.PolicyTargetReferences{
				TargetRefs: []gwapiv1.LocalPolicyTargetReferenceWithSectionName{target},
			},
			RateLimit: &egv1alpha1.RateLimitSpec{
				Type: &rateLimitType,
				Global: &egv1alpha1.GlobalRateLimit{
					Rules: []egv1alpha1.RateLimitRule{{
						ClientSelectors: []egv1alpha1.RateLimitSelectCondition{{
							Headers: []egv1alpha1.HeaderMatch{{
								Type: &headerType,
								Name: rateLimitHeaderName,
							}},
						}},
						Limit: egv1alpha1.RateLimitValue{
							Requests: rateLimitRequests,
							Unit:     egv1alpha1.RateLimitUnitMinute,
						},
					}},
				},
			},
		},
	}
}
