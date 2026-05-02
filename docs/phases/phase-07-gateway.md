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
| `HTTPRoute` `http-redirect` | HTTP → HTTPS 301 |
| `HTTPRoute` `smd` | `/hsm/*` |
| `HTTPRoute` `tokensmith` | `/.well-known/jwks.json`, `/oauth/token`, `/health` |
| `HTTPRoute` `boot-service` | `/boot/*` |
| `HTTPRoute` `metadata-public` | `/cloud-init/*` (no JWT) |
| `HTTPRoute` `metadata-admin` | `/cloud-init/admin/*` (JWT) |
| `SecurityPolicy` `jwt-smd` | JWKS: `http://tokensmith.openchami-{name}.svc.cluster.local:8080/.well-known/jwks.json` |
| `SecurityPolicy` `jwt-boot` | same JWKS |
| `SecurityPolicy` `jwt-metadata-admin` | same JWKS |
| `BackendTrafficPolicy` `smd-ratelimit` | 1000 req/min per X-User-ID header |

All SecurityPolicy resources reference the in-cluster tokensmith JWKS URL,
not the external gateway URL. This avoids a routing loop.

Condition: `GatewayReady=True` when Gateway status `Programmed=True`.

```bash
tools/check-phase.sh 7
```
