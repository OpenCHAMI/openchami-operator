#!/bin/bash
# Exit immediately if a command exits with a non-zero status
set -e

OUTFILE="openchami-diag-$(date +%Y%m%d-%H%M%S).txt"
NAMESPACE="openchami-frodo"

echo "Gathering OpenCHAMI diagnostic data into $OUTFILE..."

{
    echo "========================================"
    echo "CLUSTER STATE: PODS"
    echo "========================================"
    kubectl get pods -A -o wide

    echo -e "\n========================================"
    echo "OPENCHAMI CONTROL PLANE"
    echo "========================================"
    kubectl describe openchamicontrolplane frodo
    
    echo -e "\n========================================"
    echo "NETWORK POLICIES & ENDPOINTS"
    echo "========================================"
    kubectl get networkpolicy -n "$NAMESPACE"
    kubectl get endpoints -n "$NAMESPACE"

    echo -e "\n========================================"
    echo "SECRETS (BOOTSTRAP TOKENS)"
    echo "========================================"
    kubectl get secrets -n "$NAMESPACE" | grep -i token || echo "No tokens found."

    echo -e "\n========================================"
    echo "OPERATOR LOGS"
    echo "========================================"
    kubectl logs -n openchami-operator-system -l control-plane=controller-manager --tail=200

    echo -e "\n========================================"
    echo "TOKENSMITH LOGS"
    echo "========================================"
    kubectl logs -n "$NAMESPACE" -l app.kubernetes.io/name=tokensmith --tail=200 || echo "No tokensmith logs."

    echo -e "\n========================================"
    echo "METADATA SERVICE LOGS (PREVIOUS CRASH)"
    echo "========================================"
    kubectl logs -n "$NAMESPACE" -l app.kubernetes.io/name=metadata-service --previous --tail=100 2>/dev/null || echo "No previous metadata-service logs found."
    
    echo -e "\n========================================"
    echo "METADATA SERVICE LOGS (CURRENT)"
    echo "========================================"
    kubectl logs -n "$NAMESPACE" -l app.kubernetes.io/name=metadata-service --tail=100 2>/dev/null || echo "No current metadata-service logs found."

} > "$OUTFILE"

echo "Done. Diagnostic snapshot saved to $OUTFILE"
