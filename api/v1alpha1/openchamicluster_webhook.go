/*
Copyright 2026 OpenCHAMI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"context"
	"fmt"
	"reflect"
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

// openchamiclusterlog is the package logger for webhook diagnostics.
var openchamiclusterlog = logf.Log.WithName("openchamicluster-webhook")

// gvkOpenCHAMICluster is the GroupVersionKind used in field.Duplicate errors.
var gvkOpenCHAMICluster = schema.GroupVersionKind{
	Group:   "openchami.openchami.org",
	Version: "v1alpha1",
	Kind:    "OpenCHAMICluster",
}

// OpenCHAMIClusterWebhook implements both webhook.CustomDefaulter and
// webhook.CustomValidator (via the typed admission.Defaulter/Validator
// interfaces in controller-runtime 0.23.x).
//
// It carries a client.Client so that cluster-wide uniqueness checks
// (ClusterName, CoreDHCP nodeSelector) can list every OpenCHAMICluster
// at admission time.
//
// +kubebuilder:object:generate=false
type OpenCHAMIClusterWebhook struct {
	Client client.Client
}

// SetupOpenCHAMIClusterWebhookWithManager registers both the defaulting
// and validating webhooks for OpenCHAMICluster with the supplied manager.
// Wired from cmd/operator/main.go.
func SetupOpenCHAMIClusterWebhookWithManager(mgr ctrl.Manager) error {
	w := &OpenCHAMIClusterWebhook{Client: mgr.GetClient()}
	return ctrl.NewWebhookManagedBy(mgr, &OpenCHAMICluster{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-openchami-openchami-org-v1alpha1-openchamicluster,mutating=true,failurePolicy=fail,sideEffects=None,groups=openchami.openchami.org,resources=openchamiclusters,verbs=create;update,versions=v1alpha1,name=mopenchamicluster.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-openchami-openchami-org-v1alpha1-openchamicluster,mutating=false,failurePolicy=fail,sideEffects=None,groups=openchami.openchami.org,resources=openchamiclusters,verbs=create;update,versions=v1alpha1,name=vopenchamicluster.kb.io,admissionReviewVersions=v1

// ----- Defaulting --------------------------------------------------------

// Default fills in derived defaults (those that depend on clusterName) and
// belt-and-braces literal defaults that are also covered by
// +kubebuilder:default markers on the type. Only zero/empty fields are set;
// user-supplied values are never overwritten.
func (w *OpenCHAMIClusterWebhook) Default(_ context.Context, obj *OpenCHAMICluster) error {
	openchamiclusterlog.Info("defaulting OpenCHAMICluster", "name", obj.GetName())

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
func (w *OpenCHAMIClusterWebhook) ValidateCreate(ctx context.Context, obj *OpenCHAMICluster) (admission.Warnings, error) {
	openchamiclusterlog.Info("validating OpenCHAMICluster create", "name", obj.GetName())
	return w.validate(ctx, obj)
}

// ValidateUpdate enforces immutability of clusterName and re-runs the
// create-time validations against the new spec.
func (w *OpenCHAMIClusterWebhook) ValidateUpdate(ctx context.Context, oldObj, newObj *OpenCHAMICluster) (admission.Warnings, error) {
	openchamiclusterlog.Info("validating OpenCHAMICluster update", "name", newObj.GetName())

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
			gvkOpenCHAMICluster.GroupKind(),
			newObj.GetName(),
			allErrs,
		)
	}
	return warnings, nil
}

// ValidateDelete has no invariants beyond the controller's finalizer.
func (w *OpenCHAMIClusterWebhook) ValidateDelete(_ context.Context, obj *OpenCHAMICluster) (admission.Warnings, error) {
	openchamiclusterlog.Info("validating OpenCHAMICluster delete", "name", obj.GetName())
	return nil, nil
}

// validate runs the shared create/update validation rules.
func (w *OpenCHAMIClusterWebhook) validate(ctx context.Context, obj *OpenCHAMICluster) (admission.Warnings, error) {
	var (
		warnings admission.Warnings
		allErrs  field.ErrorList
	)
	specPath := field.NewPath("spec")

	// 1. Vault address must be https://.
	vaultPath := specPath.Child("platform", "vault", "address")
	if !strings.HasPrefix(obj.Spec.Platform.Vault.Address, "https://") {
		allErrs = append(allErrs, field.Invalid(
			vaultPath,
			obj.Spec.Platform.Vault.Address,
			"vault address must use https:// scheme",
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

	// 4. ClusterName uniqueness across the cluster.
	others, listErr := w.listOtherClusters(ctx, obj)
	if listErr != nil {
		// Don't fail admission on a transient list error; log and skip
		// uniqueness checks. Other invariants are still enforced.
		openchamiclusterlog.Error(listErr, "listing OpenCHAMIClusters for uniqueness check; skipping")
	} else {
		for i := range others {
			if others[i].Spec.ClusterName == obj.Spec.ClusterName {
				allErrs = append(allErrs, field.Duplicate(
					specPath.Child("clusterName"),
					obj.Spec.ClusterName,
				))
				break
			}
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
			if listErr == nil {
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

	if len(allErrs) > 0 {
		return warnings, apierrors.NewInvalid(
			gvkOpenCHAMICluster.GroupKind(),
			obj.GetName(),
			allErrs,
		)
	}
	return warnings, nil
}

// listOtherClusters returns every OpenCHAMICluster on the API server except
// the one being validated (compared by UID). The Client is the manager's
// cached reader.
func (w *OpenCHAMIClusterWebhook) listOtherClusters(ctx context.Context, self *OpenCHAMICluster) ([]OpenCHAMICluster, error) {
	if w.Client == nil {
		return nil, nil
	}
	var list OpenCHAMIClusterList
	if err := w.Client.List(ctx, &list); err != nil {
		return nil, err
	}
	out := make([]OpenCHAMICluster, 0, len(list.Items))
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

// nodeSelectorHasClusterDiscriminator reports whether at least one value in
// the given selector map contains the cluster name as a substring. The
// discriminator requirement prevents two clusters from accidentally
// targeting the same nodes when probing is disabled.
func nodeSelectorHasClusterDiscriminator(selector map[string]string, clusterName string) bool {
	if clusterName == "" {
		return false
	}
	for _, v := range selector {
		if strings.Contains(v, clusterName) {
			return true
		}
	}
	return false
}
