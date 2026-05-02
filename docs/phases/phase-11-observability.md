# Phase 11 — Status Reporting and Observability

**File:** `internal/status/reporter.go`

## Phase aggregation (call after each reconcileAll)
```
All conditions True + all services ready        → Ready
CertificatesValid=False (any reason)            → Degraded
Any service Ready→NotReady transition           → Degraded + Warning Event
NetworkProbeReady=False, NoEligibleNodes        → Degraded + Warning Event
Any condition False, Reason contains Provisioning → Provisioning
Any condition False, Reason contains Error       → Failed
```

## Reporter methods
```go
type Reporter struct { client client.Client; recorder record.EventRecorder }

func (r *Reporter) SetCondition(ctx, cluster, condType, status, reason, message string)
func (r *Reporter) UpdateServiceStatus(ctx, cluster, svcName string, ready bool, endpoint, message string)
func (r *Reporter) ComputeAndSetPhase(ctx, cluster)    // runs aggregation logic above
func (r *Reporter) UpdateCertExpiry(ctx, cluster, certSecret *corev1.Secret)
```

## Custom Prometheus metrics
Register in `cmd/operator/main.go` via `prometheus.MustRegister`:
```
openchami_cluster_ready{cluster}                          gauge
openchami_cluster_phase{cluster,phase}                    gauge
openchami_cluster_service_ready{cluster,service}          gauge
openchami_cluster_vault_reachable{cluster}                gauge
openchami_cluster_cert_expiry_seconds{cluster}            gauge
openchami_cluster_reconcile_duration_seconds{cluster,reconciler} histogram
openchami_cluster_dhcp_nodes{cluster}                     gauge
openchami_cluster_probe_nodes_provision{cluster}          gauge
openchami_cluster_probe_nodes_bmc{cluster}                gauge
```

Update all gauges in the reporter after each reconcile.

## ServiceMonitor
If `spec.observability.prometheusOperator=true`:
Create `ServiceMonitor` in cluster namespace selecting all pods with label
`app.kubernetes.io/part-of=openchami`.

```bash
tools/check-phase.sh 11
```
