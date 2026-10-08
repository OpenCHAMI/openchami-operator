# Phase 7 — Gateway and Certificates

## Certificates — `internal/reconcilers/certificates.go`

cert-manager Certificate:
```
Name:        {clusterName}-gateway-tls
SecretName:  spec.networking.tls.secretName (default: {clusterName}-gateway-tls)
IssuerRef:   {kind: ClusterIssuer, name: spec.networking.tls.issuer}
DNSNames:    [spec.domain]
Duration:    72h
RenewBefore: 12h
```

**Expiry tracking:**
Watch the TLS Secret. On update, parse `tls.crt` using `crypto/x509`.
Extract `NotAfter`. Write RFC3339 string to `status.certExpiryTime`.

```
CertificatesValid=True   if NotAfter > now + 24h
CertificatesValid=False  Reason=ExpirationImminent  if 0 < gap <= 24h
CertificatesValid=False  Reason=Expired             if gap <= 0
```

Record Warning Event at <48h remaining. This triggers `status.phase=Degraded`.

## Gateway — `internal/reconcilers/gateway.go`

All resources in `openchami-{clusterName}` namespace.

| Resource | Key details |
|---|---|
| `Gateway` | HTTPS/443 + HTTP/80, hostname=spec.domain |
| `HTTPRoute` `http-redirect` | HTTP → HTTPS 301 (catch-all on the HTTP listener) |
| `HTTPRoute` `boot-service-public-http` | HTTP listener: Exact `GET /boot/v1/bootscript` only (no JWT, no redirect); see below |
| `HTTPRoute` `smd` | `/hsm/*` (JWT) |
| `HTTPRoute` `smd-public` | Exact-path, GET-only allowlist (no JWT); see below |
| `HTTPRoute` `tokensmith` | `/.well-known/jwks.json`, `/oauth/token`, `/health` |
| `HTTPRoute` `boot-service` | `/boot/*` (JWT) |
| `HTTPRoute` `boot-service-public` | Exact-path, GET-only allowlist (no JWT); see below |
| `HTTPRoute` `metadata-public` | `/cloud-init/*` (no JWT) |
| `HTTPRoute` `metadata-admin` | `/cloud-init/admin/*` (JWT) |
| `SecurityPolicy` `jwt-smd` | JWKS: `http://tokensmith.openchami-{name}.svc.cluster.local:8080/.well-known/jwks.json` |
| `SecurityPolicy` `jwt-boot` | same JWKS |
| `SecurityPolicy` `jwt-metadata-admin` | same JWKS |
| `BackendTrafficPolicy` `smd-ratelimit` | 1000 req/min per X-User-ID header |

All SecurityPolicy resources reference the in-cluster tokensmith JWKS URL,
not the external gateway URL. This avoids a routing loop.

### Public read-only routes (`smd-public`, `boot-service-public`)

Some endpoints must be reachable without a JWT (issue #65): SMD registers
a set of GET routes outside its own auth middleware, and clients such as
`ochami` and coresmd call them tokenless; nodes fetch their iPXE boot
script before holding any credential. The operator publishes these as a
separate HTTPRoute per service with **no** SecurityPolicy attached.

- Every match is `Exact` path + `method: GET`. The method is hard-coded;
  the spec can only choose paths, so a public route can never expose a write.
- Gateway API precedence (Exact beats PathPrefix, method match beats
  none) routes `GET <listed path>` to the public route; every other
  method, sub-path, or unlisted path falls through to the JWT-gated
  prefix route (fail closed).
- Applied in the always-safe set (before tokensmith is Ready), since no
  JWKS fetch is involved.
- Deleted explicitly when disabled or when the service stops being
  deployed in-cluster. SSA alone would leave a stale unauthenticated route.

Defaults (configurable via `spec.services.{smd,bootService}.publicRoutes`):

| Service | Default public GET paths |
|---|---|
| SMD | `/hsm/v2/service/{ready,liveness,values}`, `/hsm/v2/service/values/{arch,class,flag,nettype,role,subrole,state,type}`, `/hsm/v2/State/Components`, `/hsm/v2/Inventory/EthernetInterfaces` (mirrors SMD `generatePublicRoutes`) |
| boot-service | `/boot/v1/bootscript`, `/boot/v1/service/status`, `/boot/v1/service/version` |

```yaml
spec:
  services:
    smd:
      publicRoutes:
        enabled: true            # default; false = everything needs a JWT
        paths:                   # optional; replaces the defaults
          - /hsm/v2/State/Components
    bootService:
      publicRoutes:
        enabled: false
```

CRD validation (CEL) rejects paths outside the service's prefix (`/hsm/`,
`/boot/`), paths with wildcard or query characters, and paths that aren't
normalized (`//`, `.`, `..` segments).

### Plain-HTTP boot script (`boot-service-public-http`, issue #69)

The iPXE binaries bundled with coresmd don't trust a private gateway CA,
so a node following `http-redirect` to HTTPS fails TLS before it can fetch
its boot script. The operator therefore also serves the boot script on the
**HTTP** listener:

```
HTTP :80
  ├─ GET /boot/v1/bootscript   → boot-service   (boot-service-public-http)
  └─ everything else           → 301 https://…  (http-redirect)
```

- Attached only to the `http` listener, hostname = `spec.domain`.
- One match: `Exact /boot/v1/bootscript` + `method: GET`. Query strings
  (`?mac=…`) aren't part of path matching, so they still match. Sub-paths,
  other methods, and the other boot public paths (`/boot/v1/service/*`)
  still redirect. Nothing JWT-gated or admin-facing is ever on `:80`.
- Out-ranks `http-redirect` by standard precedence: the redirect has no
  matches (implicit `PathPrefix /`), and Exact + method beats that.
- No SecurityPolicy. Published in the always-safe set.
- Exists only while boot-service is deployed in-cluster,
  `spec.services.bootService.httpBootScript` is true (the default), **and**
  `/boot/v1/bootscript` is in the resolved HTTPS public path list. So
  `publicRoutes.enabled: false`, or a `publicRoutes.paths` override that
  omits the boot script, also removes the plaintext route — making the boot
  script JWT-only on HTTPS never leaves an unauthenticated HTTP bypass.
  Otherwise the route is deleted explicitly on reconcile.
- The HTTPS `boot-service-public` route is unchanged, and
  `.status.gateway.url` stays `https://`.

Sites whose gateway certificate iPXE already trusts can keep `:80`
redirect-only:

```yaml
spec:
  services:
    bootService:
      httpBootScript: false
```

SMD still enforces its own auth, so listing a path SMD protects only moves
the 401 from Envoy to SMD. boot-service has **no** inbound auth, so for it
the gateway is the only gate. Keep its public list to non-sensitive reads.

Each `remoteJWKS` also carries an explicit `backendRefs` entry pointing at
the in-cluster `tokensmith` Service (port 8080). Newer Envoy Gateway
releases require this: without it the gateway auto-derives a backend from
the URI that does not pick up the `tokensmith-backend-tls`
`BackendTLSPolicy`, so the JWKS fetch fails its TLS handshake with
`tls: unknown certificate authority`. The `backendRef` is omitted when
tokensmith is served by an `externalEndpoint` (the site owns that TLS
trust chain).

Condition: `GatewayReady=True` when Gateway status `Programmed=True`.

```bash
tools/check-phase.sh 7
```
