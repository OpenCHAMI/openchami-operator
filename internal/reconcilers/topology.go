// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/logging"
	"github.com/openchami/openchami-operator/internal/vault"
)

const (
	kindConfigMap = "ConfigMap"

	// topologyConfigMapKey is the data key under which the topology JSON is
	// stored in the ConfigMap.
	topologyConfigMapKey = "topology.json"

	// topologyLabelKey marks a ConfigMap as the operator-published topology
	// for a cluster. Labelled separately from labelManagedBy so consumers can
	// select the topology ConfigMap with a single, stable selector.
	topologyLabelKey   = "openchami.org/topology"
	topologyLabelValue = "true"
	clusterLabelKey    = "openchami.org/cluster"

	// topologyHashPrefix prefixes the SHA-256 hex digest written into
	// status.topologyVersion. Mirrors the docker/oci convention so consumers
	// can reason about the digest format.
	topologyHashPrefix = "sha256:"
)

// TopologyReconciler publishes the cluster topology ConfigMap.
//
// The topology is the operator's contract with the services it deploys: a
// single ConfigMap (data key topology.json) describing every service endpoint,
// platform infrastructure address, and database endpoint that services need to
// discover each other and external infrastructure. The schema is owned by the
// operator (see topology_schema.go) — services consume it.
//
// Disabled services still appear in the topology with their deterministic
// endpoint URL but ready=false. Omitting fields conditionally would make the
// version hash depend on enablement flags and complicate downstream consumers.
type TopologyReconciler struct {
	Client   client.Client
	Recorder record.EventRecorder

	// nowFunc returns the current time. Tests inject a fixed clock so the
	// generatedAt timestamp is deterministic; production leaves it nil and
	// time.Now is used.
	nowFunc func() time.Time
}

// Reconcile builds the topology document, applies it as a ConfigMap, and
// records the content hash in cluster.Status.TopologyVersion.
func (r *TopologyReconciler) Reconcile(ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane) (ctrl.Result, error) {
	log := logging.Enrich(ctx, cp, "topology")

	spec := r.buildTopology(cp)
	hash, err := computeTopologyHash(spec)
	if err != nil {
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionTopologyPublished,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonError,
			Message:            fmt.Sprintf("hashing topology: %v", err),
			ObservedGeneration: cp.Generation,
		})
		RecordConditionEvent(r.Recorder, cp, corev1.EventTypeWarning,
			conditions.ReasonError,
			fmt.Sprintf("could not hash topology: %v", err))
		return ctrl.Result{}, fmt.Errorf("hashing topology: %w", err)
	}
	spec.Version = topologyHashPrefix + hash
	spec.GeneratedAt = r.now().UTC().Format(time.RFC3339)

	payload, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionTopologyPublished,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonError,
			Message:            fmt.Sprintf("marshaling topology: %v", err),
			ObservedGeneration: cp.Generation,
		})
		RecordConditionEvent(r.Recorder, cp, corev1.EventTypeWarning,
			conditions.ReasonError,
			fmt.Sprintf("could not marshal topology: %v", err))
		return ctrl.Result{}, fmt.Errorf("marshaling topology: %w", err)
	}

	cm := r.buildConfigMap(cp, string(payload))
	cmLog := logging.EnrichWithResource(log, kindConfigMap, cm.Name)
	cmLog.Info("applying topology ConfigMap", "version", spec.Version)
	if err := r.Client.Patch(ctx, cm, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
			Type:               conditions.ConditionTopologyPublished,
			Status:             metav1.ConditionFalse,
			Reason:             conditions.ReasonError,
			Message:            fmt.Sprintf("applying topology ConfigMap: %v", err),
			ObservedGeneration: cp.Generation,
		})
		RecordConditionEvent(r.Recorder, cp, corev1.EventTypeWarning,
			conditions.ReasonError,
			fmt.Sprintf("applying topology ConfigMap failed: %v", err))
		return ctrl.Result{}, fmt.Errorf("applying topology ConfigMap: %w", err)
	}

	cp.Status.TopologyVersion = spec.Version
	apimeta.SetStatusCondition(&cp.Status.Conditions, metav1.Condition{
		Type:               conditions.ConditionTopologyPublished,
		Status:             metav1.ConditionTrue,
		Reason:             conditions.ReasonReady,
		Message:            "topology ConfigMap applied",
		ObservedGeneration: cp.Generation,
	})
	return ctrl.Result{}, nil
}

// Describe returns the ConfigMap this reconciler would apply. The version and
// generatedAt fields are populated so the rendered JSON is stable enough for
// `ochami-admin describe` to round-trip through `kubectl apply`.
func (r *TopologyReconciler) Describe(cp *openchamiv1alpha1.OpenCHAMIControlPlane) ([]client.Object, error) {
	spec := r.buildTopology(cp)
	hash, err := computeTopologyHash(spec)
	if err != nil {
		return []client.Object{}, fmt.Errorf("hashing topology: %w", err)
	}
	spec.Version = topologyHashPrefix + hash
	spec.GeneratedAt = r.now().UTC().Format(time.RFC3339)
	payload, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return []client.Object{}, fmt.Errorf("marshaling topology: %w", err)
	}
	return []client.Object{r.buildConfigMap(cp, string(payload))}, nil
}

// now returns the current time, honouring the test-injected nowFunc when set.
func (r *TopologyReconciler) now() time.Time {
	if r.nowFunc != nil {
		return r.nowFunc()
	}
	return time.Now()
}

// buildTopology assembles the TopologySpec from cluster spec and per-service
// status. The version and generatedAt fields are intentionally left blank
// here; the caller fills them in after computing the content hash.
func (r *TopologyReconciler) buildTopology(cp *openchamiv1alpha1.OpenCHAMIControlPlane) TopologySpec {
	ns := ControlPlaneNamespace(cp)
	clusterName := cp.Spec.ClusterName
	paths := vault.Paths(clusterName)

	return TopologySpec{
		ClusterName: clusterName,
		Domain:      cp.Spec.Domain,
		Services: TopologyServices{
			SMD: TopologyServiceEntry{
				Endpoint:     serviceURL(ServiceSMD, ns, smdPort),
				ExternalPath: pathSMDPrefix,
				Ready:        serviceReady(cp, ServiceSMD),
			},
			Tokensmith: TopologyServiceEntry{
				Endpoint:     serviceURL(ServiceTokensmith, ns, tokensmithPort),
				ExternalPath: "/oauth",
				Ready:        serviceReady(cp, ServiceTokensmith),
				JWKSURL:      serviceURL(ServiceTokensmith, ns, tokensmithPort) + pathTokensmithJWKS,
			},
			BootService: TopologyServiceEntry{
				Endpoint:     serviceURL(ServiceBootService, ns, bootServicePort),
				ExternalPath: pathBootPrefix,
				Ready:        serviceReady(cp, ServiceBootService),
				S3Endpoint:   cp.Spec.Platform.ObjectStorage.Endpoint,
				S3Bucket:     BootBucketName(cp),
			},
			MetadataService: TopologyServiceEntry{
				Endpoint:     serviceURL(ServiceMetadataService, ns, metadataServicePort),
				ExternalPath: pathMetadataPrefix,
				Ready:        serviceReady(cp, ServiceMetadataService),
			},
		},
		Platform: TopologyPlatform{
			Vault: TopologyVault{
				Address:    cp.Spec.Platform.Vault.Address,
				KVMount:    paths.KVMount,
				PathPrefix: paths.SecretPrefix,
			},
			ObjectStorage: TopologyObjectStorage{
				Endpoint: cp.Spec.Platform.ObjectStorage.Endpoint,
				Bucket:   BootBucketName(cp),
			},
			Logging: TopologyLogging{
				Endpoint:      cp.Spec.Platform.ObjectStorage.Endpoint,
				Bucket:        LogBucketName(cp),
				ParquetPrefix: "logs/",
			},
		},
		Database: TopologyDatabase{
			ReadWriteEndpoint: postgresEndpoint(clusterName, ns, "rw"),
			ReadOnlyEndpoint:  postgresEndpoint(clusterName, ns, "ro"),
		},
	}
}

// buildConfigMap returns the operator-managed topology ConfigMap.
func (r *TopologyReconciler) buildConfigMap(cp *openchamiv1alpha1.OpenCHAMIControlPlane, payload string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: coreAPIVersion, Kind: kindConfigMap},
		ObjectMeta: metav1.ObjectMeta{
			Name:      topologyConfigMapName(cp),
			Namespace: ControlPlaneNamespace(cp),
			Labels: map[string]string{
				topologyLabelKey: topologyLabelValue,
				clusterLabelKey:  cp.Spec.ClusterName,
				labelManagedBy:   managedByValue,
			},
		},
		Data: map[string]string{
			topologyConfigMapKey: payload,
		},
	}
}

// topologyConfigMapName returns the canonical ConfigMap name for the cluster
// topology document.
func topologyConfigMapName(cp *openchamiv1alpha1.OpenCHAMIControlPlane) string {
	return "openchami-" + cp.Spec.ClusterName + "-topology"
}

// serviceURL returns the in-cluster URL for a service running in the given
// namespace and port.
func serviceURL(service, namespace string, port int32) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", service, namespace, port)
}

// postgresEndpoint returns the host:port string for the CNPG -rw or -ro
// service. Mirrors the naming used by DatabaseReconciler.
func postgresEndpoint(clusterName, namespace, role string) string {
	return fmt.Sprintf("openchami-%s-postgres-%s.%s.svc.cluster.local:%d",
		clusterName, role, namespace, postgresPort)
}

// serviceReady reports whether the named service has reported Ready in the
// cluster status. Absent or not-yet-ready services map to false.
func serviceReady(cp *openchamiv1alpha1.OpenCHAMIControlPlane, name string) bool {
	st, ok := cp.Status.Services[name]
	return ok && st.Ready
}

// computeTopologyHash returns the SHA-256 of the topology document with the
// version and generatedAt fields zeroed. Hashing the canonical JSON of a
// version-stripped copy keeps the hash stable across reconciles where only the
// timestamp changes.
func computeTopologyHash(spec TopologySpec) (string, error) {
	stripped := spec
	stripped.Version = ""
	stripped.GeneratedAt = ""
	body, err := json.Marshal(stripped)
	if err != nil {
		return "", fmt.Errorf("marshaling topology for hash: %w", err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}
