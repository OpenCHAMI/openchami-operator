#!/usr/bin/env bash
set -euo pipefail

# Disposable smoke-test bootstrap for a three-node Linux bare-metal cluster.
# This installs dev-mode Vault and LocalStack; do not use it for production.

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CLUSTER_NAME=${CLUSTER_NAME:-smoke}
TOOLS_NAMESPACE=${TOOLS_NAMESPACE:-openchami-smoke-system}
OPERATOR_IMAGE=${OPERATOR_IMAGE:-ghcr.io/openchami/openchami-operator:v0.0.8}
K3S_CHANNEL=${K3S_CHANNEL:-stable}
KUBECONFIG=${KUBECONFIG:-}
WORKER_NODES=${WORKER_NODES:-}

VAULT_TOKEN=smoke-dev-root-token
S3_ACCESS_KEY=smoke-access-key
S3_SECRET_KEY=smoke-secret-key
CP_NAMESPACE="openchami-${CLUSTER_NAME}"
VAULT_ADDRESS="http://openchami-smoke-vault.${TOOLS_NAMESPACE}.svc.cluster.local:8200"
S3_ENDPOINT="http://openchami-smoke-s3.${TOOLS_NAMESPACE}.svc.cluster.local:10000"

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

say() {
  printf '\n==> %s\n' "$*"
}

as_root() {
  if [[ $(id -u) -eq 0 ]]; then
    "$@"
  else
    sudo "$@"
  fi
}

[[ $(uname -s) == Linux ]] || die "Run this script on a Linux Kubernetes node, not on macOS."
[[ -f "${ROOT_DIR}/config/default/kustomization.yaml" ]] || die "Run from a checkout of openchami-operator."
command -v curl >/dev/null || die "curl is required on this node."
command -v sudo >/dev/null || [[ $(id -u) -eq 0 ]] || die "sudo or a root shell is required."

say "Using operator image: ${OPERATOR_IMAGE}"

if ! command -v kubectl >/dev/null 2>&1; then
  if command -v k3s >/dev/null 2>&1; then
    kubectl() { as_root k3s kubectl "$@"; }
  fi
fi

if [[ -n "$KUBECONFIG" ]]; then
  export KUBECONFIG
fi

use_existing=false
local_k3s=false
if [[ -z "$KUBECONFIG" && -f /etc/rancher/k3s/k3s.yaml ]] && as_root systemctl is-active --quiet k3s; then
  export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
  local_k3s=true
fi
if kubectl get nodes >/dev/null 2>&1; then
  context=$(kubectl config current-context 2>/dev/null || printf unknown)
  [[ "$context" != kind-* ]] || die "Context ${context} is a local kind cluster, not the bare-metal target. Use hack/local-mac-smoke-test.sh on your Mac, or run this script on the first Linux bare-metal node with its kubeconfig."
  use_existing=true
  say "Found reachable Kubernetes context: ${context}"
else
  if [[ -f /etc/rancher/k3s/k3s.yaml ]] && as_root systemctl is-active --quiet k3s; then
    export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
    use_existing=true
    local_k3s=true
    say "Using the local K3s cluster."
  fi
fi

if [[ "$use_existing" != true ]]; then
  if ! command -v k3s >/dev/null 2>&1; then
    command -v ssh >/dev/null || die "ssh is required to join the two worker nodes."
    if [[ -z "$WORKER_NODES" ]]; then
      read -r -p "Two SSH targets for the worker nodes (user@host user@host): " WORKER_NODES
    fi
    read -r -a workers <<< "$WORKER_NODES"
    [[ ${#workers[@]} -eq 2 ]] || die "Enter exactly two SSH targets, for example root@192.0.2.12 root@192.0.2.13."

    say "Installing K3s server on this node"
    curl -sfL https://get.k3s.io | as_root env INSTALL_K3S_CHANNEL="$K3S_CHANNEL" \
      sh -s - server --write-kubeconfig-mode 644
    export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
    as_root chmod 644 "$KUBECONFIG"
    if ! command -v kubectl >/dev/null 2>&1; then
      kubectl() { as_root k3s kubectl "$@"; }
    fi

  else
    export KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}
    as_root systemctl is-active --quiet k3s || die "K3s is installed but its server is not running. Check systemctl status k3s; no other cluster will be installed over it."
  fi
  local_k3s=true
fi

if [[ "$local_k3s" == true ]]; then
  ready_nodes=$(kubectl get nodes --no-headers 2>/dev/null | awk '$2 == "Ready" {count++} END {print count+0}')
  if [[ "$ready_nodes" -lt 3 ]]; then
    command -v ssh >/dev/null || die "ssh is required to join worker nodes."
    if [[ -z "$WORKER_NODES" ]]; then
      read -r -p "Two SSH targets for the worker nodes (user@host user@host): " WORKER_NODES
    fi
    read -r -a workers <<< "$WORKER_NODES"
    [[ ${#workers[@]} -eq 2 ]] || die "Enter exactly two SSH targets for the workers."
    server_ip=${K3S_SERVER_IP:-}
    if [[ -z "$server_ip" ]]; then
      read -r -p "IP address the worker nodes use to reach this server: " server_ip
    fi
    [[ -n "$server_ip" ]] || die "K3S_SERVER_IP is required."
    token=$(as_root cat /var/lib/rancher/k3s/server/node-token)

    for worker in "${workers[@]}"; do
      say "Joining worker ${worker}"
      ssh -o BatchMode=yes "$worker" 'test "$(id -u)" -eq 0 || sudo -n true' || die "SSH to ${worker} must work without a password prompt, with root access or passwordless sudo."
      if ssh -o BatchMode=yes "$worker" 'if [ "$(id -u)" -eq 0 ]; then systemctl is-active --quiet k3s-agent; else sudo -n systemctl is-active --quiet k3s-agent; fi'; then
        say "Worker ${worker} already runs k3s-agent; leaving it unchanged"
        continue
      fi
      printf -v remote_install 'if [ "$(id -u)" -eq 0 ]; then env K3S_URL=%q K3S_TOKEN=%q INSTALL_K3S_CHANNEL=%q sh -s - agent; else sudo env K3S_URL=%q K3S_TOKEN=%q INSTALL_K3S_CHANNEL=%q sh -s - agent; fi' \
        "https://${server_ip}:6443" "$token" "$K3S_CHANNEL" \
        "https://${server_ip}:6443" "$token" "$K3S_CHANNEL"
      ssh -o BatchMode=yes "$worker" "curl -sfL https://get.k3s.io | ${remote_install}"
    done
  fi
fi

if ! command -v helm >/dev/null 2>&1; then
  say "Installing Helm"
  curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | as_root bash
fi
command -v helm >/dev/null 2>&1 || die "Helm installation failed."

context=$(kubectl config current-context 2>/dev/null || printf unknown)
printf '\nThis will install OpenCHAMI dependencies and a disposable smoke-test control plane\n'
printf 'into Kubernetes context: %s\n' "$context"
printf 'It creates temporary, non-durable Vault and LocalStack services.\n'
read -r -p "Continue? [y/N] " answer
[[ "$answer" == [yY] || "$answer" == [yY][eE][sS] ]] || die "Cancelled without applying the OpenCHAMI test stack."

say "Waiting for Kubernetes nodes to become Ready"
if [[ "$local_k3s" == true ]]; then
  for _ in $(seq 1 90); do
    node_count=$(kubectl get nodes --no-headers | awk 'END {print NR}')
    [[ "$node_count" -ge 3 ]] && break
    sleep 5
  done
  [[ "$node_count" -ge 3 ]] || die "Workers have not registered with K3s. Check SSH targets, worker service logs, and connectivity to ${K3S_SERVER_IP:-the server} on port 6443. Rerun this script to resume without teardown."
fi
kubectl wait --for=condition=Ready nodes --all --timeout=450s

say "Installing gateway-api and cert-manager"
kubectl apply --server-side -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/standard-install.yaml
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
kubectl wait --for=condition=Available --timeout=180s -n cert-manager deployment/cert-manager-webhook

say "Installing CloudNativePG and Vault Secrets Operator"
kubectl apply --server-side -f https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.29/releases/cnpg-1.29.0.yaml
helm repo add hashicorp https://helm.releases.hashicorp.com --force-update
helm upgrade --install vault-secrets-operator hashicorp/vault-secrets-operator \
  --namespace vault-secrets-operator-system --create-namespace \
  --set defaultVaultConnection.enabled=false --wait --timeout 5m

say "Installing Envoy Gateway"
helm template eg-crds oci://docker.io/envoyproxy/gateway-crds-helm \
  --version v1.5.1 \
  --set crds.gatewayAPI.enabled=false \
  --set crds.envoyGateway.enabled=true | kubectl apply --server-side -f -
helm upgrade --install envoy-gateway oci://docker.io/envoyproxy/gateway-helm \
  --version v1.5.1 \
  --namespace envoy-gateway-system --create-namespace --skip-crds \
  --wait --timeout 5m
kubectl apply -f - <<'YAML'
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: envoy
spec:
  controllerName: gateway.envoyproxy.io/gatewayclass-controller
YAML
kubectl apply -f - <<YAML
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: selfsigned-smoke
spec:
  selfSigned: {}
YAML

say "Installing temporary Vault and LocalStack in ${TOOLS_NAMESPACE}"
kubectl create namespace "$TOOLS_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: openchami-smoke-vault
  namespace: ${TOOLS_NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: openchami-smoke-vault
  template:
    metadata:
      labels:
        app: openchami-smoke-vault
    spec:
      containers:
        - name: vault
          image: hashicorp/vault:1.21
          args: ["server", "-dev"]
          env:
            - name: VAULT_DEV_ROOT_TOKEN_ID
              value: ${VAULT_TOKEN}
            - name: VAULT_DEV_LISTEN_ADDRESS
              value: 0.0.0.0:8200
          ports:
            - name: http
              containerPort: 8200
          securityContext:
            capabilities:
              add: [IPC_LOCK]
          readinessProbe:
            httpGet:
              path: /v1/sys/health?standbyok=true
              port: 8200
            initialDelaySeconds: 2
            periodSeconds: 3
---
apiVersion: v1
kind: Service
metadata:
  name: openchami-smoke-vault
  namespace: ${TOOLS_NAMESPACE}
spec:
  selector:
    app: openchami-smoke-vault
  ports:
    - name: http
      port: 8200
      targetPort: 8200
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: openchami-smoke-s3
  namespace: ${TOOLS_NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: openchami-smoke-s3
  template:
    metadata:
      labels:
        app: openchami-smoke-s3
    spec:
      containers:
        - name: localstack
          image: localstack/localstack:3
          env:
            - name: SERVICES
              value: s3
            - name: AWS_DEFAULT_REGION
              value: us-east-1
            - name: LOCALSTACK_HOST
              value: localstack
            - name: EDGE_PORT
              value: "10000"
          ports:
            - name: edge
              containerPort: 10000
          readinessProbe:
            httpGet:
              path: /_localstack/health
              port: 10000
            initialDelaySeconds: 5
            periodSeconds: 5
---
apiVersion: v1
kind: Service
metadata:
  name: openchami-smoke-s3
  namespace: ${TOOLS_NAMESPACE}
spec:
  selector:
    app: openchami-smoke-s3
  ports:
    - name: edge
      port: 10000
      targetPort: 10000
YAML
kubectl rollout status --timeout=180s -n "$TOOLS_NAMESPACE" deployment/openchami-smoke-vault
kubectl rollout status --timeout=300s -n "$TOOLS_NAMESPACE" deployment/openchami-smoke-s3

vault_exec() {
  kubectl exec -n "$TOOLS_NAMESPACE" deployment/openchami-smoke-vault -- \
    env VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN="$VAULT_TOKEN" vault "$@"
}

say "Preparing the disposable Vault test data"
vault_exec secrets enable -path=openchami kv-v2 >/dev/null 2>&1 || true
vault_exec auth enable approle >/dev/null 2>&1 || true
vault_exec kv put "openchami/${CLUSTER_NAME}/s3/versitygw" \
  access_key="$S3_ACCESS_KEY" secret_key="$S3_SECRET_KEY" >/dev/null

say "Installing the operator and its CRD"
kubectl apply --server-side --force-conflicts -k "${ROOT_DIR}/config/default"
kubectl -n openchami-operator-system set image \
  deployment/openchami-operator-controller-manager "manager=${OPERATOR_IMAGE}"
kubectl -n openchami-operator-system set env deployment/openchami-operator-controller-manager \
  VAULT_ADDR="$VAULT_ADDRESS" \
  VAULT_AUTH_METHOD=token \
  VAULT_TOKEN="$VAULT_TOKEN" \
  AWS_ENDPOINT_URL="$S3_ENDPOINT" \
  AWS_ACCESS_KEY_ID="$S3_ACCESS_KEY" \
  AWS_SECRET_ACCESS_KEY="$S3_SECRET_KEY" \
  AWS_REGION=us-east-1
kubectl rollout status --timeout=300s -n openchami-operator-system \
  deployment/openchami-operator-controller-manager

say "Applying a minimal SMD + Tokensmith smoke-test control plane"
kubectl apply -f - <<YAML
apiVersion: openchami.openchami.org/v1alpha1
kind: OpenCHAMIControlPlane
metadata:
  name: ${CLUSTER_NAME}
  namespace: default
spec:
  clusterName: ${CLUSTER_NAME}
  domain: ${CLUSTER_NAME}.local
  platform:
    vault:
      address: ${VAULT_ADDRESS}
      authMethod: appRole
      appRoleSecretRef:
        name: ${CLUSTER_NAME}-vault-approle
    objectStorage:
      endpoint: ${S3_ENDPOINT}
      bucket: ${CLUSTER_NAME}-boot-images
      tlsInsecure: true
  networkProbe:
    enabled: false
  services:
    smd:
      enabled: true
      replicas: 1
    tokensmith:
      enabled: true
      oidcProvider: vault
    bootService:
      enabled: false
    metadataService:
      enabled: false
    coreDHCP:
      enabled: false
    magellan:
      enabled: false
  networking:
    gatewayClass: envoy
    tls:
      issuer: selfsigned-smoke
  database:
    instances: 1
    storageSize: 5Gi
  logging:
    enabled: false
YAML

say "Waiting for SMD and Tokensmith to become available"
kubectl rollout status --timeout=15m -n "$CP_NAMESPACE" deployment/tokensmith
kubectl rollout status --timeout=15m -n "$CP_NAMESPACE" deployment/smd
kubectl get openchamicontrolplane "$CLUSTER_NAME" -n default -o wide

say "Querying SMD readiness through a local port-forward"
command -v curl >/dev/null 2>&1 || die "curl is required for the endpoint check."
local_port=${SMOKE_LOCAL_PORT:-27779}
endpoint_url="http://127.0.0.1:${local_port}/hsm/v2/service/ready"
if curl -fsS --max-time 3 "$endpoint_url"; then
  printf '\nSMD is already reachable at %s\n' "$endpoint_url"
  exit 0
fi

forward_log=$(mktemp)
kubectl -n "$CP_NAMESPACE" port-forward --address 127.0.0.1 \
  "service/smd" "${local_port}:27779" >"$forward_log" 2>&1 &
forward_pid=$!
cleanup() {
  kill "$forward_pid" >/dev/null 2>&1 || true
  rm -f "$forward_log"
}
trap cleanup EXIT

for _ in $(seq 1 30); do
  if curl -fsS --max-time 3 "$endpoint_url"; then
    printf '\nSMD is reachable at %s\n' "$endpoint_url"
    exit 0
  fi
  kill -0 "$forward_pid" >/dev/null 2>&1 || { cat "$forward_log" >&2; die "kubectl port-forward exited unexpectedly."; }
  sleep 2
done
cat "$forward_log" >&2
die "SMD did not return HTTP success. Inspect pods and conditions with kubectl -n ${CP_NAMESPACE} get pods and kubectl get openchamicontrolplane ${CLUSTER_NAME} -o yaml."