// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/logging"
	"github.com/openchami/openchami-operator/internal/reconcilers"
	"github.com/openchami/openchami-operator/internal/s3"
	"github.com/openchami/openchami-operator/internal/status"
	"github.com/openchami/openchami-operator/internal/vault"
	"github.com/openchami/openchami-operator/internal/version"
)

const (
	clusterFinalizer = "openchami.org/cluster-protection"

	// annotationCleanupVault and annotationCleanupS3 opt the control plane
	// into deleting external Vault paths / S3 buckets on CR deletion. The
	// opt-in value (truthy) is "true".
	annotationCleanupVault = "openchami.org/cleanup-vault"
	annotationCleanupS3    = "openchami.org/cleanup-s3"
	annotationOptInTrue    = "true"
)

// +kubebuilder:rbac:groups=openchami.openchami.org,resources=openchamicontrolplanes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=openchami.openchami.org,resources=openchamicontrolplanes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=openchami.openchami.org,resources=openchamicontrolplanes/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings;clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments;daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=cronjobs;jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps;secrets;services;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=secrets.hashicorp.com,resources=vaultconnections;vaultauths;vaultstaticsecrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// pods/exec is required so the operator can run `tokensmith bootstrap-token
// create` inside the tokensmith pod when provisioning the boot-service
// bootstrap token (see internal/reconcilers/tokensmith.go).
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways;httproutes;backendtlspolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.envoyproxy.io,resources=securitypolicies;backendtrafficpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates;issuers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete

// OpenCHAMIControlPlaneReconciler reconciles an OpenCHAMIControlPlane object.
type OpenCHAMIControlPlaneReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Recorder    record.EventRecorder
	VaultClient vault.Client
	S3Client    s3.Client
	DryRun      bool

	// RESTConfig is the raw *rest.Config the manager was built with.
	// Used by sub-reconcilers that need a remotecommand executor (e.g.
	// tokensmith bootstrap-token minting via pods/exec). Optional in
	// tests; reconcilers that need it must guard for nil and degrade
	// gracefully (skip the step, requeue) rather than crash.
	RESTConfig *rest.Config

	// Reporter centralises post-reconcile phase computation and Prometheus
	// metric updates. Optional in tests; the controller falls back to its
	// existing aggregator when nil so unit tests need not wire it.
	Reporter *status.Reporter
}

func (r *OpenCHAMIControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cp := &openchamiv1alpha1.OpenCHAMIControlPlane{}
	if err := r.Get(ctx, req.NamespacedName, cp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !cp.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, cp)
	}

	if !controllerutil.ContainsFinalizer(cp, clusterFinalizer) {
		patch := client.MergeFrom(cp.DeepCopy())
		controllerutil.AddFinalizer(cp, clusterFinalizer)
		if err := r.Patch(ctx, cp, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	orig := cp.DeepCopy()
	cp.Status.ObservedGeneration = cp.Generation

	result, reconcileErr := r.reconcileAll(ctx, cp)

	if patchErr := r.Status().Patch(ctx, cp, client.MergeFrom(orig)); patchErr != nil {
		if reconcileErr != nil {
			return ctrl.Result{}, fmt.Errorf("reconcile: %w; status patch: %w", reconcileErr, patchErr)
		}
		return ctrl.Result{}, fmt.Errorf("patching status: %w", patchErr)
	}

	return result, reconcileErr
}

func (r *OpenCHAMIControlPlaneReconciler) reconcileAll(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cp, "controller")

	if cp.Spec.OperatorChannel == "pinned" && cp.Spec.PinnedVersion != version.Version {
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionReconcileActive,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonVersionPinned,
			Message:            fmt.Sprintf("operator version %s does not match pinned version %s", version.Version, cp.Spec.PinnedVersion),
			ObservedGeneration: cp.Generation,
		})
		reconcilers.RecordConditionEvent(r.Recorder, cp, corev1.EventTypeNormal,
			conditions.ReasonVersionPinned,
			fmt.Sprintf("reconciliation suspended: pinned to version %s", cp.Spec.PinnedVersion))
		return ctrl.Result{}, nil
	}

	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionReconcileActive,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            "reconciliation is active",
		ObservedGeneration: cp.Generation,
	})

	subs := []reconcilers.SubReconciler{
		&reconcilers.NamespaceReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.RBACReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.VaultReconciler{Client: r.Client, Recorder: r.Recorder, VaultClient: r.VaultClient},
		&reconcilers.BucketReconciler{Client: r.Client, Recorder: r.Recorder, S3Client: r.S3Client},
		&reconcilers.DatabaseReconciler{Client: r.Client, Recorder: r.Recorder},
		// ServiceIdentity provisions the per-cluster mTLS CA and the
		// downstream server / client certs *before* tokensmith starts,
		// so tokensmith's first rollout already has its server cert in
		// place and consumer pods on the next reconcile can switch
		// from single-use bootstrap-token auth to mTLS service-identity.
		// See internal/reconcilers/service_identity.go for the resource
		// layout and ConditionServiceIdentityReady semantics.
		&reconcilers.ServiceIdentityReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.SMDReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.TokensmithReconciler{Client: r.Client, Recorder: r.Recorder, RESTConfig: r.RESTConfig},
		&reconcilers.BootServiceReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.MetadataServiceReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.NetworkProbeReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.CoreDHCPReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.MagellanReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.CertificatesReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.GatewayReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.NetworkPoliciesReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.TopologyReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.ServiceMonitorReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.LogBucketReconciler{Client: r.Client, Recorder: r.Recorder, S3Client: r.S3Client},
		&reconcilers.FunicularReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.LogqCompactorReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.LogqQueryReconciler{Client: r.Client, Recorder: r.Recorder},
	}

	var longestRequeue time.Duration
	for _, sub := range subs {
		if r.DryRun {
			objs, err := sub.Describe(cp)
			if err != nil {
				log.Error(err, "dry-run describe failed")
				continue
			}
			for _, obj := range objs {
				log.Info("dry-run: would apply",
					"kind", obj.GetObjectKind().GroupVersionKind().Kind,
					"name", obj.GetName())
			}
			continue
		}
		start := time.Now()
		result, err := sub.Reconcile(ctx, cp)
		if r.Reporter != nil {
			status.ObserveReconcile(cp.Spec.ClusterName, subName(sub), time.Since(start))
		}
		if err != nil {
			return ctrl.Result{}, err
		}
		if result.RequeueAfter > longestRequeue {
			longestRequeue = result.RequeueAfter
		}
	}

	r.aggregateServicesReady(cp)
	cp.Status.ManagedByVersion = version.Version

	if r.Reporter != nil {
		r.Reporter.ComputeAndSetPhase(cp)
		r.Reporter.UpdateMetrics(cp)
	}

	return ctrl.Result{RequeueAfter: longestRequeue}, nil
}

// subName returns the leaf type name of a sub-reconciler (e.g. "VaultReconciler"
// from *reconcilers.VaultReconciler) for use as a metric label.
func subName(sub reconcilers.SubReconciler) string {
	return strings.TrimPrefix(fmt.Sprintf("%T", sub), "*reconcilers.")
}

// aggregateServicesReady computes ConditionServicesReady from per-service
// readiness reported by each Phase 5 sub-reconciler in cluster.Status.Services.
// True when every enabled service reports Ready; otherwise False with a list
// of pending services in the message.
func (r *OpenCHAMIControlPlaneReconciler) aggregateServicesReady(cp *openchamiv1alpha1.OpenCHAMIControlPlane) {
	enabled := map[string]bool{
		reconcilers.ServiceSMD:             cp.Spec.Services.SMD.Enabled,
		reconcilers.ServiceTokensmith:      cp.Spec.Services.Tokensmith.Enabled,
		reconcilers.ServiceBootService:     cp.Spec.Services.BootService.Enabled,
		reconcilers.ServiceMetadataService: cp.Spec.Services.MetadataService.Enabled,
	}

	var pending []string
	for name, on := range enabled {
		if !on {
			continue
		}
		st, ok := cp.Status.Services[name]
		if !ok || !st.Ready {
			pending = append(pending, name)
		}
	}

	cond := metav1.Condition{
		Type:               conditions.ConditionServicesReady,
		ObservedGeneration: cp.Generation,
	}
	if len(pending) == 0 {
		cond.Status = metav1.ConditionTrue
		cond.Reason = conditions.ReasonReady
		cond.Message = "all enabled services report Ready"
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = conditions.ReasonProvisioning
		cond.Message = fmt.Sprintf("waiting on services: %v", pending)
	}
	apimeta.SetStatusCondition(&cp.Status.Conditions, cond)
}

func (r *OpenCHAMIControlPlaneReconciler) reconcileDelete(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cp, "controller")
	log.Info("reconciling deletion")

	if !controllerutil.ContainsFinalizer(cp, clusterFinalizer) {
		return ctrl.Result{}, nil
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: reconcilers.ControlPlaneNamespace(cp)}}
	if err := r.Delete(ctx, ns); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, fmt.Errorf("deleting namespace: %w", err)
	}

	if cp.Annotations[annotationCleanupVault] == annotationOptInTrue && r.VaultClient != nil {
		prefix := "openchami/" + cp.Spec.ClusterName + "/"
		if err := r.VaultClient.DeleteClusterPaths(ctx, prefix); err != nil {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, fmt.Errorf("deleting vault paths: %w", err)
		}
	}

	if cp.Annotations[annotationCleanupS3] == annotationOptInTrue && r.S3Client != nil {
		bucket := reconcilers.BootBucketName(cp)
		if err := r.S3Client.DeleteBucket(ctx, bucket); err != nil {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, fmt.Errorf("deleting s3 bucket: %w", err)
		}
		logBucket := reconcilers.LogBucketName(cp)
		if err := r.S3Client.DeleteBucket(ctx, logBucket); err != nil {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, fmt.Errorf("deleting log s3 bucket: %w", err)
		}
	}

	cr := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{
		Name: "openchami-" + cp.Spec.ClusterName + "-network-probe",
	}}
	if err := r.Delete(ctx, cr); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, fmt.Errorf("deleting clusterrole: %w", err)
	}

	patch := client.MergeFrom(cp.DeepCopy())
	controllerutil.RemoveFinalizer(cp, clusterFinalizer)
	if err := r.Patch(ctx, cp, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	return ctrl.Result{}, nil
}

// secretToCluster maps changes to a TLS Secret back to the cluster(s) that own
// it, so that certificate renewal events trigger a fresh reconcile and the
// CertExpiryTime / CertificatesValid condition stay current.
func (r *OpenCHAMIControlPlaneReconciler) secretToCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}

	clusterList := &openchamiv1alpha1.OpenCHAMIControlPlaneList{}
	if err := r.List(ctx, clusterList); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for i := range clusterList.Items {
		c := &clusterList.Items[i]
		if reconcilers.ControlPlaneNamespace(c) != secret.Namespace {
			continue
		}
		if secret.Name != reconcilers.GatewayTLSSecretName(c) {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      c.Name,
				Namespace: c.Namespace,
			},
		})
	}
	return requests
}

func (r *OpenCHAMIControlPlaneReconciler) nodeToCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return nil
	}

	clusterList := &openchamiv1alpha1.OpenCHAMIControlPlaneList{}
	if err := r.List(ctx, clusterList); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, c := range clusterList.Items {
		if !c.Spec.NetworkProbe.Enabled {
			continue
		}
		// Either probe type can change independently — a node may pass the
		// BMC subnet probe but not the provision probe, or vice versa — so
		// we trigger a reconcile when either label is present on the node.
		provisionKey := fmt.Sprintf("openchami.org/%s-provision-network-ready", c.Spec.ClusterName)
		bmcKey := fmt.Sprintf("openchami.org/%s-bmc-network-ready", c.Spec.ClusterName)
		if _, ok := node.Labels[provisionKey]; ok {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: c.Name, Namespace: c.Namespace},
			})
			continue
		}
		if _, ok := node.Labels[bmcKey]; ok {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: c.Name, Namespace: c.Namespace},
			})
		}
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *OpenCHAMIControlPlaneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&openchamiv1alpha1.OpenCHAMIControlPlane{}).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.DaemonSet{}).
		Owns(&batchv1.CronJob{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.nodeToCluster)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.secretToCluster)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 5}).
		Named("openchamicontrolplane").
		Complete(r)
}
