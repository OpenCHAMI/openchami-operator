// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Canonical pinning keys for Spec.Images.Pinned. The strings are the
// service short-names the operator uses everywhere (label values,
// Service names, namespace suffixes). Mirrored from
// internal/reconcilers/helpers.go — duplicated here on purpose so the
// API package has no dependency on the controller package. If you add
// a new pinned-image service, update both places + builtInImages in
// internal/reconcilers/images.go.
const (
	pinKeySMD             = "smd"
	pinKeyTokensmith      = "tokensmith"
	pinKeyBootService     = "boot-service"
	pinKeyMetadataService = "metadata-service"
	pinKeyCoreDHCP        = "coredhcp"
	pinKeyMagellan        = "magellan"
	pinKeyNetworkProbe    = "network-probe"
	pinKeyFunicular       = "funicular-collector"
)

// enabledServicesForImagePinning returns the canonical names of every
// service this control plane has enabled (and therefore needs a Pinned
// entry for when Stream=pinned). Order is irrelevant for correctness
// but kept stable for legible error messages.
func enabledServicesForImagePinning(cp *OpenCHAMIControlPlane) []string {
	var names []string
	if cp.Spec.Services.SMD.Enabled {
		names = append(names, pinKeySMD)
	}
	if cp.Spec.Services.Tokensmith.Enabled {
		names = append(names, pinKeyTokensmith)
	}
	if cp.Spec.Services.BootService.Enabled {
		names = append(names, pinKeyBootService)
	}
	if cp.Spec.Services.MetadataService.Enabled {
		names = append(names, pinKeyMetadataService)
	}
	if cp.Spec.Services.CoreDHCP.Enabled {
		names = append(names, pinKeyCoreDHCP)
	}
	if cp.Spec.Services.Magellan.Enabled {
		names = append(names, pinKeyMagellan)
	}
	if cp.Spec.NetworkProbe.Enabled {
		names = append(names, pinKeyNetworkProbe)
	}
	if cp.Spec.Logging.Enabled {
		names = append(names, pinKeyFunicular)
	}
	return names
}

// openchamicontrolplanelog is the package logger for webhook diagnostics.
var openchamicontrolplanelog = logf.Log.WithName("openchamicontrolplane-webhook")

// gvkOpenCHAMIControlPlane is the GroupVersionKind used in field.Duplicate errors.
var gvkOpenCHAMIControlPlane = schema.GroupVersionKind{
	Group:   "openchami.openchami.org",
	Version: "v1alpha1",
	Kind:    "OpenCHAMIControlPlane",
}

// OpenCHAMIControlPlaneWebhook implements both webhook.CustomDefaulter and
// webhook.CustomValidator (via the typed admission.Defaulter/Validator
// interfaces in controller-runtime 0.23.x).
//
// It carries a client.Client so that cluster-wide uniqueness checks
// (ClusterName, CoreDHCP nodeSelector) can list every OpenCHAMIControlPlane
// at admission time.
//
// +kubebuilder:object:generate=false
type OpenCHAMIControlPlaneWebhook struct {
	Client client.Client
}

// SetupOpenCHAMIControlPlaneWebhookWithManager registers both the defaulting
// and validating webhooks for OpenCHAMIControlPlane with the supplied manager.
// Wired from cmd/operator/main.go.
func SetupOpenCHAMIControlPlaneWebhookWithManager(mgr ctrl.Manager) error {
	w := &OpenCHAMIControlPlaneWebhook{Client: mgr.GetClient()}
	return ctrl.NewWebhookManagedBy(mgr, &OpenCHAMIControlPlane{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-openchami-openchami-org-v1alpha1-openchamicontrolplane,mutating=true,failurePolicy=fail,sideEffects=None,groups=openchami.openchami.org,resources=openchamicontrolplanes,verbs=create;update,versions=v1alpha1,name=mopenchamicontrolplane.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-openchami-openchami-org-v1alpha1-openchamicontrolplane,mutating=false,failurePolicy=fail,sideEffects=None,groups=openchami.openchami.org,resources=openchamicontrolplanes,verbs=create;update,versions=v1alpha1,name=vopenchamicontrolplane.kb.io,admissionReviewVersions=v1

// ----- Defaulting --------------------------------------------------------

// Default fills in derived defaults (those that depend on clusterName) and
// belt-and-braces literal defaults that are also covered by
// +kubebuilder:default markers on the type. Only zero/empty fields are set;
// user-supplied values are never overwritten.
func (w *OpenCHAMIControlPlaneWebhook) Default(_ context.Context, obj *OpenCHAMIControlPlane) error {
	openchamicontrolplanelog.Info("defaulting OpenCHAMIControlPlane", "name", obj.GetName())

	name := obj.Spec.ClusterName

	// Derived defaults — must always be applied because they depend on clusterName.
	if obj.Spec.Platform.ObjectStorage.Bucket == "" && name != "" {
		obj.Spec.Platform.ObjectStorage.Bucket = name + "-boot-images"
	}
	if obj.Spec.Logging.LogBucket == "" && name != "" {
		obj.Spec.Logging.LogBucket = name + "-logs"
	}
	if obj.Spec.Networking.TLS.SecretName == "" && name != "" {
		obj.Spec.Networking.TLS.SecretName = name + "-gateway-tls"
	}

	// Literal defaults — duplicated from +kubebuilder:default markers in case
	// the API server skips defaulting (CRD-only installations, conversion paths).
	if obj.Spec.Networking.GatewayClass == "" {
		obj.Spec.Networking.GatewayClass = "envoy"
	}
	if obj.Spec.Networking.TLS.Issuer == "" {
		obj.Spec.Networking.TLS.Issuer = "vault-pki-issuer"
	}
	if obj.Spec.Database.Instances == 0 {
		obj.Spec.Database.Instances = 3
	}
	if obj.Spec.NetworkProbe.IntervalSeconds == 0 {
		obj.Spec.NetworkProbe.IntervalSeconds = 300
	}
	if obj.Spec.Logging.RetentionDays == 0 {
		obj.Spec.Logging.RetentionDays = 90
	}
	if obj.Spec.Logging.FlushIntervalSeconds == 0 {
		obj.Spec.Logging.FlushIntervalSeconds = 60
	}

	// Per-service replicas. Tokensmith stays at 1 (single-process by design;
	// the reconciler ignores Replicas anyway, so this is just for clarity).
	if obj.Spec.Services.SMD.Replicas == 0 {
		obj.Spec.Services.SMD.Replicas = 2
	}
	if obj.Spec.Services.Tokensmith.Replicas == 0 {
		obj.Spec.Services.Tokensmith.Replicas = 1
	}
	if obj.Spec.Services.BootService.Replicas == 0 {
		obj.Spec.Services.BootService.Replicas = 2
	}
	if obj.Spec.Services.MetadataService.Replicas == 0 {
		obj.Spec.Services.MetadataService.Replicas = 2
	}

	// CoreDHCP / Magellan literals.
	if obj.Spec.Services.CoreDHCP.UnknownLeaseDuration == "" {
		obj.Spec.Services.CoreDHCP.UnknownLeaseDuration = "5m"
	}
	if obj.Spec.Services.CoreDHCP.KnownLeaseDuration == "" {
		obj.Spec.Services.CoreDHCP.KnownLeaseDuration = "1h"
	}
	if obj.Spec.Services.Magellan.Schedule == "" {
		obj.Spec.Services.Magellan.Schedule = "*/30 * * * *"
	}
	if obj.Spec.Services.Magellan.ConcurrencyPolicy == "" {
		obj.Spec.Services.Magellan.ConcurrencyPolicy = batchv1.ForbidConcurrent
	}

	return nil
}

// ----- Validation --------------------------------------------------------

// ValidateCreate runs all admission checks for a newly submitted resource.
func (w *OpenCHAMIControlPlaneWebhook) ValidateCreate(ctx context.Context, obj *OpenCHAMIControlPlane) (admission.Warnings, error) {
	openchamicontrolplanelog.Info("validating OpenCHAMIControlPlane create", "name", obj.GetName())
	return w.validate(ctx, obj)
}

// ValidateUpdate enforces immutability of clusterName and re-runs the
// create-time validations against the new spec.
func (w *OpenCHAMIControlPlaneWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj *OpenCHAMIControlPlane) (admission.Warnings, error) {
	openchamicontrolplanelog.Info("validating OpenCHAMIControlPlane update", "name", newObj.GetName())

	var allErrs field.ErrorList
	if oldObj.Spec.ClusterName != newObj.Spec.ClusterName {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec").Child("clusterName"),
			"clusterName is immutable after creation",
		))
	}

	warnings, err := w.validate(ctx, newObj)
	if err != nil {
		// validate() already returns an apierrors.Invalid; merge our extra errors in.
		if statusErr, ok := err.(*apierrors.StatusError); ok && statusErr.ErrStatus.Details != nil {
			for _, c := range statusErr.ErrStatus.Details.Causes {
				allErrs = append(allErrs, &field.Error{
					Type:     field.ErrorType(c.Type),
					Field:    c.Field,
					BadValue: "",
					Detail:   c.Message,
				})
			}
		} else {
			return warnings, err
		}
	}

	if len(allErrs) > 0 {
		return warnings, apierrors.NewInvalid(
			gvkOpenCHAMIControlPlane.GroupKind(),
			newObj.GetName(),
			allErrs,
		)
	}
	return warnings, nil
}

// ValidateDelete has no invariants beyond the controller's finalizer.
func (w *OpenCHAMIControlPlaneWebhook) ValidateDelete(_ context.Context, obj *OpenCHAMIControlPlane) (admission.Warnings, error) {
	openchamicontrolplanelog.Info("validating OpenCHAMIControlPlane delete", "name", obj.GetName())
	return nil, nil
}

// validate runs the shared create/update validation rules.
func (w *OpenCHAMIControlPlaneWebhook) validate(ctx context.Context, obj *OpenCHAMIControlPlane) (admission.Warnings, error) {
	var (
		warnings admission.Warnings
		allErrs  field.ErrorList
	)
	specPath := field.NewPath("spec")

	// 1. Vault address must be https:// — except for dev-mode loopback
	// addresses (localhost / 127.0.0.1 / ::1), where http:// is permitted
	// because Vault dev-mode runs without TLS and the dev shakedown
	// pipeline relies on it. Production endpoints (any non-loopback host)
	// still require https://.
	vaultPath := specPath.Child("platform", "vault", "address")
	if !isAllowedVaultAddress(obj.Spec.Platform.Vault.Address) {
		allErrs = append(allErrs, field.Invalid(
			vaultPath,
			obj.Spec.Platform.Vault.Address,
			"vault address must use https:// (http:// is allowed only for localhost/127.0.0.1/::1)",
		))
	}

	// 2. AppRole auth requires AppRoleSecretRef.
	if obj.Spec.Platform.Vault.AuthMethod == VaultAuthMethodAppRole &&
		obj.Spec.Platform.Vault.AppRoleSecretRef == nil {
		allErrs = append(allErrs, field.Required(
			specPath.Child("platform", "vault", "appRoleSecretRef"),
			"appRoleSecretRef is required when authMethod is appRole",
		))
	}

	// 3. External OIDC provider requires OIDCIssuerURL.
	if obj.Spec.Services.Tokensmith.OIDCProvider == "external" &&
		obj.Spec.Services.Tokensmith.OIDCIssuerURL == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("services", "tokensmith", "oidcIssuerURL"),
			"oidcIssuerURL is required when oidcProvider is external",
		))
	}

	// 3a. ExternalEndpoint validation for the four HTTP services that
	// support it. Setting an external endpoint requires Enabled=false
	// (operator must not produce both an in-cluster Service and an
	// external pointer for the same service) and the URL must be a
	// well-formed http(s) URL.
	for _, svc := range externallyOverridableServices(obj) {
		if svc.External == nil {
			continue
		}
		p := specPath.Child("services", svc.Name, "externalEndpoint")
		if svc.Enabled {
			allErrs = append(allErrs, field.Forbidden(
				p,
				"externalEndpoint requires services."+svc.Name+".enabled=false",
			))
		}
		if !isHTTPURL(*svc.External) {
			allErrs = append(allErrs, field.Invalid(
				p,
				*svc.External,
				"externalEndpoint must be a valid http:// or https:// URL",
			))
		}
	}

	// 3b. CoreDHCP and Magellan do not support an externalEndpoint —
	// DHCP is layer-2/3 and Magellan is a CronJob; neither is consumed
	// over HTTP. Reject the field outright.
	// (Currently their specs do not embed ServiceDefaults, so the field
	// is structurally absent; this guard catches a future regression.)

	// 4. ClusterName uniqueness across the cluster.
	// Fail closed: invariants #2 (namespace isolation) and #4 (DHCP node
	// exclusivity) both depend on this check. A transient list failure
	// becomes a transient admission failure (failurePolicy=Fail) — the
	// user retries — which is vastly safer than admitting a possible
	// duplicate.
	others, listErr := w.listOtherClusters(ctx, obj)
	if listErr != nil {
		return nil, fmt.Errorf("listing OpenCHAMIControlPlanes for uniqueness check: %w", listErr)
	}
	for i := range others {
		if others[i].Spec.ClusterName == obj.Spec.ClusterName {
			allErrs = append(allErrs, field.Duplicate(
				specPath.Child("clusterName"),
				obj.Spec.ClusterName,
			))
			break
		}
	}

	// CoreDHCP / NetworkProbe coupling.
	dhcpPath := specPath.Child("services", "coreDHCP")
	probePath := specPath.Child("networkProbe")

	if !obj.Spec.NetworkProbe.Enabled {
		// 5. nodeSelector required when probing is off.
		if len(obj.Spec.Services.CoreDHCP.NodeSelector) == 0 {
			allErrs = append(allErrs, field.Required(
				dhcpPath.Child("nodeSelector"),
				"coreDHCP.nodeSelector is required when networkProbe.enabled is false",
			))
		} else {
			// 6. nodeSelector must contain a discriminator with the cluster name.
			if !nodeSelectorHasClusterDiscriminator(obj.Spec.Services.CoreDHCP.NodeSelector, obj.Spec.ClusterName) {
				allErrs = append(allErrs, field.Invalid(
					dhcpPath.Child("nodeSelector"),
					obj.Spec.Services.CoreDHCP.NodeSelector,
					fmt.Sprintf("at least one nodeSelector value must contain the clusterName %q to prevent overlap with other clusters", obj.Spec.ClusterName),
				))
			}

			// 7. nodeSelector must be unique across clusters.
			for i := range others {
				if reflect.DeepEqual(others[i].Spec.Services.CoreDHCP.NodeSelector, obj.Spec.Services.CoreDHCP.NodeSelector) {
					allErrs = append(allErrs, field.Duplicate(
						dhcpPath.Child("nodeSelector"),
						obj.Spec.Services.CoreDHCP.NodeSelector,
					))
					break
				}
			}
		}
	} else {
		// 8. Probe + DHCP requires provisionNetwork.
		if obj.Spec.Services.CoreDHCP.Enabled && obj.Spec.NetworkProbe.ProvisionNetwork == nil {
			allErrs = append(allErrs, field.Required(
				probePath.Child("provisionNetwork"),
				"networkProbe.provisionNetwork is required when coreDHCP is enabled with networkProbe",
			))
		}
		// 9. Probe + Magellan requires bmcNetwork.
		if obj.Spec.Services.Magellan.Enabled && obj.Spec.NetworkProbe.BMCNetwork == nil {
			allErrs = append(allErrs, field.Required(
				probePath.Child("bmcNetwork"),
				"networkProbe.bmcNetwork is required when magellan is enabled with networkProbe",
			))
		}
		// 10. Warn if nodeSelector is set while probing is on.
		if len(obj.Spec.Services.CoreDHCP.NodeSelector) > 0 {
			warnings = append(warnings,
				"spec.services.coreDHCP.nodeSelector is ignored when networkProbe.enabled is true; the operator generates the selector from probe labels")
		}
	}

	// 11. Stream=pinned requires Images.Pinned to carry an entry for
	// every enabled service the operator manages images for. Caught
	// here so the user gets a single legible error at admission time
	// rather than letting the runtime fall back to release-stream tags
	// silently for missing entries.
	if obj.Spec.Images.Stream == ImageStreamPinned {
		needed := enabledServicesForImagePinning(obj)
		var missing []string
		for _, name := range needed {
			if obj.Spec.Images.Pinned[name] == "" {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			allErrs = append(allErrs, field.Required(
				specPath.Child("images", "pinned"),
				"stream=pinned requires an entry per enabled service; missing: "+strings.Join(missing, ", "),
			))
		}
	}

	if len(allErrs) > 0 {
		return warnings, apierrors.NewInvalid(
			gvkOpenCHAMIControlPlane.GroupKind(),
			obj.GetName(),
			allErrs,
		)
	}
	return warnings, nil
}

// listOtherClusters returns every OpenCHAMIControlPlane on the API server except
// the one being validated (compared by UID). The Client is the manager's
// cached reader.
func (w *OpenCHAMIControlPlaneWebhook) listOtherClusters(ctx context.Context, self *OpenCHAMIControlPlane) ([]OpenCHAMIControlPlane, error) {
	if w.Client == nil {
		return nil, nil
	}
	var list OpenCHAMIControlPlaneList
	if err := w.Client.List(ctx, &list); err != nil {
		return nil, err
	}
	out := make([]OpenCHAMIControlPlane, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].UID != "" && list.Items[i].UID == self.UID {
			continue
		}
		// Belt-and-braces: when self has no UID yet (create) skip exact name+namespace match.
		if self.UID == "" &&
			list.Items[i].Name == self.Name &&
			list.Items[i].Namespace == self.Namespace {
			continue
		}
		out = append(out, list.Items[i])
	}
	return out, nil
}

// nodeSelectorHasClusterDiscriminator reports whether at least one **key** in
// the given selector map contains the cluster name as a substring. The
// canonical convention is `openchami.org/<clusterName>-<probe>-network-ready`
// (keys, not values — values are typically the literal "true"). The
// discriminator requirement prevents two clusters from accidentally targeting
// the same nodes when probing is disabled. Values are also accepted as a
// secondary path for operators using a non-canonical key like
// `node-role.kubernetes.io/<clusterName>` where the discriminator naturally
// lives in the key already (covered by the key-substring check) — or schemes
// like `cluster: <clusterName>` where the discriminator is in the value.
// Either form satisfies the constraint.
func nodeSelectorHasClusterDiscriminator(selector map[string]string, clusterName string) bool {
	if clusterName == "" {
		return false
	}
	for k, v := range selector {
		if strings.Contains(k, clusterName) || strings.Contains(v, clusterName) {
			return true
		}
	}
	return false
}

// isAllowedVaultAddress reports whether addr satisfies the operator's vault
// address policy: https:// is always allowed; http:// is allowed only when
// the host portion is a loopback address (localhost, 127.0.0.1, or ::1)
// because the dev-mode shakedown stack uses plain HTTP. Production endpoints
// always end up via the https:// path.
func isAllowedVaultAddress(addr string) bool {
	if strings.HasPrefix(addr, "https://") {
		return true
	}
	if !strings.HasPrefix(addr, "http://") {
		return false
	}
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// externalServiceRef captures the per-service flags the webhook needs to
// validate ServiceDefaults.ExternalEndpoint for the four HTTP services that
// support it. CoreDHCP and Magellan are deliberately omitted (their specs
// don't embed ServiceDefaults).
type externalServiceRef struct {
	Name     string
	Enabled  bool
	External *string
}

func externallyOverridableServices(obj *OpenCHAMIControlPlane) []externalServiceRef {
	s := &obj.Spec.Services
	return []externalServiceRef{
		{Name: pinKeySMD, Enabled: s.SMD.Enabled, External: s.SMD.ExternalEndpoint},
		{Name: pinKeyTokensmith, Enabled: s.Tokensmith.Enabled, External: s.Tokensmith.ExternalEndpoint},
		{Name: "bootService", Enabled: s.BootService.Enabled, External: s.BootService.ExternalEndpoint},
		{Name: "metadataService", Enabled: s.MetadataService.Enabled, External: s.MetadataService.ExternalEndpoint},
	}
}

// isHTTPURL reports whether s parses as an http:// or https:// URL with a
// non-empty host. Used by the webhook to validate externalEndpoint fields.
func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Host != ""
}
