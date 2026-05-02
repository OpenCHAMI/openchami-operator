# Phase 1 — CRD Type Definition

**File:** `api/v1alpha1/openchamicluster_types.go`

**Goal:** Complete CRD spec, status, and supporting types with markers.

## API versioning policy (top-of-file block comment)
```
// v1alpha1 — current storage version. No stability guarantees.
//            Review UPGRADE.md on every operator upgrade.
// v1beta1  — promoted when stable 2+ releases + production use.
// v1       — promoted after v1beta1 stable 6+ months.
```

## Printer columns
```go
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Domain",type=string,JSONPath=`.spec.domain`
// +kubebuilder:printcolumn:name="Operator",type=string,JSONPath=`.status.managedByVersion`
// +kubebuilder:printcolumn:name="CertExpiry",type=string,JSONPath=`.status.certExpiryTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=='Ready')].status`
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
```

## Key types to implement

All types are described in the root CLAUDE.md Phase 1 section.
Implement in this order to satisfy Go's forward-declaration rules:

1. `VaultAuthMethod` string type + constants
2. `ImageSpec`
3. `ServiceDefaults` (embedded struct)
4. `DHCPLeaseRange`
5. `NetworkProbeTarget`
6. `NetworkProbeSpec`
7. `VaultSpec`
8. `ObjectStorageSpec`
9. `PlatformSpec`
10. `SMDSpec`, `TokensmithSpec`, `BootServiceSpec`, `MetadataServiceSpec`
11. `CoreDHCPSpec`, `MagellanSpec`
12. `ServicesSpec`
13. `NetworkingSpec`, `TLSSpec`
14. `DatabaseSpec`
15. `LoggingSpec`
16. `ObservabilitySpec`
17. `OpenCHAMIClusterSpec`
18. `ServiceStatus`
19. `NetworkProbeStatus`
20. `ClusterPhase` + constants
21. `OpenCHAMIClusterStatus`
22. `OpenCHAMICluster` + `OpenCHAMIClusterList`

## Conversion webhook hub
In `api/v1alpha1/openchamicluster_conversion.go`:
```go
// Hub marks v1alpha1 as the conversion hub version.
func (r *OpenCHAMICluster) Hub() {}
```

## After implementing
```bash
make generate manifests
kubectl apply -f config/crd/bases/ --dry-run=client
tools/check-phase.sh 1
```
