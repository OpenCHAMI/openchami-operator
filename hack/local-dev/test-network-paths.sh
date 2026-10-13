#!/bin/bash
NAMESPACE="openchami-frodo"

echo "1. Testing internal DNS and routing to Tokensmith..."
kubectl run debug-net --rm -i --restart=Never --image=curlimages/curl -n "$NAMESPACE" -- curl -s -v -m 5 http://tokensmith:8080/health

echo -e "\n\n2. Testing cluster-to-Vault routing..."
kubectl run debug-net --rm -i --restart=Never --image=curlimages/curl -n "$NAMESPACE" -- curl -s -v -m 5 http://openchami-vault-dev:8200/v1/sys/health
