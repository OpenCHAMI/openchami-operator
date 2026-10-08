#!/usr/bin/env bash
set -euo pipefail

# Single-Node OpenCHAMI Control Plane Deployment
# Run this from your openchami-operator checkout directory.

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CLUSTER_NAME=${CLUSTER_NAME:-local-dev}
TOOLS_NAMESPACE="openchami-tools"
CP_NAMESPACE="openchami-${CLUSTER_NAME}"
OPERATOR_IMAGE=${OPERATOR_IMAGE:-ghcr.io/openchami/openchami-operator:v0.0.8}

VAULT_TOKEN="dev-root-token"
S3_ACCESS_KEY="dev-access-key"
S3_SECRET_KEY="dev-secret-key"
VAULT_ADDRESS="http://openchami-vault.${TOOLS_NAMESPACE}.svc.cluster.local:8200"
S3_ENDPOINT="http://openchami-s3.${TOOLS_NAMESPACE}.svc.cluster.local:10000"

say() { printf '\n==> %s\n' "$*"; }

[[ -f "${ROOT_DIR}/config/default/kustomization.yaml" ]] || { echo "ERROR: Run from a checkout of openchami-operator."; exit 1; }

# 1. Install single-node K3s
if ! command -v kubectl >/dev/null 2>&1; then
  say "Installing K3s on the login node"
  curl -sfL https://get.k3s.io | sh -s - server --write-kubeconfig-mode 644
  export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
  sleep 10
fi
export KUBECONFIG=${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}
kubectl wait --for=condition=Ready nodes --all --timeout=120s

# 2. Install Helm
if ! command -v helm >/dev/null 2>&1; then
  say "Installing Helm"
  curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
fi

# 3. Install Kubernetes Dependencies
say "Installing Gateway API, Cert-Manager, CloudNativePG, and Vault Operator"
kubectl apply --server-side -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/standard-install.yaml
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
kubectl wait --for=condition=Available --timeout=180s -n cert-manager deployment/cert-manager-webhook

kubectl apply --server-side -f https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.29/releases/cnpg-1.29.0.yaml
helm repo add hashicorp https://helm.releases.hashicorp.com --force-update
helm upgrade --install vault-secrets-operator hashicorp/vault-secrets-operator \
  --namespace vault-secrets-operator-system --create-namespace \
  --set defaultVaultConnection.enabled=false --wait --timeout 5m

helm template eg-crds oci://docker.io/envoyproxy/gateway-crds-helm --version v1.5.1 \
  --set crds.gatewayAPI.enabled=false --set crds.envoyGateway.enabled=true | kubectl apply --server-side -f -
helm upgrade --install envoy-gateway oci://docker.io/envoyproxy/gateway-helm --version v1.5.1 \
  --namespace envoy-gateway-system --create-namespace --skip-crds --wait --timeout 5m

kubectl apply -f - <<'YAML'
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: envoy
spec:
  controllerName: gateway.envoyproxy.io/gatewayclass-controller
---
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: selfsigned-smoke
spec:
  selfSigned: {}
YAML

# 4. Install backend services (Vault/S3)
say "Installing local Vault and LocalStack in ${TOOLS_NAMESPACE}"
kubectl create namespace "$TOOLS_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: openchami-vault
  namespace: ${TOOLS_NAMESPACE}
spec:
  replicas: 1
  selector: { matchLabels: { app: openchami-vault } }
  template:
    metadata: { labels: { app: openchami-vault } }
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
          ports: [{ name: http, containerPort: 8200 }]
          securityContext: { capabilities: { add: [IPC_LOCK] } }
---
apiVersion: v1
kind: Service
metadata:
  name: openchami-vault
  namespace: ${TOOLS_NAMESPACE}
spec:
  selector: { app: openchami-vault }
  ports: [{ name: http, port: 8200, targetPort: 8200 }]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: openchami-s3
  namespace: ${TOOLS_NAMESPACE}
spec:
  replicas: 1
  selector: { matchLabels: { app: openchami-s3 } }
  template:
    metadata: { labels: { app: openchami-s3 } }
    spec:
      containers:
        - name: localstack
          image: localstack/localstack:3
          env:
            - name: SERVICES
              value: s3
            - name: AWS_DEFAULT_REGION
              value: us-east-1
            - name: EDGE_PORT
              value: "10000"
          ports: [{ name: edge, containerPort: 10000 }]
---
apiVersion: v1
kind: Service
metadata:
  name: openchami-s3
  namespace: ${TOOLS_NAMESPACE}
spec:
  selector: { app: openchami-s3 }
  ports: [{ name: edge, port: 10000, targetPort: 10000 }]
YAML

kubectl rollout status --timeout=180s -n "$TOOLS_NAMESPACE" deployment/openchami-vault

# 5. Apply the Operator and CRD
say "Installing the operator and configuring Vault"
kubectl exec -n "$TOOLS_NAMESPACE" deployment/openchami-vault -- \
  env VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN="$VAULT_TOKEN" vault secrets enable -path=openchami kv-v2 >/dev/null 2>&1 || true
kubectl exec -n "$TOOLS_NAMESPACE" deployment/openchami-vault -- \
  env VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN="$VAULT_TOKEN" vault auth enable approle >/dev/null 2>&1 || true

kubectl apply --server-side --force-conflicts -k "${ROOT_DIR}/config/default"
kubectl -n openchami-operator-system set image deployment/openchami-operator-controller-manager "manager=${OPERATOR_IMAGE}"
kubectl -n openchami-operator-system set env deployment/openchami-operator-controller-manager \
  VAULT_ADDR="$VAULT_ADDRESS" VAULT_AUTH_METHOD=token VAULT_TOKEN="$VAULT_TOKEN" \
  AWS_ENDPOINT_URL="$S3_ENDPOINT" AWS_ACCESS_KEY_ID="$S3_ACCESS_KEY" \
  AWS_SECRET_ACCESS_KEY="$S3_SECRET_KEY" AWS_REGION=us-east-1
kubectl rollout status --timeout=300s -n openchami-operator-system deployment/openchami-operator-controller-manager

say "Applying the Control Plane Custom Resource"
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
  services:
    smd: { enabled: true, replicas: 1 }
    tokensmith: { enabled: true, oidcProvider: vault }
  networking:
    gatewayClass: envoy
    tls: { issuer: selfsigned-smoke }
  database:
    instances: 1
    storageSize: 5Gi
YAML

# 6. Wait for Operator to provision SMD
say "Waiting for the operator to provision SMD..."
while ! kubectl get namespace "$CP_NAMESPACE" >/dev/null 2>&1; do sleep 2; done
while ! kubectl get deployment smd -n "$CP_NAMESPACE" >/dev/null 2>&1; do sleep 2; done

kubectl rollout status --timeout=15m -n "$CP_NAMESPACE" deployment/smd

say "SMD deployed successfully!"