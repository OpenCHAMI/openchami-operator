#!/bin/bash
NAMESPACE="openchami-frodo"

echo "Fetching Vault OIDC configuration..."
kubectl run debug-oidc --rm -i --restart=Never --image=curlimages/curl -n "$NAMESPACE" -- curl -s http://openchami-vault-dev:8200/v1/identity/oidc/provider/openchami/.well-known/openid-configuration
