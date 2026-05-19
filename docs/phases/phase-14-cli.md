# Phase 14 — ochami-admin CLI

**PARALLEL PHASE — see AGENTS.md for sub-agent assignments.**
Cobra skeleton must exist before spawning sub-agents.

`ochami-admin` is the **infrastructure operator's tool**.
HPC admins use the OpenCHAMI CLI and service REST APIs, not this tool.

## cobra skeleton (implement before spawning sub-agents)
```go
// cmd/ochami-admin/main.go
root := &cobra.Command{Use: "ochami-admin", Short: "OpenCHAMI operator admin CLI"}
root.AddCommand(admin.InitCmd(), admin.DescribeCmd(), admin.BackupCmd(),
                admin.RestoreCmd(), admin.LogsCmd())
```

## init — `internal/admin/init.go`
Generates a ready-to-apply OpenCHAMIControlPlane YAML from flags.

Key flags:
```
--cluster-name        required
--domain              required
--vault-addr          required
--vault-auth          kubernetes|appRole (default: kubernetes)
--s3-endpoint         required
--provision-subnet    for networkProbe
--provision-validate-host
--provision-validate-port
--bmc-subnet
--bmc-validate-host
--bmc-validate-port
--dhcp-range          "start-end" e.g. "192.168.1.100-192.168.1.200"
--skip-checks         skip connectivity pre-validation (for air-gapped installs)
--output              write to file (default: stdout)
```

Output: valid CR YAML + instructions printed to stderr.
No Kubernetes connection required.

## describe — `internal/admin/describe.go`
Dry-run view. Calls each sub-reconciler's `Describe()` method.
No cluster connection required — reads only from the CR YAML file.

```bash
ochami-admin describe -f cluster.yaml
```

Print each object type with key fields. Format as readable text, not YAML
(admins are checking what WOULD be applied, not copying the output).

## backup — `internal/admin/backup.go`
Infrastructure state snapshot to VersityGW:
1. CNPG base backup via `Backup` CRD
2. tokensmith PVC via VolumeSnapshot (if available) or `kubectl cp`
3. Vault raft snapshot via `vault operator raft snapshot save`
4. CR YAML via `kubectl get openchamicontrolplane {name} -o yaml`

Requires: `--kubeconfig`, `--vault-addr`, `--vault-token`,
          `--s3-endpoint`, `--s3-bucket`, `--output-prefix`

## restore — `internal/admin/restore.go`
Reverse of backup. Document manual steps for cases where full automation
is impractical (Vault raft restore, PVC restore). Pre-flight check that
target namespace does not already exist.

## logs — `internal/admin/logs.go`
Embeds DuckDB via `github.com/marcboeker/go-duckdb` (or shell out to `duckdb`
binary if embedding is complex). Queries Parquet in VersityGW.

```bash
# Raw SQL passthrough
ochami-admin logs query --cluster venado \
  --s3-endpoint http://versitygw:10000 --s3-bucket venado-logs \
  --sql "SELECT ts, service, msg FROM read_parquet('...') LIMIT 10"

# Service + time filter (generates SQL internally)
ochami-admin logs query --cluster venado \
  --s3-endpoint http://versitygw:10000 --s3-bucket venado-logs \
  --service tokensmith --since 2026-05-01T00:00:00Z

# Export
ochami-admin logs export --format csv --output results.csv
```

S3 credentials: flags → OPENCHAMI_S3_ACCESS_KEY/SECRET env vars → AWS chain.

```bash
tools/check-phase.sh 14
```
