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

	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

// nolint:unused
// log is for logging in this package.
var openchamiclusterlog = logf.Log.WithName("openchamicluster-resource")

// SetupOpenCHAMIClusterWebhookWithManager registers the webhook for OpenCHAMICluster in the manager.
func SetupOpenCHAMIClusterWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &openchamiv1alpha1.OpenCHAMICluster{}).
		WithValidator(&OpenCHAMIClusterCustomValidator{}).
		WithDefaulter(&OpenCHAMIClusterCustomDefaulter{}).
		Complete()
}

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

// +kubebuilder:webhook:path=/mutate-openchami-openchami-org-v1alpha1-openchamicluster,mutating=true,failurePolicy=fail,sideEffects=None,groups=openchami.openchami.org,resources=openchamiclusters,verbs=create;update,versions=v1alpha1,name=mopenchamicluster-v1alpha1.kb.io,admissionReviewVersions=v1

// OpenCHAMIClusterCustomDefaulter struct is responsible for setting default values on the custom resource of the
// Kind OpenCHAMICluster when those are created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type OpenCHAMIClusterCustomDefaulter struct {
	// TODO(user): Add more fields as needed for defaulting
}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the Kind OpenCHAMICluster.
func (d *OpenCHAMIClusterCustomDefaulter) Default(_ context.Context, obj *openchamiv1alpha1.OpenCHAMICluster) error {
	openchamiclusterlog.Info("Defaulting for OpenCHAMICluster", "name", obj.GetName())

	// TODO(user): fill in your defaulting logic.

	return nil
}

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: If you want to customise the 'path', use the flags '--defaulting-path' or '--validation-path'.
// +kubebuilder:webhook:path=/validate-openchami-openchami-org-v1alpha1-openchamicluster,mutating=false,failurePolicy=fail,sideEffects=None,groups=openchami.openchami.org,resources=openchamiclusters,verbs=create;update,versions=v1alpha1,name=vopenchamicluster-v1alpha1.kb.io,admissionReviewVersions=v1

// OpenCHAMIClusterCustomValidator struct is responsible for validating the OpenCHAMICluster resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type OpenCHAMIClusterCustomValidator struct {
	// TODO(user): Add more fields as needed for validation
}

// ValidateCreate implements webhook.CustomValidator so a webhook will be registered for the type OpenCHAMICluster.
func (v *OpenCHAMIClusterCustomValidator) ValidateCreate(_ context.Context, obj *openchamiv1alpha1.OpenCHAMICluster) (admission.Warnings, error) {
	openchamiclusterlog.Info("Validation for OpenCHAMICluster upon creation", "name", obj.GetName())

	// TODO(user): fill in your validation logic upon object creation.

	return nil, nil
}

// ValidateUpdate implements webhook.CustomValidator so a webhook will be registered for the type OpenCHAMICluster.
func (v *OpenCHAMIClusterCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj *openchamiv1alpha1.OpenCHAMICluster) (admission.Warnings, error) {
	openchamiclusterlog.Info("Validation for OpenCHAMICluster upon update", "name", newObj.GetName())

	// TODO(user): fill in your validation logic upon object update.

	return nil, nil
}

// ValidateDelete implements webhook.CustomValidator so a webhook will be registered for the type OpenCHAMICluster.
func (v *OpenCHAMIClusterCustomValidator) ValidateDelete(_ context.Context, obj *openchamiv1alpha1.OpenCHAMICluster) (admission.Warnings, error) {
	openchamiclusterlog.Info("Validation for OpenCHAMICluster upon deletion", "name", obj.GetName())

	// TODO(user): fill in your validation logic upon object deletion.

	return nil, nil
}
