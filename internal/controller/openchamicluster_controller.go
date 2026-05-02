/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	openahamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/logging"
	"github.com/openchami/openchami-operator/internal/reconcilers"
	"github.com/openchami/openchami-operator/internal/s3"
	"github.com/openchami/openchami-operator/internal/vault"
	"github.com/openchami/openchami-operator/internal/version"
)

const clusterFinalizer = "openchami.org/cluster-protection"

// +kubebuilder:rbac:groups=openchami.openchami.org,resources=openchamiclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=openchami.openchami.org,resources=openchamiclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=openchami.openchami.org,resources=openchamiclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings;clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments;daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps;secrets;services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways;httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.envoyproxy.io,resources=securitypolicies;backendtrafficpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete

// OpenCHAMIClusterReconciler reconciles an OpenCHAMICluster object.
type OpenCHAMIClusterReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Recorder      record.EventRecorder
	VaultClient   vault.Client
	S3Client      s3.Client
	DefaultImages version.ImageConfig
	DryRun        bool
}

func (r *OpenCHAMIClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cluster := &openahamiv1alpha1.OpenCHAMICluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !cluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, cluster)
	}

	if !controllerutil.ContainsFinalizer(cluster, clusterFinalizer) {
		patch := client.MergeFrom(cluster.DeepCopy())
		controllerutil.AddFinalizer(cluster, clusterFinalizer)
		if err := r.Patch(ctx, cluster, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	orig := cluster.DeepCopy()
	cluster.Status.ObservedGeneration = cluster.Generation

	result, reconcileErr := r.reconcileAll(ctx, cluster)

	if patchErr := r.Status().Patch(ctx, cluster, client.MergeFrom(orig)); patchErr != nil {
		if reconcileErr != nil {
			return ctrl.Result{}, fmt.Errorf("reconcile: %w; status patch: %w", reconcileErr, patchErr)
		}
		return ctrl.Result{}, fmt.Errorf("patching status: %w", patchErr)
	}

	return result, reconcileErr
}

func (r *OpenCHAMIClusterReconciler) reconcileAll(ctx context.Context, cluster *openahamiv1alpha1.OpenCHAMICluster) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cluster, "controller")

	if cluster.Spec.OperatorChannel == "pinned" && cluster.Spec.PinnedVersion != version.Version {
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionReconcileActive,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonVersionPinned,
			Message:            fmt.Sprintf("operator version %s does not match pinned version %s", version.Version, cluster.Spec.PinnedVersion),
			ObservedGeneration: cluster.Generation,
		})
		reconcilers.RecordConditionEvent(r.Recorder, cluster, corev1.EventTypeNormal,
			conditions.ReasonVersionPinned,
			fmt.Sprintf("reconciliation suspended: pinned to version %s", cluster.Spec.PinnedVersion))
		return ctrl.Result{}, nil
	}

	apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionReconcileActive,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            "reconciliation is active",
		ObservedGeneration: cluster.Generation,
	})

	subs := []reconcilers.SubReconciler{
		&reconcilers.NamespaceReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.RBACReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.VaultReconciler{Client: r.Client, Recorder: r.Recorder, VaultClient: r.VaultClient},
		&reconcilers.BucketReconciler{Client: r.Client, Recorder: r.Recorder, S3Client: r.S3Client},
		&reconcilers.DatabaseReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.SMDReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.TokensmithReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.BootServiceReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.MetadataServiceReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.NetworkProbeReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.CoreDHCPReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.MagellanReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.CertificatesReconciler{Client: r.Client, Recorder: r.Recorder},
		&reconcilers.GatewayReconciler{Client: r.Client, Recorder: r.Recorder},
		// Phase 3:  logbucket (deferred to phase 12 with funicular)
		// Phase 8:  networkpolicies
		// Phase 9:  topology
		// Phase 12: funicular
	}

	var longestRequeue time.Duration
	for _, sub := range subs {
		if r.DryRun {
			objs, err := sub.Describe(cluster)
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
		result, err := sub.Reconcile(ctx, cluster)
		if err != nil {
			return ctrl.Result{}, err
		}
		if result.RequeueAfter > longestRequeue {
			longestRequeue = result.RequeueAfter
		}
	}

	r.aggregateServicesReady(cluster)
	cluster.Status.ManagedByVersion = version.Version
	return ctrl.Result{RequeueAfter: longestRequeue}, nil
}

// aggregateServicesReady computes ConditionServicesReady from per-service
// readiness reported by each Phase 5 sub-reconciler in cluster.Status.Services.
// True when every enabled service reports Ready; otherwise False with a list
// of pending services in the message.
func (r *OpenCHAMIClusterReconciler) aggregateServicesReady(cluster *openahamiv1alpha1.OpenCHAMICluster) {
	enabled := map[string]bool{
		reconcilers.ServiceSMD:             cluster.Spec.Services.SMD.Enabled,
		reconcilers.ServiceTokensmith:      cluster.Spec.Services.Tokensmith.Enabled,
		reconcilers.ServiceBootService:     cluster.Spec.Services.BootService.Enabled,
		reconcilers.ServiceMetadataService: cluster.Spec.Services.MetadataService.Enabled,
	}

	var pending []string
	for name, on := range enabled {
		if !on {
			continue
		}
		st, ok := cluster.Status.Services[name]
		if !ok || !st.Ready {
			pending = append(pending, name)
		}
	}

	cond := metav1.Condition{
		Type:               conditions.ConditionServicesReady,
		ObservedGeneration: cluster.Generation,
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
	apimeta.SetStatusCondition(&cluster.Status.Conditions, cond)
}

func (r *OpenCHAMIClusterReconciler) reconcileDelete(ctx context.Context, cluster *openahamiv1alpha1.OpenCHAMICluster) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cluster, "controller")
	log.Info("reconciling deletion")

	if !controllerutil.ContainsFinalizer(cluster, clusterFinalizer) {
		return ctrl.Result{}, nil
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: reconcilers.ClusterNamespace(cluster)}}
	if err := r.Delete(ctx, ns); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, fmt.Errorf("deleting namespace: %w", err)
	}

	if cluster.Annotations["openchami.org/cleanup-vault"] == "true" && r.VaultClient != nil {
		prefix := "openchami/" + cluster.Spec.ClusterName + "/"
		if err := r.VaultClient.DeleteClusterPaths(ctx, prefix); err != nil {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, fmt.Errorf("deleting vault paths: %w", err)
		}
	}

	if cluster.Annotations["openchami.org/cleanup-s3"] == "true" && r.S3Client != nil {
		bucket := reconcilers.BootBucketName(cluster)
		if err := r.S3Client.DeleteBucket(ctx, bucket); err != nil {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, fmt.Errorf("deleting s3 bucket: %w", err)
		}
	}

	cr := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{
		Name: "openchami-" + cluster.Spec.ClusterName + "-network-probe",
	}}
	if err := r.Delete(ctx, cr); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, fmt.Errorf("deleting clusterrole: %w", err)
	}

	patch := client.MergeFrom(cluster.DeepCopy())
	controllerutil.RemoveFinalizer(cluster, clusterFinalizer)
	if err := r.Patch(ctx, cluster, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	return ctrl.Result{}, nil
}

// secretToCluster maps changes to a TLS Secret back to the cluster(s) that own
// it, so that certificate renewal events trigger a fresh reconcile and the
// CertExpiryTime / CertificatesValid condition stay current.
func (r *OpenCHAMIClusterReconciler) secretToCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}

	clusterList := &openahamiv1alpha1.OpenCHAMIClusterList{}
	if err := r.List(ctx, clusterList); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for i := range clusterList.Items {
		c := &clusterList.Items[i]
		if reconcilers.ClusterNamespace(c) != secret.Namespace {
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

func (r *OpenCHAMIClusterReconciler) nodeToCluster(ctx context.Context, obj client.Object) []reconcile.Request {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return nil
	}

	clusterList := &openahamiv1alpha1.OpenCHAMIClusterList{}
	if err := r.List(ctx, clusterList); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, c := range clusterList.Items {
		if !c.Spec.NetworkProbe.Enabled {
			continue
		}
		key := fmt.Sprintf("openchami.org/%s/provision-network-ready", c.Spec.ClusterName)
		if _, ok := node.Labels[key]; ok {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      c.Name,
					Namespace: c.Namespace,
				},
			})
		}
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *OpenCHAMIClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&openahamiv1alpha1.OpenCHAMICluster{}).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.DaemonSet{}).
		Owns(&batchv1.CronJob{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.nodeToCluster)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.secretToCluster)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 5}).
		Named("openchamicluster").
		Complete(r)
}
