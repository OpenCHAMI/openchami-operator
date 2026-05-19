# OpenCHAMI Operator — One-Pager for HPC Operations Teams

## What this is

OpenCHAMI is a set of microservices that provide the control-plane for an HPC
cluster: state management (SMD), boot configuration, node metadata, power
control, BMC discovery (Magellan), DHCP for the provision network, and
authentication tokens (tokensmith). The **OpenCHAMI Operator** runs all of
these services on Kubernetes for you.

You manage one object — a `OpenCHAMIControlPlane` — and the operator continuously
reconciles underlying Kubernetes resources to match it.

## What an "operator" is, in 60 seconds

An **operator** is a long-running program inside Kubernetes that watches a
custom resource (a `CRD` — Custom Resource Definition) you defined, and makes
the world match what that resource says. Think of it as a robot administrator
that re-applies your intended state every few seconds and writes status back so
you can see what's happening.

The CRD here is `OpenCHAMIControlPlane`. You write one YAML file describing the
cluster you want; the operator does the rest. Need a second cluster? Apply a
second `OpenCHAMIControlPlane` — the operator gives it a fully isolated namespace,
its own database, its own Vault paths, its own DHCP server, etc.

## What the operator manages for you

For each `OpenCHAMIControlPlane` you apply, the operator creates and maintains:

- **Per-cluster namespace** (`openchami-<clusterName>`) with security policies.
- **PostgreSQL database** (via CloudNativePG operator, separate cluster).
- **All eight services**: SMD, tokensmith, boot-service, metadata-service,
  CoreDHCP, Magellan, funicular log collector, network-probe.
- **TLS certificates** (via cert-manager) for the public gateway.
- **Gateway / HTTPRoutes** (via Envoy Gateway) so external clients can reach
  the API.
- **NetworkPolicies** — zero-trust by default; only the right traffic flows
  between services.
- **S3 buckets** for boot images and logs (against your existing VersityGW).
- **Vault paths and policies** (against your existing Vault) for service
  credentials.

Things the operator does NOT deploy: Vault, VersityGW, the Kubernetes cluster
itself, cert-manager, Envoy Gateway, CloudNativePG. Those are external
prerequisites.

## Installing a cluster

Day-1 looks like this:

```yaml
# my-cluster.yaml
apiVersion: openchami.openchami.org/v1alpha1
kind: OpenCHAMIControlPlane
metadata:
  name: venado
spec:
  clusterName: venado
  domain: venado.lanl.gov

  platform:
    vault:
      address: https://vault.lanl.gov:8200
      authMethod: kubernetes
    objectStorage:
      endpoint: https://s3.lanl.gov

  services:
    smd:        { enabled: true, replicas: 3 }
    tokensmith: { enabled: true, oidcProvider: vault }
    bootService:     { enabled: true, replicas: 3 }
    metadataService: { enabled: true, replicas: 3 }
    coreDHCP:
      enabled: true
      nodeSelector:
        openchami.org/venado-provision-network-ready: "true"
      leaseRanges:
        - subnet: 10.42.0.0/16
          start:  10.42.10.1
          end:    10.42.250.254

  networking:
    gatewayClass: envoy
    tls:
      issuer: vault-pki-issuer

  database:
    instances: 3
    storageSize: 200Gi

  logging:
    enabled: false   # set true once your funicular-collector image is ready
```

Apply and watch:

```sh
kubectl apply -f my-cluster.yaml
kubectl get openchamicontrolplane venado -w
```

Within a few minutes the cluster reaches `READY=True`. At that point the API
endpoints (e.g. `https://smd.venado.lanl.gov`) are live and your compute nodes
can DHCP / iPXE / boot against them.

## Upgrading

Two upgrade modes:

### Operator upgrade
Replacing the operator pod is safe and non-disruptive — services continue
running their existing pods while the new operator version takes over
reconciliation. Roll the operator deployment normally:

```sh
kubectl -n openchami-operator-system set image \
  deploy/openchami-operator-controller-manager \
  manager=ghcr.io/openchami/openchami-operator:v1.3.0
```

### Service upgrade
Bump the image tag for one service in your `OpenCHAMIControlPlane`:

```yaml
services:
  smd:
    enabled: true
    replicas: 3
    image:
      repository: ghcr.io/openchami/smd
      tag: v2.18.1     # was v2.17.0
```

`kubectl apply` and the operator rolls just that service. The CRD's
`operatorChannel: stable | pinned` controls how aggressively the operator
adopts new defaults; use `pinned` with an explicit `pinnedVersion` if you need
deterministic version control across ops cycles.

## Troubleshooting

The operator writes its findings into the CR's `.status.conditions`. **Always
start there.**

```sh
kubectl get openchamicontrolplane venado \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
```

You'll see entries like:

```
ReconcileActive=True Ready: reconciliation is active
NamespaceReady=True Ready: Namespace exists with correct labels
VaultConfigured=True Ready: Vault paths, policies, and VSO resources configured
DatabaseReady=False Provisioning: waiting for VSO to materialize db-credentials Secret
DHCPReady=True Ready: coredhcp DaemonSet ready (numberReady=1)
GatewayReady=False AwaitingCertificate: waiting for CertificatesValid before applying gateway
ServicesReady=False Provisioning: waiting on services: [smd boot-service]
```

Each condition has a **Reason** that points at the next thing to look at:

| Reason | What it means | Where to look |
|---|---|---|
| `Ready` | This piece is fine. | — |
| `Provisioning` | In-flight; resources are being created. | Wait, then check pods in `openchami-<cluster>` namespace. |
| `AwaitingCertificate` | cert-manager hasn't issued the TLS cert yet. | `kubectl describe certificate -n openchami-<cluster>` |
| `Unreachable` | An external dep (Vault, VersityGW) didn't respond. | Check the dep itself; the operator will retry. |
| `Error` | Something is wrong; details in the message. | Read the message — it usually names the resource. |
| `WaitingForProbe` | Network-probe DaemonSet hasn't labeled any nodes yet. | `kubectl get nodes --show-labels \| grep openchami` |
| `ImageNotConfigured` | A service was enabled but no image override was provided. | Set the `image:` block in spec. |

Every `Warning` event the operator emits includes a runbook URL of the form
`https://openchami.org/docs/ops/<reason-in-kebab-case>`. Open it for a more
detailed playbook than this page can fit.

### Three quick recipes

**The CR shows `Ready=False` and I don't know why.**
```sh
kubectl describe openchamicontrolplane venado
```
Reads conditions + events in one place.

**A service won't start.**
```sh
kubectl get pods -n openchami-venado
kubectl logs -n openchami-venado deploy/<service-name>
```
The operator never modifies pod logs; what's there is the service's own output.

**I want to force a re-reconcile (no-op patch).**
```sh
kubectl patch openchamicontrolplane venado --type=merge -p '{}'
```
The operator picks up the change immediately and runs the full pipeline.

### When in doubt

Three things to capture before opening a support ticket:

1. The CR YAML and its `.status`:
   `kubectl get openchamicontrolplane venado -o yaml > /tmp/cr.yaml`
2. Recent operator logs:
   `kubectl logs -n openchami-operator-system deploy/openchami-operator-controller-manager --tail=500`
3. Cluster-namespace dump:
   `kubectl get all,networkpolicy,httproute,certificate,vaultstaticsecret -n openchami-venado -o yaml > /tmp/dump.yaml`

That bundle is enough for the operator team to diagnose 90% of issues without
needing a live session.

## Mental model

| HPC concept | Maps to in OpenCHAMI |
|---|---|
| Site / system | One `OpenCHAMIControlPlane` resource |
| Provision network | `spec.services.coreDHCP.leaseRanges` + `provisionNetwork` |
| BMC network | `spec.services.magellan.bmcSubnet` |
| Service catalog | `spec.services.*` (each microservice toggleable) |
| Quadlet/systemd unit per service | The Kubernetes Deployment/DaemonSet behind the service |
| Per-site secrets | Per-cluster Vault paths under `openchami/<clusterName>/` |
| Site changes / config drift | `kubectl apply` an updated CR; reconcile is automatic |
| Reading current state | `kubectl get openchamicontrolplane <name> -o yaml` |

## Further reading

- `docs/quickstart.md` — full local-dev walkthrough.
- `docs/troubleshooting.md` — the long-form runbook.
- `docs/crd-reference.md` — every field in `OpenCHAMIControlPlane`, with defaults.
- `docs/architecture.md` — how the operator's reconcilers compose.
