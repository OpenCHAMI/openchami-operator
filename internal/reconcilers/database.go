/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package reconcilers

import (
	"context"
	"fmt"
	"time"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openahamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/logging"
)

const (
	databaseRequeueAfter = 30 * time.Second

	cnpgImage      = "ghcr.io/cloudnative-pg/postgresql:16.3"
	dbInitImage    = "postgres:16-alpine"
	defaultStorage = "20Gi"

	cnpgAPIVersion  = "postgresql.cnpg.io/v1"
	cnpgKindCluster = "Cluster"
)

// DatabaseReconciler ensures a CloudNativePG Cluster exists and runs the
// post-init Job to create the boot_service database.
type DatabaseReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder
}

func (r *DatabaseReconciler) Reconcile(ctx context.Context, cluster *openahamiv1alpha1.OpenCHAMICluster) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cluster, "database")
	ns := ClusterNamespace(cluster)

	credsName := "openchami-" + cluster.Spec.ClusterName + "-db-credentials"
	creds := &corev1.Secret{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: credsName}, creds)
	if apierrors.IsNotFound(err) {
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionDatabaseReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonProvisioning,
			Message:            "waiting for VSO to materialize db-credentials Secret",
			ObservedGeneration: cluster.Generation,
		})
		return ctrl.Result{RequeueAfter: databaseRequeueAfter}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting db credentials secret: %w", err)
	}

	cnpg := r.buildCNPGCluster(cluster, credsName)
	log.Info("applying CNPG cluster", "name", cnpg.Name)
	if err := r.Client.Patch(ctx, cnpg, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return ctrl.Result{}, fmt.Errorf("applying cnpg cluster: %w", err)
	}

	current := &cnpgv1.Cluster{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: cnpg.Name}, current); err != nil {
		return ctrl.Result{}, fmt.Errorf("reading cnpg cluster status: %w", err)
	}
	if current.Status.Phase != cnpgv1.PhaseHealthy {
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionDatabaseReady,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonProvisioning,
			Message:            fmt.Sprintf("cnpg phase: %q", current.Status.Phase),
			ObservedGeneration: cluster.Generation,
		})
		return ctrl.Result{RequeueAfter: databaseRequeueAfter}, nil
	}

	job := r.buildPostInitJob(cluster)
	existing := &batchv1.Job{}
	err = r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: job.Name}, existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Client.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("creating post-init job: %w", err)
		}
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("checking post-init job: %w", err)
	}

	apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionDatabaseReady,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            "CNPG cluster healthy; both databases provisioned",
		ObservedGeneration: cluster.Generation,
	})
	return ctrl.Result{}, nil
}

func (r *DatabaseReconciler) Describe(cluster *openahamiv1alpha1.OpenCHAMICluster) ([]client.Object, error) {
	credsName := "openchami-" + cluster.Spec.ClusterName + "-db-credentials"
	return []client.Object{
		r.buildCNPGCluster(cluster, credsName),
		r.buildPostInitJob(cluster),
	}, nil
}

func (r *DatabaseReconciler) buildCNPGCluster(cluster *openahamiv1alpha1.OpenCHAMICluster, credsName string) *cnpgv1.Cluster {
	instances := int(cluster.Spec.Database.Instances)
	if instances == 0 {
		instances = 3
	}
	storage := cluster.Spec.Database.StorageSize
	if storage.IsZero() {
		storage = resource.MustParse(defaultStorage)
	}
	storageStr := storage.String()

	return &cnpgv1.Cluster{
		TypeMeta: metav1.TypeMeta{APIVersion: cnpgAPIVersion, Kind: cnpgKindCluster},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openchami-" + cluster.Spec.ClusterName + "-postgres",
			Namespace: ClusterNamespace(cluster),
		},
		Spec: cnpgv1.ClusterSpec{
			ImageName: cnpgImage,
			Instances: instances,
			Bootstrap: &cnpgv1.BootstrapConfiguration{
				InitDB: &cnpgv1.BootstrapInitDB{
					Database: ServiceSMD,
					Owner:    ServiceSMD,
					Secret:   &cnpgv1.LocalObjectReference{Name: credsName},
				},
			},
			StorageConfiguration: cnpgv1.StorageConfiguration{
				Size:         storageStr,
				StorageClass: storageClassPtr(cluster.Spec.Database.StorageClass),
			},
		},
	}
}

func (r *DatabaseReconciler) buildPostInitJob(cluster *openahamiv1alpha1.OpenCHAMICluster) *batchv1.Job {
	rwService := "openchami-" + cluster.Spec.ClusterName + "-postgres-rw"
	one := int32(1)
	script := `set -euo pipefail
psql "$PG_URI" -tc "SELECT 1 FROM pg_database WHERE datname='boot_service'" | grep -q 1 || \
  psql "$PG_URI" -c "CREATE DATABASE boot_service OWNER boot_service"
`
	return &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openchami-" + cluster.Spec.ClusterName + "-db-init",
			Namespace: ClusterNamespace(cluster),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &one,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"openchami.org/role": "db-init"},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyOnFailure,
					ServiceAccountName: configReaderName,
					SecurityContext:    CommonPodSecurityContext(),
					Containers: []corev1.Container{{
						Name:            "init",
						Image:           dbInitImage,
						SecurityContext: CommonSecurityContext(),
						Command:         []string{"/bin/sh", "-c", script},
						Env: []corev1.EnvVar{{
							Name: "PG_URI",
							ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: rwService,
									},
									Key: "uri",
								},
							},
						}},
					}},
				},
			},
		},
	}
}

func storageClassPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
