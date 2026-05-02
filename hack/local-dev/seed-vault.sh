#!/usr/bin/env bash
# Seeds Vault dev instance with test secrets for cluster "testcluster".
# Idempotent — safe to run multiple times.
set -euo pipefail

VAULT_ADDR=${VAULT_ADDR:-http://localhost:8200}
VAULT_TOKEN=${VAULT_TOKEN:-dev-root-token}
CLUSTER=${1:-testcluster}

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
vault kv put "$PREFIX/db/credentials" \
  SMD_DB_PASSWORD="test-smd-password-$(openssl rand -hex 8)" \
  BOOT_SERVICE_DB_PASSWORD="test-boot-password-$(openssl rand -hex 8)"

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
echo ""
echo "Store these in a Secret named 'openchami-$CLUSTER-approle':"
echo "  kubectl -n openchami-$CLUSTER create secret generic openchami-$CLUSTER-approle \\"
echo "    --from-literal=role_id=$ROLE_ID \\"
echo "    --from-literal=secret_id=$SECRET_ID"
