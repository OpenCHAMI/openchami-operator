#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
bootstrap=$(awk '/^if \[\[ "\$use_existing" != true/ {exit} !/^ROOT_DIR=/ {print}' "$ROOT_DIR/hack/baremetal-smoke-test.sh")

run_case() (
  platform=$1
  target_context=$2
  export KUBECONFIG=/test/explicit.kubeconfig
  uname() { printf '%s\n' "$platform"; }
  id() { printf '0\n'; }
  sudo() { return 0; }
  curl() { printf 'Unexpected download\n' >&2; exit 91; }
  kubectl() {
    if [[ "$*" == 'config current-context' ]]; then
      printf '%s\n' "$target_context"
    else
      printf 'node-one Ready control-plane 1d v1.32.0\n'
    fi
  }
  exec() {
    [[ "$1" == bash && "$2" == "$ROOT_DIR/hack/local-mac-smoke-test.sh" ]]
    printf 'LOCAL_RUNNER_SELECTED\n'
    exit 0
  }
  eval "$bootstrap"
)

for platform in Darwin Linux; do
  output=$(run_case "$platform" kind-openchami-dev)
  [[ "$output" == *LOCAL_RUNNER_SELECTED* ]]
  printf 'PASS: %s selects the local runner\n' "$platform"
done

if output=$(run_case Linux kind-unrelated 2>&1); then
  printf 'FAIL: unrelated kind context accepted\n' >&2
  exit 1
fi
[[ "$output" == *"not the bare-metal target"* ]]
printf 'PASS: unrelated kind context is protected\n'