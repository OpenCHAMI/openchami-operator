#!/usr/bin/env bash
# Seeds Vault dev instance with test secrets for cluster "testcluster".
# Idempotent — safe to run multiple times.
set -euo pipefail

VAULT_ADDR=${VAULT_ADDR:-http://localhost:8200}
VAULT_TOKEN=${VAULT_TOKEN:-dev-root-token}
CLUSTER=${1:-testcluster}
KUBE_CONTEXT=${KUBE_CONTEXT:-kind-openchami-dev}

export VAULT_ADDR VAULT_TOKEN

echo "Seeding Vault at $VAULT_ADDR for cluster: $CLUSTER"

# Enable KV v2 (idempotent)
vault secrets enable -path=openchami kv-v2 2>/dev/null || true

# Enable AppRole auth (idempotent)
vault auth enable approle 2>/dev/null || true

# Enable PKI (idempotent)
vault secrets enable pki 2>/dev/null || true
vault secrets tune -max-lease-ttl=87600h pki 2>/dev/null || true
vault write pki/root/generate/internal \
  common_name="openchami-test-ca" \
  ttl=87600h 2>/dev/null || true

# Write test credentials
PREFIX="openchami/$CLUSTER"
# DB credentials: one per-role path per dbRoleSpec in
# internal/reconcilers/database.go. The vault reconciler reads from
# $PREFIX/db/{smd,boot-service} (see VaultPaths.DBSMDCredentials and
# DBBootServiceCredentials in internal/vault/paths.go) using the
# `username` and `password` keys (VaultKeyDBUsername / VaultKeyDBPassword
# in internal/reconcilers/helpers.go).
#
# EnsureSecret(..., overwrite=false) means: if the path already has a
# secret, leave it alone; if empty, the operator generates random
# credentials itself. Seeding here lets the dev cluster start with
# deterministic passwords so `vault kv get` reproduces them.
vault kv put "$PREFIX/db/smd" \
  username="smd" \
  password="test-smd-password-$(openssl rand -hex 8)"

vault kv put "$PREFIX/db/boot-service" \
  username="boot_service" \
  password="test-boot-password-$(openssl rand -hex 8)"

vault kv put "$PREFIX/s3/versitygw" \
  access_key="test-access-$(openssl rand -hex 8)" \
  secret_key="test-secret-$(openssl rand -hex 16)"

vault kv put "$PREFIX/s3/logs" \
  access_key="test-log-access-$(openssl rand -hex 8)" \
  secret_key="test-log-secret-$(openssl rand -hex 16)"

vault kv put "$PREFIX/oidc/tokensmith-client" \
  client_secret="test-oidc-secret-$(openssl rand -hex 16)"

# Write policy
vault policy write "openchami-$CLUSTER-services" - << POLICY
path "$PREFIX/data/*" {
  capabilities = ["read"]
}
path "pki/issue/openchami-services" {
  capabilities = ["create", "update"]
}
POLICY

# Create AppRole
vault write "auth/approle/role/openchami-$CLUSTER-services" \
  token_policies="openchami-$CLUSTER-services" \
  token_ttl=15m \
  token_max_ttl=1h

ROLE_ID=$(vault read -field=role_id "auth/approle/role/openchami-$CLUSTER-services/role-id")
SECRET_ID=$(vault write -f -field=secret_id "auth/approle/role/openchami-$CLUSTER-services/secret-id")

echo ""
echo "Vault seeded for cluster: $CLUSTER"
echo "  role_id:   $ROLE_ID"
echo "  secret_id: $SECRET_ID"

# Materialize the AppRole credentials as a Kubernetes Secret in the
# operator-managed per-cluster namespace, with the name the
# OpenCHAMIControlPlane fixture references (spec.platform.vault.appRoleSecretRef.name).
# The operator creates the namespace itself when it reconciles the CR;
# we create it here too so the secret is in place when the CR is
# applied. Both are idempotent.
NS="openchami-$CLUSTER"
SECRET_NAME="$CLUSTER-approle"

if kubectl --context "$KUBE_CONTEXT" version --request-timeout=2s >/dev/null 2>&1; then
  echo ""
  echo "Materializing AppRole secret in cluster..."
  kubectl --context "$KUBE_CONTEXT" create namespace "$NS" \
    --dry-run=client -o yaml | kubectl --context "$KUBE_CONTEXT" apply -f - >/dev/null
  # VSO's AppRole auth reads the secret_id from the Secret data key named
  # "id" (not "secret_id"). We also keep "role_id" as a convenience for
  # humans inspecting the Secret; VSO ignores it because the role_id UUID
  # comes from VaultAuth.spec.appRole.roleId (set by the operator after
  # EnsureAppRole).
  kubectl --context "$KUBE_CONTEXT" -n "$NS" create secret generic "$SECRET_NAME" \
    --from-literal=id="$SECRET_ID" \
    --from-literal=role_id="$ROLE_ID" \
    --dry-run=client -o yaml | kubectl --context "$KUBE_CONTEXT" apply -f - >/dev/null
  echo "  created Secret $NS/$SECRET_NAME"
else
  echo ""
  echo "kubectl context '$KUBE_CONTEXT' is not reachable; skipping in-cluster Secret creation."
  echo "Once your kind cluster is up, run:"
  echo "  kubectl -n $NS create secret generic $SECRET_NAME \\"
  echo "    --from-literal=id=$SECRET_ID \\"
  echo "    --from-literal=role_id=$ROLE_ID"
fi
