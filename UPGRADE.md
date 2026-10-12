# UPGRADE.md
<!-- This file is updated on each release. See SERVICES.md for image versions. -->

# Upgrading to openchami-operator (unreleased)

## CRD changes
- Added `spec.services.coreDHCP.configMapRef` (`name`, `key` default
  `config.yml`) for a user-provided CoreDHCP config (issue #66).
- Removed the CRD-level defaults on `spec.services.coreDHCP.unknownLeaseDuration`
  / `knownLeaseDuration`. The mutating webhook still defaults them (`5m` / `1h`)
  when `configMapRef` is unset, so existing objects are unaffected.
- Added `spec.services.bootService.httpBootScript` (default `true`) (issue #69).
  When enabled, the gateway's HTTP listener serves `GET /boot/v1/bootscript`
  directly (new HTTPRoute `boot-service-public-http`) so the bundled iPXE
  binaries can fetch the boot script without trusting a private gateway CA.
  Every other HTTP request still 301s to HTTPS. The route exists only while
  `/boot/v1/bootscript` is also on boot-service's HTTPS `publicRoutes` list.

## Service image updates
- CoreDHCP: `ghcr.io/openchami/coredhcp:v0.3.1` → `ghcr.io/openchami/coresmd:v0.7.1`
  (CoreDHCP with the `coresmd` and `bootloop` plugins). The generated config
  uses only stock plugins and works unchanged on the new image.

## Breaking changes
- The `coredhcp` DaemonSet now also binds UDP 69 as a hostPort (coresmd TFTP).
  Nodes already running a TFTP server on 69 will not schedule the pod.
- The unused `CLUSTER_NAME`, `LEASE_RANGES_JSON`, `UNKNOWN_LEASE_DURATION`
  and `KNOWN_LEASE_DURATION` env vars were removed from the coredhcp container.
- The webhook rejects `configMapRef` combined with `leaseRanges` or either
  lease duration.
- After upgrade, port 80 on the gateway is no longer redirect-only: it serves
  the unauthenticated iPXE boot script in plaintext. Sites whose iPXE trusts
  the gateway certificate, or that don't want any plaintext endpoint, should
  set `spec.services.bootService.httpBootScript: false` before upgrading.
  coresmd configs can switch `ipxe_uri` to `http://<spec.domain>/boot/v1/bootscript`.
- Production note: the operator-generated CoreDHCP config is dev/test only
  (no coresmd, so no SMD-backed leases or iPXE boot). Production clusters
  should move to `configMapRef`; see `docs/install-production.md` §9.

## Upgrade procedure
1. Pin production clusters before upgrading:
   ```
   kubectl patch openchamicontrolplane <name> --type=merge \
     -p '{"spec":{"operatorChannel":"pinned","pinnedVersion":"<current>"}}'
   ```
2. Deploy new operator
3. Validate staging clusters: `kubectl get openchamicontrolplane -A`
4. Unpin production clusters one at a time
5. Verify `status.managedByVersion` matches new version

## Storage version migration
After installing a new operator that ships a new CRD storage version, run:
```
hack/migrate-storage-version.sh
```
The script no-op patches every OpenCHAMIControlPlane, forcing the API server to
re-encode it at the current storage version. Safe to re-run.
