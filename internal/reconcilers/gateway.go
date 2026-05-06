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
	routeMetadataPublic = "metadata-public"
	routeMetadataAdmin  = "metadata-admin"
	policyJWTSMD        = "jwt-smd"
	policyJWTBoot       = "jwt-boot"
	policyJWTMetaAdmin  = "jwt-metadata-admin"
	policySMDRateLimit  = "smd-ratelimit"
	rateLimitHeaderName = "X-User-ID"
	rateLimitRequests   = uint(1000)
)

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
func (r *GatewayReconciler) Reconcile(ctx context.Context, cluster *openchamiv1alpha1.OpenCHAMICluster) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cluster, "gateway")

	if !apimeta.IsStatusConditionTrue(cluster.Status.Conditions, conditions.ConditionCertificatesValid) {
		log.Info("waiting for certificate before applying gateway")
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionGatewayReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonAwaitingCertificate,
			Message:            "waiting for CertificatesValid before applying gateway",
			ObservedGeneration: cluster.Generation,
		})
		return ctrl.Result{RequeueAfter: gatewayRequeueAfter}, nil
	}

	objs := r.objectsToApply(cluster)
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

	current := &gwapiv1.Gateway{}
	getErr := r.Client.Get(ctx, types.NamespacedName{
		Namespace: ClusterNamespace(cluster), Name: gatewayName,
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
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionGatewayReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonNotProgrammed,
			Message:            "waiting for Gateway Programmed=True from Envoy Gateway",
			ObservedGeneration: cluster.Generation,
		})
		return ctrl.Result{RequeueAfter: gatewayRequeueAfter}, nil
	}

	apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionGatewayReady,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            "Envoy Gateway reports Programmed=True",
		ObservedGeneration: cluster.Generation,
	})
	return ctrl.Result{}, nil
}

// Describe returns the Kubernetes objects this reconciler would apply.
func (r *GatewayReconciler) Describe(cluster *openchamiv1alpha1.OpenCHAMICluster) ([]client.Object, error) {
	return r.objectsToApply(cluster), nil
}

// objectsToApply returns the canonical Gateway, HTTPRoute, SecurityPolicy,
// and BackendTrafficPolicy set in deterministic apply order.
//
// HTTPRoute backends point at in-namespace Services that the operator
// produces. When a service has spec.services.<svc>.externalEndpoint set,
// the operator does not deploy that Service, so we skip the corresponding
// HTTPRoute and any policies that target it. Sites using externalEndpoint
// are expected to terminate routing for those services externally.
func (r *GatewayReconciler) objectsToApply(cluster *openchamiv1alpha1.OpenCHAMICluster) []client.Object {
	objs := []client.Object{
		r.buildGateway(cluster),
		r.buildHTTPRedirectRoute(cluster),
	}
	if ServiceDeployedInCluster(cluster, ServiceSMD) {
		objs = append(objs,
			r.buildSMDRoute(cluster),
			r.buildSecurityPolicy(cluster, policyJWTSMD, routeSMD),
			r.buildSMDRateLimit(cluster),
		)
	}
	if ServiceDeployedInCluster(cluster, ServiceTokensmith) {
		objs = append(objs, r.buildTokensmithRoute(cluster))
	}
	if ServiceDeployedInCluster(cluster, ServiceBootService) {
		objs = append(objs,
			r.buildBootServiceRoute(cluster),
			r.buildSecurityPolicy(cluster, policyJWTBoot, routeBootService),
		)
	}
	if ServiceDeployedInCluster(cluster, ServiceMetadataService) {
		objs = append(objs,
			r.buildMetadataPublicRoute(cluster),
			r.buildMetadataAdminRoute(cluster),
			r.buildSecurityPolicy(cluster, policyJWTMetaAdmin, routeMetadataAdmin),
		)
	}
	return objs
}

// gatewayLabels returns the canonical labels for gateway-managed resources.
func gatewayLabels(cluster *openchamiv1alpha1.OpenCHAMICluster) map[string]string {
	return map[string]string{
		labelAppName:   "gateway",
		labelAppInst:   "openchami-" + cluster.Spec.ClusterName,
		labelManagedBy: managedByValue,
	}
}

// gatewayClassName returns the configured gatewayClass with a sensible default.
func gatewayClassName(cluster *openchamiv1alpha1.OpenCHAMICluster) string {
	if gc := cluster.Spec.Networking.GatewayClass; gc != "" {
		return gc
	}
	return "envoy"
}

// jwksURL returns the JWKS URL for tokensmith — by default the in-cluster
// Service URL, or a site-provided externalEndpoint when set. SecurityPolicy
// resources reference this directly (rather than the gateway-fronted URL)
// to avoid a routing loop when tokensmith is operator-managed.
func jwksURL(cluster *openchamiv1alpha1.OpenCHAMICluster) string {
	return ServiceURL(cluster, ServiceTokensmith) + "/.well-known/jwks.json"
}

func (r *GatewayReconciler) buildGateway(cluster *openchamiv1alpha1.OpenCHAMICluster) *gwapiv1.Gateway {
	hostname := gwapiv1.Hostname(cluster.Spec.Domain)
	tlsMode := gwapiv1.TLSModeTerminate
	secretRef := gwapiv1.SecretObjectReference{
		Name: gwapiv1.ObjectName(GatewayTLSSecretName(cluster)),
	}
	return &gwapiv1.Gateway{
		TypeMeta: metav1.TypeMeta{APIVersion: gatewayAPIVersion, Kind: kindGateway},
		ObjectMeta: metav1.ObjectMeta{
			Name:      gatewayName,
			Namespace: ClusterNamespace(cluster),
			Labels:    gatewayLabels(cluster),
		},
		Spec: gwapiv1.GatewaySpec{
			GatewayClassName: gwapiv1.ObjectName(gatewayClassName(cluster)),
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
func httpParentRef(cluster *openchamiv1alpha1.OpenCHAMICluster) gwapiv1.ParentReference {
	section := gwapiv1.SectionName(listenerHTTP)
	ns := gwapiv1.Namespace(ClusterNamespace(cluster))
	return gwapiv1.ParentReference{
		Name:        gwapiv1.ObjectName(gatewayName),
		Namespace:   &ns,
		SectionName: &section,
	}
}

// httpsParentRef returns the parentRef pointing at the HTTPS listener of the
// cluster gateway.
func httpsParentRef(cluster *openchamiv1alpha1.OpenCHAMICluster) gwapiv1.ParentReference {
	section := gwapiv1.SectionName(listenerHTTPS)
	ns := gwapiv1.Namespace(ClusterNamespace(cluster))
	return gwapiv1.ParentReference{
		Name:        gwapiv1.ObjectName(gatewayName),
		Namespace:   &ns,
		SectionName: &section,
	}
}

func (r *GatewayReconciler) buildHTTPRedirectRoute(cluster *openchamiv1alpha1.OpenCHAMICluster) *gwapiv1.HTTPRoute {
	hostname := gwapiv1.Hostname(cluster.Spec.Domain)
	scheme := "https"
	statusCode := 301
	return &gwapiv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{APIVersion: gatewayAPIVersion, Kind: kindHTTPRoute},
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeHTTPRedirect,
			Namespace: ClusterNamespace(cluster),
			Labels:    gatewayLabels(cluster),
		},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{
				ParentRefs: []gwapiv1.ParentReference{httpParentRef(cluster)},
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

func (r *GatewayReconciler) buildSMDRoute(cluster *openchamiv1alpha1.OpenCHAMICluster) *gwapiv1.HTTPRoute {
	hostname := gwapiv1.Hostname(cluster.Spec.Domain)
	return &gwapiv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{APIVersion: gatewayAPIVersion, Kind: kindHTTPRoute},
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeSMD,
			Namespace: ClusterNamespace(cluster),
			Labels:    gatewayLabels(cluster),
		},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{
				ParentRefs: []gwapiv1.ParentReference{httpsParentRef(cluster)},
			},
			Hostnames: []gwapiv1.Hostname{hostname},
			Rules:     []gwapiv1.HTTPRouteRule{pathPrefixRule("/hsm", ServiceSMD, smdPort)},
		},
	}
}

func (r *GatewayReconciler) buildTokensmithRoute(cluster *openchamiv1alpha1.OpenCHAMICluster) *gwapiv1.HTTPRoute {
	hostname := gwapiv1.Hostname(cluster.Spec.Domain)
	return &gwapiv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{APIVersion: gatewayAPIVersion, Kind: kindHTTPRoute},
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeTokensmith,
			Namespace: ClusterNamespace(cluster),
			Labels:    gatewayLabels(cluster),
		},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{
				ParentRefs: []gwapiv1.ParentReference{httpsParentRef(cluster)},
			},
			Hostnames: []gwapiv1.Hostname{hostname},
			Rules: []gwapiv1.HTTPRouteRule{
				exactPathRule("/.well-known/jwks.json", ServiceTokensmith, tokensmithPort),
				exactPathRule("/oauth/token", ServiceTokensmith, tokensmithPort),
				exactPathRule("/health", ServiceTokensmith, tokensmithPort),
			},
		},
	}
}

func (r *GatewayReconciler) buildBootServiceRoute(cluster *openchamiv1alpha1.OpenCHAMICluster) *gwapiv1.HTTPRoute {
	hostname := gwapiv1.Hostname(cluster.Spec.Domain)
	return &gwapiv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{APIVersion: gatewayAPIVersion, Kind: kindHTTPRoute},
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeBootService,
			Namespace: ClusterNamespace(cluster),
			Labels:    gatewayLabels(cluster),
		},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{
				ParentRefs: []gwapiv1.ParentReference{httpsParentRef(cluster)},
			},
			Hostnames: []gwapiv1.Hostname{hostname},
			Rules:     []gwapiv1.HTTPRouteRule{pathPrefixRule("/boot", ServiceBootService, bootServicePort)},
		},
	}
}

func (r *GatewayReconciler) buildMetadataPublicRoute(cluster *openchamiv1alpha1.OpenCHAMICluster) *gwapiv1.HTTPRoute {
	hostname := gwapiv1.Hostname(cluster.Spec.Domain)
	return &gwapiv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{APIVersion: gatewayAPIVersion, Kind: kindHTTPRoute},
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeMetadataPublic,
			Namespace: ClusterNamespace(cluster),
			Labels:    gatewayLabels(cluster),
		},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{
				ParentRefs: []gwapiv1.ParentReference{httpsParentRef(cluster)},
			},
			Hostnames: []gwapiv1.Hostname{hostname},
			Rules:     []gwapiv1.HTTPRouteRule{pathPrefixRule("/cloud-init", ServiceMetadataService, metadataServicePort)},
		},
	}
}

func (r *GatewayReconciler) buildMetadataAdminRoute(cluster *openchamiv1alpha1.OpenCHAMICluster) *gwapiv1.HTTPRoute {
	hostname := gwapiv1.Hostname(cluster.Spec.Domain)
	return &gwapiv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{APIVersion: gatewayAPIVersion, Kind: kindHTTPRoute},
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeMetadataAdmin,
			Namespace: ClusterNamespace(cluster),
			Labels:    gatewayLabels(cluster),
		},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{
				ParentRefs: []gwapiv1.ParentReference{httpsParentRef(cluster)},
			},
			Hostnames: []gwapiv1.Hostname{hostname},
			Rules:     []gwapiv1.HTTPRouteRule{pathPrefixRule("/cloud-init/admin", ServiceMetadataService, metadataServicePort)},
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

func (r *GatewayReconciler) buildSecurityPolicy(cluster *openchamiv1alpha1.OpenCHAMICluster, name, route string) *egv1alpha1.SecurityPolicy {
	target := httpRouteTargetRef(route)
	return &egv1alpha1.SecurityPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: envoyGatewayAPIVersion, Kind: kindSecurityPolicy},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ClusterNamespace(cluster),
			Labels:    gatewayLabels(cluster),
		},
		Spec: egv1alpha1.SecurityPolicySpec{
			PolicyTargetReferences: egv1alpha1.PolicyTargetReferences{
				TargetRefs: []gwapiv1.LocalPolicyTargetReferenceWithSectionName{target},
			},
			JWT: &egv1alpha1.JWT{
				Providers: []egv1alpha1.JWTProvider{{
					Name: jwtProviderName,
					RemoteJWKS: &egv1alpha1.RemoteJWKS{
						URI: jwksURL(cluster),
					},
				}},
			},
		},
	}
}

func (r *GatewayReconciler) buildSMDRateLimit(cluster *openchamiv1alpha1.OpenCHAMICluster) *egv1alpha1.BackendTrafficPolicy {
	target := httpRouteTargetRef(routeSMD)
	headerType := egv1alpha1.HeaderMatchDistinct
	rateLimitType := egv1alpha1.GlobalRateLimitType
	return &egv1alpha1.BackendTrafficPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: envoyGatewayAPIVersion, Kind: kindBackendTrafficPolicy},
		ObjectMeta: metav1.ObjectMeta{
			Name:      policySMDRateLimit,
			Namespace: ClusterNamespace(cluster),
			Labels:    gatewayLabels(cluster),
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
