# Local Vault OIDC CLI testing

This procedure tests the operator-managed Vault clients, a public-client PKCE
login, TokenSmith exchange, and SMD bearer authorization. It is intentionally
narrow: use [Testing](testing.md) for the full repository test matrix.

## Security boundary

For `oidcProvider: vault`, the operator creates two Vault OIDC clients:

| Client | Vault name | Type | Distributed data |
|---|---|---|---|
| TokenSmith | `openchami-<cluster>-tokensmith` | confidential | `client_id` and `client_secret`, only through the TokenSmith Vault/Kubernetes Secret |
| CLI | `openchami-<cluster>-cli` | public | `client_id` only; PKCE is required |

CLI users must never receive or use the TokenSmith `client_secret`. Do not use
the CLI `id_token` as the subject token for TokenSmith: its `aud` is the public
CLI client ID. Pass the Vault **access token** to `/oauth/exchange`; Vault mode
uses UserInfo to resolve that token.

## Start the local stack and configure the CR

```sh
make dev-up

export CLUSTER=testcluster
kubectl patch openchamicontrolplane "$CLUSTER" --type=merge -p "$(cat <<'JSON'
{
  "spec": {
    "services": {
      "tokensmith": {
        "oidcProvider": "vault",
        "cliOIDC": {
          "redirectURIs": ["http://127.0.0.1:8250/callback"],
          "assignments": ["allow_all"]
        }
      }
    }
  }
}
JSON
)"
```

The two `cliOIDC` lists default to the values above when omitted. Wait for the
Vault reconciler and TokenSmith deployment:

```sh
kubectl wait --for=jsonpath='{.status.conditions[?(@.type=="VaultConfigured")].status}'=True \
  openchamicontrolplane/"$CLUSTER" --timeout=2m
kubectl -n "openchami-$CLUSTER" rollout status deployment/tokensmith --timeout=2m
```

Confirm Vault mode is selected without printing confidential credentials:

```sh
kubectl -n "openchami-$CLUSTER" get deployment tokensmith \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="TOKENSMITH_OIDC_PROVIDER_MODE")].value}{"\n"}'
```

The result must be `vault`. External-provider deployments omit this variable.

## Create a Vault userpass user

Point the Vault CLI at the development Vault. The default `make dev-up` stack
exposes it on loopback; set the development root token used by that stack.

```sh
export VAULT_ADDR=http://127.0.0.1:8200
export VAULT_TOKEN=<development-root-token>

vault auth enable userpass || true
vault write auth/userpass/users/openchami-cli \
  password='local-test-password' \
  token_policies=default

export VAULT_USER_TOKEN="$(vault login -method=userpass -field=token \
  username=openchami-cli password='local-test-password')"
```

If `userpass/` is already enabled, skip `vault auth enable userpass`; do not
disable or recreate an auth mount that other local tests use.

## Run the public-client PKCE flow

Read only the public CLI client ID:

```sh
export CLI_CLIENT_ID="$(vault read -field=client_id \
  "identity/oidc/client/openchami-$CLUSTER-cli")"
export REDIRECT_URI=http://127.0.0.1:8250/callback
```

Generate a verifier, S256 challenge, and state:

```sh
eval "$(python3 - <<'PY'
import base64, hashlib, secrets, shlex
verifier = base64.urlsafe_b64encode(secrets.token_bytes(48)).rstrip(b'=').decode()
challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b'=').decode()
state = secrets.token_urlsafe(24)
print('export PKCE_VERIFIER=' + shlex.quote(verifier))
print('export PKCE_CHALLENGE=' + shlex.quote(challenge))
print('export OIDC_STATE=' + shlex.quote(state))
PY
)"
```

Request authorization with the userpass-derived Vault token and capture the
loopback redirect. `curl` does not follow the redirect, so no callback listener
is required:

```sh
export AUTH_URL="$VAULT_ADDR/v1/identity/oidc/provider/openchami/authorize"
export REDIRECT_LOCATION="$(curl -sS -D - -o /dev/null \
  -H "X-Vault-Token: $VAULT_USER_TOKEN" \
  --get "$AUTH_URL" \
  --data-urlencode "client_id=$CLI_CLIENT_ID" \
  --data-urlencode 'response_type=code' \
  --data-urlencode 'scope=openid' \
  --data-urlencode "redirect_uri=$REDIRECT_URI" \
  --data-urlencode "state=$OIDC_STATE" \
  --data-urlencode "code_challenge=$PKCE_CHALLENGE" \
  --data-urlencode 'code_challenge_method=S256' \
  | tr -d '\r' | awk '/^Location:/ {print $2}')"

eval "$(REDIRECT_LOCATION="$REDIRECT_LOCATION" python3 - <<'PY'
import os, shlex, urllib.parse
query = urllib.parse.parse_qs(urllib.parse.urlparse(os.environ['REDIRECT_LOCATION']).query)
print('export AUTH_CODE=' + shlex.quote(query['code'][0]))
print('export RETURNED_STATE=' + shlex.quote(query['state'][0]))
PY
)"
test "$RETURNED_STATE" = "$OIDC_STATE"
```

Exchange the code without a client secret:

```sh
export VAULT_OIDC_RESPONSE="$(curl -sS \
  -X POST "$VAULT_ADDR/v1/identity/oidc/provider/openchami/token" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode 'grant_type=authorization_code' \
  --data-urlencode "code=$AUTH_CODE" \
  --data-urlencode "client_id=$CLI_CLIENT_ID" \
  --data-urlencode "redirect_uri=$REDIRECT_URI" \
  --data-urlencode "code_verifier=$PKCE_VERIFIER")"

export VAULT_ACCESS_TOKEN="$(jq -r '.access_token' <<<"$VAULT_OIDC_RESPONSE")"
test -n "$VAULT_ACCESS_TOKEN" && test "$VAULT_ACCESS_TOKEN" != null
```

The response also contains an `id_token`; retain it only for OIDC identity
claims. Do not send it to TokenSmith's exchange endpoint.

## Exchange through TokenSmith and call SMD

Use the control plane URL reported by the operator:

```sh
export OPENCHAMI_URL="$(kubectl get openchamicontrolplane "$CLUSTER" \
  -o jsonpath='{.status.gateway.url}')"

export TOKENSMITH_RESPONSE="$(curl -sS \
  -X POST "$OPENCHAMI_URL/oauth/exchange" \
  -H "Authorization: Bearer $VAULT_ACCESS_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"target_service":"smd","scope":["read"]}')"

export OPENCHAMI_BEARER="$(jq -r '.access_token' <<<"$TOKENSMITH_RESPONSE")"
test -n "$OPENCHAMI_BEARER" && test "$OPENCHAMI_BEARER" != null

curl -sS -H "Authorization: Bearer $OPENCHAMI_BEARER" \
  "$OPENCHAMI_URL/hsm/v2/State/Components" | jq .
```

For a development CA, pass its CA file with `curl --cacert`; do not normalize
`--insecure` into production instructions. Remove the test user when finished:

```sh
VAULT_TOKEN=<development-root-token> vault delete auth/userpass/users/openchami-cli
```
