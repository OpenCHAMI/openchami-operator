#!/usr/bin/env bash
set -euo pipefail

# Run the disposable OpenCHAMI smoke test locally on macOS using Docker, kind,
# the repo's dev Vault/LocalStack, and a locally running operator process.

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CONTEXT=kind-openchami-dev
CLUSTER=smoke-local
NAMESPACE="openchami-${CLUSTER}"
DEFAULT_LOCAL_PORT=27779
ENDPOINT_PATH=/hsm/v2/service/ready
TEMP_DIR=$(mktemp -d)

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

say() {
  printf '\n==> %s\n' "$*"
}

cleanup() {
  rm -rf "$TEMP_DIR"
}
trap cleanup EXIT

[[ $(uname -s) == Darwin ]] || die "This script is for your Mac. Use hack/baremetal-smoke-test.sh on the Linux bare-metal host."
for tool in docker kind kubectl helm make go curl lsof; do
  command -v "$tool" >/dev/null 2>&1 || die "Required command not found: $tool"
done
[[ -f "${ROOT_DIR}/test/fixtures/local-smd-smoke.yaml" ]] || die "Run from the openchami-operator checkout."
docker info >/dev/null 2>&1 || die "Docker Desktop must be running."

port_in_use() {
  lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
}

choose_port() {
  local candidate=$1
  while port_in_use "$candidate"; do
    candidate=$((candidate + 1))
  done
  printf '%s' "$candidate"
}

kind_config="${ROOT_DIR}/hack/local-dev/kind-config.yaml"
if ! kind get clusters 2>/dev/null | grep -qx openchami-dev; then
  http_port=8080
  https_port=8443
  port_in_use "$http_port" && http_port=$(choose_port 18080)
  port_in_use "$https_port" && https_port=$(choose_port 18443)
  kind_config="${TEMP_DIR}/kind-config.yaml"
  sed \
    -e "s/hostPort: 8080/hostPort: ${http_port}/" \
    -e "s/hostPort: 8443/hostPort: ${https_port}/" \
    "${ROOT_DIR}/hack/local-dev/kind-config.yaml" > "$kind_config"
fi

say "Starting or reusing the local development dependencies"
make -C "$ROOT_DIR" dev-up KIND_CONFIG="$kind_config"
kubectl --context "$CONTEXT" get nodes >/dev/null || die "kind cluster is not reachable."

say "Starting or reusing the local operator"
operator_pid=$(ps -Ao pid=,command= | awk '$0 ~ /[b]in\/operator --zap-encoder=json/ {print $1; exit}')
operator_log="${TMPDIR:-/tmp}/openchami-dev-run.log"
if [[ -z "$operator_pid" ]]; then
  nohup make -C "$ROOT_DIR" dev-run > "$operator_log" 2>&1 < /dev/null &
  operator_pid=$!
  printf 'Started operator (pid %s); log: %s\n' "$operator_pid" "$operator_log"
else
  printf 'Reusing operator process (pid %s).\n' "$operator_pid"
fi

ready=false
for _ in $(seq 1 180); do
  if ! kill -0 "$operator_pid" >/dev/null 2>&1; then
    die "The local operator exited. Inspect ${operator_log}."
  fi
  if ps -Ao command= | awk '$0 ~ /[b]in\/operator --zap-encoder=json/ {found=1} END {exit !found}'; then
    ready=true
    break
  fi
  sleep 2
done
[[ "$ready" == true ]] || die "The local operator did not start. Inspect ${operator_log}."

say "Applying or updating the local SMD smoke-test resource"
kubectl --context "$CONTEXT" apply -f "${ROOT_DIR}/test/fixtures/local-smd-smoke.yaml"
kubectl --context "$CONTEXT" rollout status --timeout=15m -n "$NAMESPACE" deployment/tokensmith
kubectl --context "$CONTEXT" rollout status --timeout=15m -n "$NAMESPACE" deployment/smd

local_port=$DEFAULT_LOCAL_PORT
endpoint_url="http://127.0.0.1:${local_port}${ENDPOINT_PATH}"
if curl -fsS --max-time 3 "$endpoint_url"; then
  printf '\nSMD is already reachable at %s\n' "$endpoint_url"
else
  port_in_use "$local_port" && local_port=$(choose_port 27800)
  endpoint_url="http://127.0.0.1:${local_port}${ENDPOINT_PATH}"
  forward_log="${TEMP_DIR}/port-forward.log"
  kubectl --context "$CONTEXT" -n "$NAMESPACE" port-forward --address 127.0.0.1 \
    "service/smd" "${local_port}:27779" >"$forward_log" 2>&1 &
  forward_pid=$!
  forwarded=false
  for _ in $(seq 1 30); do
    if curl -fsS --max-time 3 "$endpoint_url"; then
      printf '\nSMD is reachable at %s\n' "$endpoint_url"
      printf 'The temporary port-forward stops when this script exits.\n'
      forwarded=true
      break
    fi
    kill -0 "$forward_pid" >/dev/null 2>&1 || { cat "$forward_log" >&2; die "kubectl port-forward exited unexpectedly."; }
    sleep 2
  done
  kill "$forward_pid" >/dev/null 2>&1 || true
  wait "$forward_pid" 2>/dev/null || true
  [[ "$forwarded" == true ]] || die "SMD did not answer at ${endpoint_url}. Inspect the service Pods and controller status."
fi

say "Local status"
kubectl --context "$CONTEXT" get pods -n "$NAMESPACE" -o wide
kubectl --context "$CONTEXT" get openchamicontrolplane "$CLUSTER" -n default -o wide