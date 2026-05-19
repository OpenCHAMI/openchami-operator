#!/usr/bin/env bash
# Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
# SPDX-License-Identifier: MIT
#
# smoke-inventory-round-trip.sh
#
# Smoke test the cross-service inventory chain on a running operator-
# managed control plane:
#
#   1. Wait for OpenCHAMIControlPlane to be Ready.
#   2. Read .status.gateway from the operator (canonical https:// URL +
#      per-service path map), then start ONE port-forward to the
#      Envoy Gateway proxy. Every HTTP call below goes through the
#      gateway via curl --resolve, so there's a single ingress origin
#      to debug rather than three direct backend port-forwards.
#   3. POST fixtures to SMD (RedfishEndpoint, EthernetInterface,
#      Component) — SMD is the source of truth for node identity.
#   4. Rollout-restart boot-service so its on-startup HSM sync pulls
#      the new SMD node into its local /nodes cache. POST
#      bootparameters (boot-service's local augmentation store) and
#      GET the generated iPXE bootscript by both xname and MAC.
#      Assert the script contains the xname (from SMD), the
#      configured kernel/initrd, and the boot-params line.
#   5. Verify metadata-service was wired to SMD + tokensmith by the
#      operator: the per-cluster bootstrap-token Secret exists with a
#      non-empty payload, the Deployment env references it via
#      SecretKeyRef, and the pod log confirms the real (not mock) SMD
#      HTTP client initialised. POST an InstanceInfo CR (cloud-init
#      augmentation only — node identity still comes from SMD at
#      request time) and verify instance_id / hostname / public_keys
#      round-trip.
#
# Architectural invariant exercised:
#   Both boot-service and metadata-service are read-only consumers of
#   SMD for node identity. boot-service's /nodes is a cache populated
#   by background HSM sync; metadata-service resolves the requesting
#   node from SMD on every cloud-init lookup. Neither service should
#   ever be POSTed a Node directly. The only writes this script makes
#   to those services are augmentation data: bootparameters (kernel,
#   initrd, cmdline) and InstanceInfo (cloud-init user-data, SSH keys).
#
# Scope and limits:
#   - This is a *smoke* test of the operator-managed services as black
#     boxes. It does NOT assert source-IP-based node identification on
#     metadata-service (that needs PreserveClientIP=true or a trusted
#     X-Forwarded-For header). For source-IP cloud-init lookups, build
#     the Go e2e test.
#   - Writes to SMD use a JWT minted by tokensmith via its break-glass
#     `user-token create --enable-local-user-mint` CLI path, run by
#     kubectl exec'ing into the tokensmith pod and reading the private
#     key (/var/lib/tokensmith/keys/private.pem). The resulting token
#     is signed by tokensmith's keys — same keys SMD's SMD_JWKS_URL
#     resolves to — so JWKS validation on the SMD side succeeds. The
#     token's `aud` is "smd" and `scope` is "node:read,node:write",
#     which covers the inventory POSTs/GETs this script performs.
#
#     This is local-user-mint, deliberately a break-glass path; never
#     used by production services and not a substitute for real
#     OIDC/RFC 8693 flows. It's appropriate here because the smoke
#     test runs locally against a dev cluster.
#   - Idempotent: every run deletes fixture IDs before posting so a
#     re-run doesn't trip "already exists" errors.
#
# Requires: kubectl, jq, curl. Cluster context defaults to
# kind-openchami-dev (override via KUBE_CONTEXT).

set -euo pipefail

CONTEXT="${KUBE_CONTEXT:-kind-openchami-dev}"
CP_NAME="${CP_NAME:-testcluster}"
NS="openchami-${CP_NAME}"

# Single port-forward to the operator-managed Envoy Gateway proxy.
# Every HTTP call this script makes flows through the gateway's
# path-routed listener — no per-service port-forwards. The host header
# / SNI must match cp.Spec.Domain (the cert is issued for that name),
# so we --resolve it to 127.0.0.1 via curl.
GW_LOCAL_PORT="${GW_LOCAL_PORT:-8443}"

# Fixture identifiers — kept distinct from the dev-fixture xnames so
# this script can run against a cluster that already has data.
#
# HMS xname format (validated by SMD's `couldn't validate endpoint
# data ... is not a valid locational xname ID` check) constrains the
# digits per position — chassis 0-7, BMC 0-1, etc. Cabinet, by
# contrast, accepts 1-4 digits. We pick cabinet 9000 so the xname is
# clearly synthetic / outside any real HPC site cabinet range but
# passes validation; chassis/slot/BMC stay at 0 to keep it short.
FIXTURE_RFE_ID="x9000c0s0b0"
FIXTURE_NODE_ID="x9000c0s0b0n0"
FIXTURE_ETHIF_ID="aabbccddee99"
FIXTURE_MAC="aa:bb:cc:dd:ee:99"

# --- helpers -----------------------------------------------------------------

red()    { printf "\033[31m%s\033[0m\n" "$*" >&2; }
green()  { printf "\033[32m%s\033[0m\n" "$*"; }
yellow() { printf "\033[33m%s\033[0m\n" "$*"; }
step()   { printf "\n\033[1;36m== %s ==\033[0m\n" "$*"; }

require_cmd() {
  for c in "$@"; do
    command -v "$c" >/dev/null 2>&1 || { red "missing required command: $c"; exit 2; }
  done
}

PF_PIDS=()

cleanup() {
  # SIGTERM to direct children first.
  if (( ${#PF_PIDS[@]} > 0 )); then
    for pid in "${PF_PIDS[@]}"; do
      kill "$pid" 2>/dev/null || true
    done
    wait "${PF_PIDS[@]}" 2>/dev/null || true
  fi
  # kubectl port-forward sometimes leaves a child reparented to init
  # before the parent reaps. Sweep any kubectl port-forwards whose
  # parent is us OR whose command line targets our local port — both
  # cases would otherwise block a re-run of this script.
  pkill -P $$ -f 'kubectl.*port-forward' 2>/dev/null || true
  pkill -f "kubectl.*port-forward.*${GW_LOCAL_PORT}:" 2>/dev/null || true
}
trap cleanup EXIT

# wait_for_port host port retries — return 0 once a TCP connection succeeds.
wait_for_port() {
  local host=$1 port=$2 retries=${3:-30}
  for _ in $(seq 1 "$retries"); do
    if (echo > "/dev/tcp/${host}/${port}") 2>/dev/null; then
      return 0
    fi
    sleep 1
  done
  return 1
}

# require_port_free <port> — refuse to start if something else holds the
# local port. Otherwise kubectl port-forward fails silently (it logs to
# /dev/null in this script) and curls land at whatever's already there.
require_port_free() {
  local port=$1
  if (echo > "/dev/tcp/127.0.0.1/${port}") 2>/dev/null; then
    red "local port ${port} is already in use — pick another via the matching *_LOCAL_PORT env var"
    red "  hint: 'ss -tlnp | grep :${port}' or 'lsof -i :${port}' to see who"
    exit 2
  fi
}

# kc <args> — kubectl with the context baked in.
kc() { kubectl --context "$CONTEXT" "$@"; }

# start_portforward <local_port> <service> <remote_port>
start_portforward() {
  local lport=$1 svc=$2 rport=$3
  kc -n "$NS" port-forward "svc/${svc}" "${lport}:${rport}" >/dev/null 2>&1 &
  PF_PIDS+=("$!")
  wait_for_port 127.0.0.1 "$lport" 30 || {
    red "port-forward to ${svc}:${rport} did not become reachable on :${lport}"
    return 1
  }
}

# SMOKE_JWT is the bearer token attached to every SMD request. Minted
# once by mint_jwt() before any HTTP traffic and reused for the run.
SMOKE_JWT=""

# mint_jwt SUBJECT AUDIENCE SCOPES TTL — print a raw JWT on stdout, or
# exit non-zero with a clear error. Runs `tokensmith user-token create
# --enable-local-user-mint ...` inside the tokensmith pod, which signs
# the token with the on-disk RSA key tokensmith's JWKS endpoint
# publishes.
mint_jwt() {
  local subject=$1 audience=$2 scopes=$3 ttl=$4
  local jwt
  jwt=$(kc -n "$NS" exec deploy/tokensmith -- \
    /usr/local/bin/tokensmith user-token create \
      --enable-local-user-mint \
      --subject "$subject" \
      --audience "$audience" \
      --scopes "$scopes" \
      --ttl "$ttl" \
      --key-file /var/lib/tokensmith/keys/private.pem 2>/dev/null | tr -d '[:space:]') || {
    red "failed to mint JWT via tokensmith pod"
    return 1
  }
  # Sanity check: JWTs are exactly three base64url segments joined by '.'.
  if [[ ! "$jwt" =~ ^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$ ]]; then
    red "tokensmith did not return a well-formed JWT (got: '${jwt:0:80}...')"
    return 1
  fi
  printf "%s" "$jwt"
}

# CURL_RESOLVE wires curl's --resolve so that requests for
# <Spec.Domain>:<GW_LOCAL_PORT> end up at 127.0.0.1:<GW_LOCAL_PORT> (the
# port-forwarded envoy proxy). Set after the gateway URL is known so we
# can use it in every curl below; -k because the dev TLS issuer is
# self-signed and not in any local trust store.
CURL_RESOLVE=""

# curl_json METHOD URL [BODY] — emit "<HTTP_STATUS>\n<BODY>"
# Attaches SMOKE_JWT as a bearer token when non-empty; threads the
# --resolve mapping so curl can reach the gateway via the single
# port-forward; -k because the dev cert is self-signed.
curl_json() {
  local method=$1 url=$2 body=${3:-}
  local args=(-sS -k -o /tmp/curl_body.$$ -w '%{http_code}' -X "$method" -H 'Content-Type: application/json')
  if [[ -n "$CURL_RESOLVE" ]]; then
    args+=(--resolve "$CURL_RESOLVE")
  fi
  if [[ -n "$SMOKE_JWT" ]]; then
    args+=(-H "Authorization: Bearer $SMOKE_JWT")
  fi
  args+=("$url")
  if [[ -n "$body" ]]; then args+=(--data "$body"); fi
  local code
  code=$(curl "${args[@]}" || echo "000")
  printf "%s\n" "$code"
  cat /tmp/curl_body.$$ 2>/dev/null || true
  rm -f /tmp/curl_body.$$
}

# expect_2xx STEP_LABEL STATUS_CODE BODY — exit 1 with detail if not 2xx.
expect_2xx() {
  local label=$1 code=$2 body=$3
  if [[ "$code" =~ ^2[0-9][0-9]$ ]]; then
    green "  $label: HTTP $code"
    return 0
  fi
  red "  $label: HTTP $code"
  echo "$body" | head -c 800 >&2
  echo >&2
  if [[ "$code" == "401" || "$code" == "403" ]]; then
    yellow "  hint: gateway/backend rejected the token. Likely causes:"
    yellow "    - tokensmith's signing key changed since the SecurityPolicy"
    yellow "      last fetched JWKS (workaround: 'kubectl rollout restart"
    yellow "      deploy/envoy-gateway -n envoy-gateway-system')"
    yellow "    - SMOKE_JWT expired (TTL was 10m at mint time)"
    yellow "    - SMD's own JWKS_URL still points at a stale tokensmith pod"
    yellow "      ('kubectl rollout restart deploy/smd -n $NS')"
  fi
  exit 1
}

# --- preflight ---------------------------------------------------------------

require_cmd kubectl curl jq

step "Preflight"
ready=$(kc get openchamicontrolplane "$CP_NAME" \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")
if [[ "$ready" != "True" ]]; then
  yellow "control plane $CP_NAME Ready=$ready (continuing — pods may still be reachable)"
fi

# --- read gateway endpoint + per-service paths from .status.gateway ---------
#
# Single source of truth: the operator publishes everything we need
# under .status.gateway (URL + per-route path map). The script does
# not hard-code paths.

step "Discovering gateway endpoint from .status.gateway"
gw_json=$(kc get openchamicontrolplane "$CP_NAME" -o jsonpath='{.status.gateway}' 2>/dev/null || echo "")
if [[ -z "$gw_json" || "$gw_json" == "null" ]]; then
  red "  .status.gateway is empty — Envoy Gateway hasn't reported Programmed=True yet"
  red "  hint: kubectl describe ocp $CP_NAME | grep -A20 'Gateway:'"
  exit 2
fi
GW_URL=$(echo "$gw_json" | jq -r '.url')
GW_HOST=$(echo "$GW_URL" | sed -E 's#^https?://##; s#:.*##')
# Per-service path lookups from the operator's reported map.
PATH_SMD=$(echo "$gw_json" | jq -r '.routes.smd')
PATH_BOOT=$(echo "$gw_json" | jq -r '.routes["boot-service"]')
PATH_BOOT_ADMIN=$(echo "$gw_json" | jq -r '.routes["boot-service-admin"]')
PATH_META_ADMIN=$(echo "$gw_json" | jq -r '.routes["metadata-service-admin"]')
for var_name in GW_HOST PATH_SMD PATH_BOOT PATH_BOOT_ADMIN PATH_META_ADMIN; do
  if [[ -z "${!var_name}" || "${!var_name}" == "null" ]]; then
    red "  expected .status.gateway to include a non-empty $var_name; got '$gw_json'"
    exit 2
  fi
done
green "  GW_URL=$GW_URL  host=$GW_HOST"
green "  routes: smd=$PATH_SMD boot=$PATH_BOOT boot-admin=$PATH_BOOT_ADMIN meta-admin=$PATH_META_ADMIN"

# --- single port-forward to the envoy proxy ---------------------------------
#
# Envoy Gateway names its proxy service "envoy-<gw-ns>-<gw-name>-<hash>"
# in envoy-gateway-system, with a stable label selector. Look it up by
# the owning-gateway labels rather than hard-coding the hash.

step "Starting port-forward to the Envoy Gateway proxy"
require_port_free "$GW_LOCAL_PORT"
proxy_svc=$(kc -n envoy-gateway-system get svc \
  -l "gateway.envoyproxy.io/owning-gateway-namespace=${NS}" \
  -l "gateway.envoyproxy.io/owning-gateway-name=openchami-gateway" \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
if [[ -z "$proxy_svc" ]]; then
  red "  could not find envoy proxy service for gateway ${NS}/openchami-gateway"
  red "  hint: kubectl -n envoy-gateway-system get svc -l gateway.envoyproxy.io/owning-gateway-namespace=${NS}"
  exit 2
fi
# Forward local GW_LOCAL_PORT to the proxy's https listener (port 443
# on the Service, target 10443 on the envoy container — but we forward
# against the Service so kube-proxy handles the indirection).
kc -n envoy-gateway-system port-forward "svc/${proxy_svc}" "${GW_LOCAL_PORT}:443" >/dev/null 2>&1 &
PF_PIDS+=("$!")
wait_for_port 127.0.0.1 "$GW_LOCAL_PORT" 30 || {
  red "  port-forward to ${proxy_svc} did not become reachable on :${GW_LOCAL_PORT}"
  exit 1
}
green "  ${proxy_svc} -> 127.0.0.1:${GW_LOCAL_PORT}"

# Build the absolute URL the script will use as the gateway base —
# includes the local port so curl --resolve maps the cert hostname to
# our port-forwarded socket.
GW="https://${GW_HOST}:${GW_LOCAL_PORT}"
CURL_RESOLVE="${GW_HOST}:${GW_LOCAL_PORT}:127.0.0.1"

# Compose per-service base URLs against the gateway. SMD_URL / BOOT_URL
# are the bare gateway origin because their call sites already include
# the route prefix in the path (e.g. /hsm/v2/State/Components,
# /boot/v1/bootscript) and the gateway forwards those paths as-is to
# the backend. The *_ADMIN bases DO include their admin prefix because
# the gateway URL-rewrites that prefix to '/' before hitting the
# backend — so /admin/boot/nodes lands at boot-service as /nodes.
#
# The split is asymmetric on purpose: SMD/boot-service public APIs
# expect their prefix in the request path (it's part of the API), while
# admin endpoints reuse the backend's bare CRUD routes namespaced by
# the gateway. Sanity-check PATH_SMD / PATH_BOOT against the existing
# call sites so a future route rename here surfaces immediately.
case "$PATH_SMD" in /hsm) ;; *) red "PATH_SMD=$PATH_SMD; smoke test expects /hsm — call sites are hard-coded for that prefix"; exit 2 ;; esac
case "$PATH_BOOT" in /boot) ;; *) red "PATH_BOOT=$PATH_BOOT; smoke test expects /boot"; exit 2 ;; esac
SMD_URL="${GW}"
BOOT_URL="${GW}"
BOOT_ADMIN_URL="${GW}${PATH_BOOT_ADMIN}"
META_ADMIN_URL="${GW}${PATH_META_ADMIN}"

# --- mint smoke-test JWT -----------------------------------------------------

step "Minting smoke-test JWT via tokensmith"
SMOKE_JWT=$(mint_jwt "smoke-test" "smd" "node:read,node:write" "10m")
green "  JWT minted (len=${#SMOKE_JWT})"

# --- clean any prior fixture -------------------------------------------------

step "Cleaning prior fixture state (best-effort, ignore 404s)"
for path in \
  "/hsm/v2/Inventory/EthernetInterfaces/${FIXTURE_ETHIF_ID}" \
  "/hsm/v2/Inventory/RedfishEndpoints/${FIXTURE_RFE_ID}" \
  "/hsm/v2/State/Components/${FIXTURE_NODE_ID}" \
; do
  curl -sS -k --resolve "$CURL_RESOLVE" \
    -o /dev/null -w "  DELETE %{http_code} ${path}\n" \
    -H "Authorization: Bearer ${SMOKE_JWT}" \
    -X DELETE "${SMD_URL}${path}" || true
done

# --- POST RedfishEndpoint ---------------------------------------------------

step "POST RedfishEndpoint -> SMD"
rfe_body=$(jq -n --arg id "$FIXTURE_RFE_ID" --arg mac "$FIXTURE_MAC" '
  { RedfishEndpoints: [{
      ID:       $id,
      FQDN:     ($id + ".smoke.local"),
      MACAddr:  $mac,
      User:     "root",
      Password: "root_password",
      Enabled:  true
    }] }')
out=$(curl_json POST "${SMD_URL}/hsm/v2/Inventory/RedfishEndpoints" "$rfe_body")
code=${out%%$'\n'*}; body=${out#*$'\n'}
expect_2xx "RedfishEndpoint create" "$code" "$body"

# --- POST EthernetInterface --------------------------------------------------

step "POST EthernetInterface -> SMD"
eth_body=$(jq -n \
  --arg id "$FIXTURE_ETHIF_ID" \
  --arg mac "$FIXTURE_MAC" \
  --arg cid "$FIXTURE_NODE_ID" '
  { ID:           $id,
    Description:  "smoke-test nic",
    MACAddress:   $mac,
    IPAddress:    "10.99.0.99",
    ComponentID:  $cid,
    Type:         "Node"
  }')
out=$(curl_json POST "${SMD_URL}/hsm/v2/Inventory/EthernetInterfaces" "$eth_body")
code=${out%%$'\n'*}; body=${out#*$'\n'}
expect_2xx "EthernetInterface create" "$code" "$body"

# --- POST Component (Node) ---------------------------------------------------

step "POST Component (Node) -> SMD"
comp_body=$(jq -n --arg id "$FIXTURE_NODE_ID" '
  { Components: [{
      ID:      $id,
      Type:    "Node",
      State:   "On",
      Flag:    "OK",
      Enabled: true,
      Role:    "Compute",
      NID:     999,
      Arch:    "X86"
    }] }')
out=$(curl_json POST "${SMD_URL}/hsm/v2/State/Components" "$comp_body")
code=${out%%$'\n'*}; body=${out#*$'\n'}
expect_2xx "Component create" "$code" "$body"

# --- GET round-trip from SMD -------------------------------------------------

step "GET fixtures back from SMD"

out=$(curl_json GET "${SMD_URL}/hsm/v2/Inventory/RedfishEndpoints/${FIXTURE_RFE_ID}")
code=${out%%$'\n'*}; body=${out#*$'\n'}
expect_2xx "RedfishEndpoint read" "$code" "$body"
got_mac=$(echo "$body" | jq -r '.MACAddr // .RedfishEndpoints[0].MACAddr // empty')
[[ "$got_mac" == "$FIXTURE_MAC" ]] || { red "  MAC mismatch: expected $FIXTURE_MAC, got '$got_mac'"; exit 1; }
green "  MACAddr round-tripped: $got_mac"

out=$(curl_json GET "${SMD_URL}/hsm/v2/Inventory/EthernetInterfaces/${FIXTURE_ETHIF_ID}")
code=${out%%$'\n'*}; body=${out#*$'\n'}
expect_2xx "EthernetInterface read" "$code" "$body"
got_cid=$(echo "$body" | jq -r '.ComponentID // empty')
[[ "$got_cid" == "$FIXTURE_NODE_ID" ]] || { red "  ComponentID mismatch: expected $FIXTURE_NODE_ID, got '$got_cid'"; exit 1; }
green "  ComponentID round-tripped: $got_cid"

out=$(curl_json GET "${SMD_URL}/hsm/v2/State/Components/${FIXTURE_NODE_ID}")
code=${out%%$'\n'*}; body=${out#*$'\n'}
expect_2xx "Component read" "$code" "$body"
got_role=$(echo "$body" | jq -r '.Role // empty')
[[ "$got_role" == "Compute" ]] || { red "  Role mismatch: expected Compute, got '$got_role'"; exit 1; }
green "  Role round-tripped: $got_role"

# --- boot-service: SMD-sourced node + bootscript round-trip -----------------
#
# Architecture invariant: boot-service is a *consumer* of node identity,
# not a source. Its background HSM sync loop (BOOT_SERVICE_HSM_URL +
# the RFC 8693 bootstrap-token exchange wired by the operator) pulls
# Components/EthernetInterfaces from SMD on a schedule; the local
# /nodes registry is populated by that sync, never by direct POST. The
# only thing this script writes to boot-service is /bootparameters,
# which is augmentation data (kernel/initrd/params) that has no
# counterpart in SMD.
#
# NODE_NAME below is purely for the metadata-service InstanceInfo
# fixture later in the script; boot-service identifies the node by its
# SMD xname/MAC.

NODE_NAME="smoke-${FIXTURE_NODE_ID}"

step "boot-service: trigger HSM sync and wait for the SMD node to appear"
# boot-service v0.1.5's periodic sync runs every 5 minutes (minimum
# granularity is 1 minute — HSMSyncInterval is an int representing
# minutes). It also runs once on startup after HSM is reachable, so
# the fastest reliable way to make a freshly-seeded SMD node land in
# boot-service's /nodes cache is rollout-restart and wait for the
# initial sync. That's what we do here.
#
# This also closes the port-forward to the old pod — we re-establish
# it before the next curl. Without that we'd be talking to an
# Endpoint that no longer exists.
kc -n "$NS" rollout restart deploy/boot-service >/dev/null
green "  boot-service rollout restarted"
kc -n "$NS" rollout status deploy/boot-service --timeout=120s >/dev/null
green "  new boot-service pod ready"

# Re-establish the port-forward against the new pod's Endpoint.
pkill -f "kubectl.*port-forward.*${BOOT_LOCAL_PORT}:" 2>/dev/null || true
sleep 1
start_portforward "$BOOT_LOCAL_PORT" boot-service 27778

# The initial HSM sync happens after the pod reports Ready, so /nodes
# is empty for a few seconds; poll until our SMD-seeded xname surfaces.
# Failure here means the HSM sync path (BOOT_SERVICE_HSM_URL +
# tokensmith bootstrap-token exchange) is broken — the operator wiring
# is in place but the runtime call to SMD isn't succeeding.
sync_deadline=$(( SECONDS + 90 ))
while (( SECONDS < sync_deadline )); do
  if curl -sS -k --resolve "$CURL_RESOLVE" "${BOOT_ADMIN_URL}/nodes" -H "Authorization: Bearer ${SMOKE_JWT}" \
       | jq -e --arg x "$FIXTURE_NODE_ID" '[.[]? | select(.spec.xname==$x)] | length > 0' \
       >/dev/null 2>&1; then
    green "  boot-service synced ${FIXTURE_NODE_ID} from SMD"
    break
  fi
  sleep 3
done
if ! curl -sS -k --resolve "$CURL_RESOLVE" "${BOOT_ADMIN_URL}/nodes" -H "Authorization: Bearer ${SMOKE_JWT}" \
     | jq -e --arg x "$FIXTURE_NODE_ID" '[.[]? | select(.spec.xname==$x)] | length > 0' \
     >/dev/null 2>&1; then
  red "  boot-service did not sync ${FIXTURE_NODE_ID} from SMD within 90s"
  red "  hint: check boot-service logs for HSM-sync errors:"
  red "    kubectl -n $NS logs deploy/boot-service | grep -iE 'hsm|sync|tokensmith'"
  exit 1
fi

step "boot-service: POST bootparameters (augmentation only)"
# bootparameters is boot-service's local augmentation store —
# kernel/initrd/params that SMD has no opinion on. Clean stale
# duplicates first; the legacy POST creates a new BootConfiguration
# named "legacy-<xname>" every time, and the bootscript generator
# picks the OLDEST match if multiples exist.
legacy_name="legacy-${FIXTURE_NODE_ID}"
for uid in $(curl -sS -k --resolve "$CURL_RESOLVE" "${BOOT_ADMIN_URL}/bootconfigurations" -H "Authorization: Bearer ${SMOKE_JWT}" \
  | jq -r --arg n "$legacy_name" '.[]? | select(.metadata.name==$n) | .metadata.uid'); do
  curl -sS -k --resolve "$CURL_RESOLVE" -o /dev/null -w "  cleanup DELETE /bootconfigurations/%{http_code}\n" \
    -H "Authorization: Bearer ${SMOKE_JWT}" \
    -X DELETE "${BOOT_ADMIN_URL}/bootconfigurations/${uid}"
done

bp_body=$(jq -n --arg xname "$FIXTURE_NODE_ID" '
  { hosts:  [$xname],
    params: "console=ttyS0 root=/dev/sda1 smoke=true",
    kernel: "http://example.invalid/kernel-smoke",
    initrd: "http://example.invalid/initrd-smoke"
  }')
out=$(curl_json POST "${BOOT_URL}/boot/v1/bootparameters" "$bp_body")
code=${out%%$'\n'*}; body=${out#*$'\n'}
expect_2xx "bootparameters create" "$code" "$body"

step "boot-service: GET bootscript by xname + by MAC"
# This is the cross-service assertion: xname resolution comes from SMD
# (via the sync above), augmentation (kernel/initrd/params) comes from
# the bootparameters POST. A bootscript that contains all three pieces
# is end-to-end proof that the SMD->boot-service path is wired.
for q in "host=${FIXTURE_NODE_ID}" "mac=${FIXTURE_MAC}"; do
  out=$(curl_json GET "${BOOT_URL}/boot/v1/bootscript?${q}")
  code=${out%%$'\n'*}; body=${out#*$'\n'}
  expect_2xx "bootscript ?${q}" "$code" "$body"
  # boot-service returns 200 with an "Error iPXE Boot Script" body when
  # node resolution fails — distinguish on body content, not status.
  if echo "$body" | grep -q "node not found"; then
    red "  bootscript reports node not found — HSM sync may have regressed:"
    echo "$body" | head -c 400 >&2; echo >&2
    exit 1
  fi
  for expect in "$FIXTURE_NODE_ID" "kernel-smoke" "root=/dev/sda1"; do
    if ! echo "$body" | grep -q "$expect"; then
      red "  bootscript missing expected string: $expect"
      echo "$body" | head -c 400 >&2; echo >&2
      exit 1
    fi
  done
  green "  bootscript references xname, kernel, and params"
done

# --- metadata-service: InstanceInfo round-trip ------------------------------
#
# Metadata-service is also fabrica-shaped. It exposes /instanceinfos
# rather than EC2-style /latest/meta-data. We assert field round-trip
# only — source-IP-keyed cloud-init lookup needs a real client IP and
# is out of scope for this smoke test.

step "metadata-service: /health reachability"
out=$(curl_json GET "${META_ADMIN_URL}/health")
code=${out%%$'\n'*}; body=${out#*$'\n'}
expect_2xx "metadata-service /health" "$code" "$body"

# Verify the operator wired the bootstrap-token + SMD path so the
# metadata-service container will (a) initialize an SMD HTTP client
# pointed at the in-cluster SMD service and (b) hold the RFC 8693
# bootstrap token the future PR build will exchange for an aud=hsm
# JWT. Catches regressions in either the tokensmith reconciler (Secret
# missing/empty) or the metadata-service reconciler (env vars dropped
# or pointed at the wrong Secret).
step "metadata-service: verify bootstrap-token + SMD wiring"
# Mirror internal/reconcilers/helpers.go:SecretName — operator-managed
# per-cluster Secrets are always namespaced "openchami-<cluster>-<suffix>".
meta_secret="openchami-${CP_NAME}-metadata-service-bootstrap-token"
if ! kc -n "$NS" get secret "$meta_secret" >/dev/null 2>&1; then
  red "  bootstrap-token Secret ${meta_secret} missing — tokensmith reconciler did not mint it"
  red "  hint: 'kubectl -n $NS logs deploy/openchami-operator | grep tokensmith-bootstrap'"
  exit 1
fi
green "  bootstrap-token Secret ${meta_secret} present"
# Secret data is base64; the value should be a non-trivial opaque blob
# (>=32 chars). An empty value means tokensmith CLI returned nothing
# and we applied a Secret with no data — a silent regression we hit
# once when the JSON-tag was misnamed (bootstrap_token vs token).
meta_token_len=$(kc -n "$NS" get secret "$meta_secret" \
  -o jsonpath='{.data.token}' | base64 -d 2>/dev/null | wc -c)
if (( meta_token_len < 32 )); then
  red "  bootstrap-token Secret value too short (len=${meta_token_len}) — likely empty"
  exit 1
fi
green "  bootstrap-token Secret payload looks plausible (len=${meta_token_len})"

# Deployment env must reference the bootstrap-token via SecretKeyRef
# and point SMD_URL at the cluster's SMD service. Failures here mean
# the metadata-service reconciler is out of sync with the binary's env
# contract.
meta_env=$(kc -n "$NS" get deploy metadata-service \
  -o jsonpath='{.spec.template.spec.containers[0].env}')
for required in TOKENSMITH_URL TOKENSMITH_TARGET_SERVICE TOKENSMITH_BOOTSTRAP_TOKEN SMD_URL JWKS_URL; do
  if ! echo "$meta_env" | grep -q "\"name\":\"$required\""; then
    red "  metadata-service Deployment env missing $required"
    echo "$meta_env" | jq -r '.[].name' >&2
    exit 1
  fi
done
got_secret_ref=$(echo "$meta_env" \
  | jq -r '.[] | select(.name=="TOKENSMITH_BOOTSTRAP_TOKEN") | .valueFrom.secretKeyRef.name')
if [[ "$got_secret_ref" != "$meta_secret" ]]; then
  red "  TOKENSMITH_BOOTSTRAP_TOKEN points at Secret '$got_secret_ref', expected '$meta_secret'"
  exit 1
fi
green "  metadata-service env wired to SMD + tokensmith + ${meta_secret}"

# The binary logs "SMD_URL configured, using real SMD HTTP client" at
# startup when SMD_URL is non-empty; if env wiring breaks it falls
# back to "using mock SMD client for development" — which would mean
# node/group lookups silently come from the binary's hardcoded fixture
# data, NOT the SMD we just populated. Grep both messages so we fail
# loudly on the bad path instead of trusting the success path.
meta_pod=$(kc -n "$NS" get pod -l app.kubernetes.io/name=metadata-service \
  -o jsonpath='{.items[0].metadata.name}')
if [[ -z "$meta_pod" ]]; then
  red "  no metadata-service pod found via app.kubernetes.io/name selector"
  exit 1
fi
meta_log=$(kc -n "$NS" logs "$meta_pod" --tail=200 2>/dev/null || true)
if echo "$meta_log" | grep -q "using mock SMD client"; then
  red "  metadata-service initialized the MOCK SMD client — env wiring broken"
  red "  ${meta_log}" | head -20 >&2
  exit 1
fi
if ! echo "$meta_log" | grep -q "using real SMD HTTP client"; then
  yellow "  metadata-service log did not include the 'using real SMD HTTP client' marker"
  yellow "  (this can happen if the pod is older than the log tail; not failing)"
else
  green "  metadata-service log confirms real SMD HTTP client initialised"
fi

step "metadata-service: clean prior fixture + POST InstanceInfo"
# InstanceInfo is metadata-service's local augmentation store —
# cloud-init user-data, hostname overrides, SSH keys. It does not
# duplicate node identity from SMD; the binary resolves the node
# (xname/NID/IP) from SMD on the request path and joins it with the
# matching InstanceInfo at serve time. So all we POST here is the
# augmentation: instance_id keyed to the SMD xname plus a hostname
# and a sample public_key.
existing_uid=$(curl -sS -k --resolve "$CURL_RESOLVE" \
  -H "Authorization: Bearer ${SMOKE_JWT}" "${META_ADMIN_URL}/instanceinfos" \
  | jq -r --arg n "$NODE_NAME" '[.[]? | select(.metadata.name==$n)][0].metadata.uid // empty')
if [[ -n "$existing_uid" ]]; then
  curl -sS -k --resolve "$CURL_RESOLVE" -o /dev/null -w "  cleanup DELETE /instanceinfos/%{http_code}\n" \
    -H "Authorization: Bearer ${SMOKE_JWT}" \
    -X DELETE "${META_ADMIN_URL}/instanceinfos/${existing_uid}"
fi

ii_body=$(jq -n --arg name "$NODE_NAME" --arg xname "$FIXTURE_NODE_ID" '
  { metadata: {name: $name},
    spec: {
      instance_id:    $xname,
      hostname:       $name,
      local_hostname: $name,
      public_keys:    ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA smoke@test"]
    }
  }')
out=$(curl_json POST "${META_ADMIN_URL}/instanceinfos" "$ii_body")
code=${out%%$'\n'*}; body=${out#*$'\n'}
expect_2xx "InstanceInfo create" "$code" "$body"
ii_uid=$(echo "$body" | jq -r '.metadata.uid // empty')
[[ -n "$ii_uid" ]] || { red "  no uid returned from InstanceInfo POST"; exit 1; }
green "  uid=${ii_uid}"

step "metadata-service: GET InstanceInfo back"
out=$(curl_json GET "${META_ADMIN_URL}/instanceinfos/${ii_uid}")
code=${out%%$'\n'*}; body=${out#*$'\n'}
expect_2xx "InstanceInfo read" "$code" "$body"
got_iid=$(echo "$body" | jq -r '.spec.instance_id // empty')
[[ "$got_iid" == "$FIXTURE_NODE_ID" ]] || {
  red "  instance_id mismatch: expected $FIXTURE_NODE_ID, got '$got_iid'"
  exit 1
}
got_keys=$(echo "$body" | jq -r '.spec.public_keys | length')
[[ "$got_keys" == "1" ]] || { red "  public_keys length expected 1, got $got_keys"; exit 1; }
green "  instance_id, hostname, public_keys round-tripped"

# --- success -----------------------------------------------------------------

step "Done"
green "Full chain OK: SMD inventory round-trip + boot-service bootscript + metadata-service InstanceInfo."
echo
echo "Fixture left in the cluster:"
echo "  SMD RedfishEndpoint        ${FIXTURE_RFE_ID}"
echo "  SMD EthernetInterface      ${FIXTURE_ETHIF_ID}"
echo "  SMD Component              ${FIXTURE_NODE_ID}"
echo "  boot-service bootparams    legacy-${FIXTURE_NODE_ID} (sync'd from SMD)"
echo "  metadata-service IInfo     ${NODE_NAME} (uid=${ii_uid})"
echo
echo "Re-run is idempotent — script cleans up by metadata.name before each POST."
