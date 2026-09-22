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
  client_id="test-oidc-client-$(openssl rand -hex 8)" \
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

echo ""
echo "Vault seeded for cluster: $CLUSTER"
echo "  role_id:   $ROLE_ID"
echo ""
echo "The operator now provisions the VSO AppRole SecretID itself: after it"
echo "ensures the openchami-$CLUSTER-services AppRole it generates a secret_id,"
echo "writes it to the Kubernetes Secret named by"
echo "spec.platform.vault.appRoleSecretRef (key 'id') in namespace"
echo "openchami-$CLUSTER, and stamps the openchami.org/vault-approle-role-id"
echo "annotation. No manual 'kubectl create secret' step is required (issue #54)."
