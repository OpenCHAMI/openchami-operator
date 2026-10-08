#!/bin/bash
# Exit immediately if a command exits with a non-zero status
set -e

echo "Tearing down any existing environment to ensure a clean slate..."
make dev-down || true

echo "Installing prerequisites and controller-gen tools..."
make install-tools

echo "Bringing up the kind cluster, Vault, LocalStack, and CRDs..."
make dev-up

echo "Building and deploying the operator into the kind cluster (Path B)..."
make dev-deploy

echo "Applying the test cluster configuration..."
kubectl apply -f test/fixtures/minimal-controlplane.yaml

echo "Waiting up to 300 seconds for the OpenCHAMI control plane to reach Ready state..."
kubectl wait --for=condition=Ready openchamicontrolplane/testcluster --timeout=300s

echo "Control plane is Ready. Fetching Envoy Gateway service details for Magellan routing:"
kubectl get svc -n envoy-gateway-system