#!/usr/bin/env bash
# Migrates all OpenCHAMICluster objects to the current storage version.
# Safe to run multiple times. Run after any CRD storage version change.
set -euo pipefail
echo "Migrating OpenCHAMICluster objects to current storage version..."
count=0
while IFS= read -r resource; do
    kubectl patch "$resource" --type=merge -p '{}' 2>/dev/null && count=$((count+1)) || true
done < <(kubectl get openchamicluster --all-namespaces -o name 2>/dev/null)
echo "Migrated $count object(s)."
