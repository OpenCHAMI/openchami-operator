# ochami-admin CLI reference

`ochami-admin` is the operator's companion CLI for site administrators.
It complements `kubectl` for tasks that go beyond plain CRUD on the
`OpenCHAMIControlPlane` resource — generating manifests, dry-running the
reconcile output, snapshotting backups, restoring from a snapshot, and
querying funicular logs.

Source: `cmd/ochami-admin/main.go` → `internal/admin/{init,describe,backup,restore,logs}.go`.

Build:
```sh
make build               # produces ./bin/ochami-admin (alongside ./bin/operator)
```

Top-level commands:

| Command | Purpose | Touches kube? |
|---|---|---|
| `init` | Generate a ready-to-apply `OpenCHAMIControlPlane` manifest from flags. | No |
| `describe` | Dry-run: read a manifest and list every Kubernetes object the operator would apply, grouped by sub-reconciler. | No |
| `backup` | Snapshot cluster infrastructure state. Automates the CNPG base backup + CR YAML dump; prints copy-paste commands for the manual steps. | Yes |
| `restore` | Re-apply a CR from a backup directory; print copy-paste commands for the manual restore steps. | Yes |
| `logs query` / `logs export` | Run SQL against funicular Parquet logs in S3 via DuckDB. | No (but talks to S3 + shells out to `duckdb`) |

---

## `ochami-admin init`

Generates a ready-to-apply `OpenCHAMIControlPlane` YAML from flags. Useful
when you'd rather not start from `production-controlplane.yaml.example`
and edit by hand, or when scripting cluster onboarding.

**Does not contact Kubernetes** — pure flag-to-YAML transform. The output
may be piped directly to `kubectl apply -f -`.

### Required flags

| Flag | Description |
|---|---|
| `--cluster-name` | Lowercase letters/digits/hyphens, must start with a letter, max 32 chars. Becomes `metadata.name` and `spec.clusterName`. |
| `--domain` | External FQDN. Must contain a dot, must not contain a scheme or path. |
| `--vault-addr` | Vault URL. Must be `https://...` unless `--allow-insecure-vault` is also set (the validating webhook only accepts `http://` for loopback). |
| `--s3-endpoint` | VersityGW or S3-compatible URL. |

### Optional flags

| Flag | Default | Description |
|---|---|---|
| `--vault-auth` | `kubernetes` | `kubernetes` or `appRole`. With `appRole`, a `Secret` named `<clusterName>-vault-approle` is referenced from the manifest — you must create it out-of-band. |
| `--allow-insecure-vault` | false | Allow `http://` Vault address. The validating webhook still rejects this in production; flag exists for dev. |
| `--provision-subnet` | unset | CIDR for the PXE/provision network probe. Setting this enables `networkProbe`. |
| `--provision-validate-host` / `--provision-validate-port` | unset | Optional reachability target inside the provision subnet. |
| `--bmc-subnet` | unset | CIDR for the BMC network probe. Setting this enables Magellan. |
| `--bmc-validate-host` / `--bmc-validate-port` | unset | Optional reachability target inside the BMC subnet. |
| `--dhcp-range` | unset | `start-end` IP pair, e.g. `192.168.1.100-192.168.1.200`. Both halves must parse as IPs. |
| `--skip-checks` | false | Advisory: skip the pre-flight connectivity validation (not yet implemented for `init`; the flag is reserved). |
| `--output` / `-o` | `-` (stdout) | Write to a file path. |

### Examples

```sh
# Minimal AppRole-auth cluster, write to a file
ochami-admin init \
  --cluster-name venado \
  --domain venado.hpc.example.com \
  --vault-addr https://vault.hpc.example.com:8200 \
  --vault-auth appRole \
  --s3-endpoint https://versitygw.hpc.example.com:7070 \
  --output venado.yaml

# Same but with network probing + DHCP enabled, applied immediately
ochami-admin init \
  --cluster-name venado \
  --domain venado.hpc.example.com \
  --vault-addr https://vault.hpc.example.com:8200 \
  --s3-endpoint https://versitygw.hpc.example.com:7070 \
  --provision-subnet 10.42.0.0/16 \
  --provision-validate-host 10.42.0.1 --provision-validate-port 22 \
  --bmc-subnet 10.43.0.0/16 \
  --bmc-validate-host 10.43.0.1 --bmc-validate-port 623 \
  --dhcp-range 10.42.0.100-10.42.0.250 | kubectl apply -f -
```

### Exit codes

`0` on success. `1` on flag validation failure (e.g. invalid CIDR, missing
required flag); the message is human-readable.

### Gotchas

- The generated manifest is `apiVersion: openchami.openchami.org/v1alpha1`
  with `metadata.namespace: default`. If your operator install puts CRs
  elsewhere, edit the namespace before applying.
- With `--vault-auth appRole`, the manifest references
  `<clusterName>-vault-approle`. The Secret must exist in the per-cluster
  namespace `openchami-<clusterName>` (not `default`) — see
  [install-production.md §8](install-production.md#8-stage-the-per-cluster-approle-secret).

---

## `ochami-admin describe`

Reads an `OpenCHAMIControlPlane` manifest and prints, grouped by
sub-reconciler in apply order, every Kubernetes object the operator
*would* apply for that CR. **Does not contact Kubernetes.** Useful for
auditing, code review, and CI gates.

### Flags

| Flag | Default | Description |
|---|---|---|
| `--file` / `-f` | required | Path to the manifest, or `-` to read from stdin. |
| `--show-details` | false | Render extra per-object details: env var names, secret references, container ports. |

### Example

```sh
ochami-admin init --cluster-name venado --domain ... | ochami-admin describe -f -
```

Output shape:

```
=== Control Plane: venado ===
Domain:    venado.hpc.example.com
Namespace: openchami-venado

== Namespace ==
Namespace/(cluster-scoped)/openchami-venado

== Vault ==
VaultConnection/openchami-venado/openchami-venado
VaultAuth/openchami-venado/openchami-venado
VaultStaticSecret/openchami-venado/openchami-venado-db-credentials
...

== SMD ==
Deployment/openchami-venado/smd                    (replicas: 3, image: ghcr.io/openchami/smd:latest)
Service/openchami-venado/smd                       (port: 27779)
...

Total: 42 Kubernetes objects.
```

### Gotchas

- `describe` calls each sub-reconciler's `Describe()` method, which is
  contractually side-effect-free (no DNS, no API calls). If a reconciler
  ever calls a network resolver inside `Describe()`, that's a violation
  of the SubReconciler contract — see follow-up F-2 in
  [phase-16-followups.md](phases/phase-16-followups.md).
- The render order mirrors the controller's reconcile order (see
  `describeSubs()` in `internal/admin/describe.go:55`). Reading the output
  top-to-bottom is the same order an admin watches conditions flip.

---

## `ochami-admin backup`

Snapshots cluster infrastructure state for disaster recovery. Steps:

1. **Automated:** create a CNPG `Backup` CR in the cluster namespace; the
   CNPG controller performs the actual S3 upload via the cluster's
   `cluster.spec.backup` configuration.
2. **Automated:** dump the live `OpenCHAMIControlPlane` CR to
   `<outputDir>/cluster.yaml` (with `managedFields`/`resourceVersion`/`uid`
   stripped so it's re-applyable).
3. **Manual (copy-paste commands printed on stderr):** snapshot the
   tokensmith PVC via VolumeSnapshot (or fall back to `tar` out of the
   running pod).
4. **Manual (copy-paste commands printed on stderr):** `vault operator raft
   snapshot save` against the cluster's Vault.

The CLI prints concrete commands for steps 3 and 4 — including the bucket
upload command for the entire output directory — so a fresh operator
doesn't need to remember the sequence.

### Required flags

| Flag | Description |
|---|---|
| `--cluster-name` | `OpenCHAMIControlPlane` name. |
| `--vault-addr` / `--vault-token` | Used only in the printed manual raft-snapshot command. The CLI itself never authenticates to Vault. |
| `--s3-endpoint` / `--s3-bucket` | Used only in the printed manual upload command. The CLI does not perform the upload. |

### Optional flags

| Flag | Default | Description |
|---|---|---|
| `--namespace` | `default` | Namespace where the `OpenCHAMIControlPlane` CR lives (not the per-cluster namespace). |
| `--kubeconfig` | standard discovery | Path to kubeconfig. |
| `--output-prefix` | `backup/<clusterName>/<RFC3339>/` | Local directory for artifacts. |
| `--dry-run` | false | Print planned actions and the copy-paste manual steps without creating any resources or files. |

### Example

```sh
ochami-admin backup \
  --cluster-name venado \
  --vault-addr https://vault.hpc.example.com:8200 \
  --vault-token "$(vault token create -ttl=30m -policy=raft-snapshot -field=token)" \
  --s3-endpoint https://versitygw.hpc.example.com:7070 \
  --s3-bucket cluster-backups
```

The Vault token needs `sys/storage/raft/snapshot` capability. Use a
short-TTL, snapshot-scoped policy rather than the root token.

### Gotchas

- The output directory is **local to the host running the CLI**. The
  printed `aws --endpoint-url ... s3 cp` command uploads it to your
  configured backup bucket; running `backup` without the upload step
  leaves the snapshot on the local disk.
- Steps 3 and 4 are by design not automated — they need credentials or
  cluster-shell access the CLI doesn't carry around. The `--dry-run` flag
  is the safe way to preview the commands.

---

## `ochami-admin restore`

Re-applies an `OpenCHAMIControlPlane` from a backup directory. Pre-flight
checks include namespace-already-exists guard and cluster-name match
against the saved manifest. The manual steps (Vault raft restore, PVC
restore from VolumeSnapshot, CNPG database recovery) are printed as
copy-paste commands.

### Required flags

| Flag | Description |
|---|---|
| `--cluster-name` | The cluster name to restore. Must match the saved `metadata.name` unless `--force`. |
| `--input-prefix` | Local directory holding the backup artifacts (the `--output-prefix` of a prior `backup` run). |

### Optional flags

| Flag | Default | Description |
|---|---|---|
| `--namespace` | `default` | Where to re-apply the CR. |
| `--kubeconfig` | standard | Kubeconfig path. |
| `--vault-addr` / `--vault-token` | unset | Used in the printed manual raft-restore command. |
| `--force` | false | Overwrite an existing namespace and ignore the cluster-name mismatch check. |
| `--dry-run` | false | Print the planned actions without writing. |

### Gotchas

- Restore is **not** automatic for the destructive bits (raft, PVC, CNPG)
  — the CLI prints the commands you need to run, in order, but waits for
  you to run them. This is intentional: an automated raft restore on the
  wrong cluster is unrecoverable.
- The `--namespace` flag controls where the CR is re-applied; the
  per-cluster namespace `openchami-<clusterName>` is unconditional.

---

## `ochami-admin logs`

Query funicular Parquet logs in S3 via DuckDB. Two sub-commands:

- `ochami-admin logs query` — run a SQL query, print results to stdout.
- `ochami-admin logs export` — run a SQL query, write results to a file in
  CSV / JSON / Parquet format.

**Requires the `duckdb` binary on PATH.** The operator does not embed
DuckDB to avoid a CGO dependency. Set `--duckdb-path` if your duckdb is
installed elsewhere.

### Shared flags

| Flag | Description |
|---|---|
| `--cluster` | Cluster name (required; used to derive the default bucket as `<cluster>-logs`). |
| `--s3-endpoint` | S3 endpoint URL, e.g. `http://versitygw:10000` (required). |
| `--s3-bucket` | Override the default bucket name. |
| `--s3-access-key` / `--s3-secret-key` | S3 credentials. Fall back to `$OPENCHAMI_S3_ACCESS_KEY` / `$OPENCHAMI_S3_SECRET_KEY`. |
| `--sql` | Raw SQL passthrough. Mutually exclusive with `--service` / `--since` / `--until` / `--limit`. |
| `--service` | Filter to one service, e.g. `tokensmith`. |
| `--since` / `--until` | RFC3339 timestamps bounding the query. |
| `--limit` | Default `100`. |
| `--duckdb-path` | Default `duckdb`. |

### `export`-specific

| Flag | Description |
|---|---|
| `--format` | `csv` (default), `json`, or `parquet`. |
| `--output` | File path, or `-` for stdout. |

### Examples

```sh
# Most recent 100 tokensmith log rows
ochami-admin logs query \
  --cluster venado \
  --s3-endpoint https://versitygw.hpc.example.com:7070 \
  --service tokensmith

# Custom SQL passthrough
ochami-admin logs query \
  --cluster venado \
  --s3-endpoint https://versitygw.hpc.example.com:7070 \
  --sql "SELECT service, COUNT(*) AS c FROM read_parquet('s3://venado-logs/**/*.parquet') GROUP BY service ORDER BY c DESC LIMIT 20"

# Export a day of boot-service errors to CSV
ochami-admin logs export \
  --cluster venado \
  --s3-endpoint https://versitygw.hpc.example.com:7070 \
  --service boot-service \
  --since 2026-05-21T00:00:00Z --until 2026-05-22T00:00:00Z \
  --format csv --output /tmp/boot-errors.csv
```

### Gotchas

- DuckDB reads Parquet directly from S3; large clusters may have many
  thousands of objects. Use `--since` / `--until` to bound the scan.
- The default bucket is `<cluster>-logs`. If you renamed the bucket via
  `spec.logging.logBucket`, pass `--s3-bucket` explicitly.

---

## Reference

- [`cmd/ochami-admin/main.go`](../cmd/ochami-admin/main.go) — command tree.
- [`internal/admin/`](../internal/admin/) — implementation of each subcommand.
- [install-production.md](install-production.md) — where `init`'s output is meant to be applied.
- [phase-14-cli.md](phases/phase-14-cli.md) — original design document for the CLI.
