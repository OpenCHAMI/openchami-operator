# Phase 3 — Vault Integration

**Files:** `internal/vault/`, `internal/reconcilers/vault.go`, `internal/reconcilers/bucket.go`

## FakeVaultClient — `internal/vault/fake/client.go`

```go
type FakeClient struct {
    mu       sync.Mutex
    Calls    map[string][][]interface{}
    Secrets  map[string]map[string]interface{}
    Policies map[string]string
    // Set to inject errors: Errors["IsReachable"] = errors.New("refused")
    Errors   map[string]error
}

func NewFakeClient() *FakeClient
func (f *FakeClient) AssertCalled(t *testing.T, method string)
func (f *FakeClient) AssertNotCalled(t *testing.T, method string)
func (f *FakeClient) AssertSecretExists(t *testing.T, path string)
func (f *FakeClient) AssertPolicyExists(t *testing.T, name string)
// Implement all Client interface methods
```

## FakeS3Client — `internal/s3/fake/client.go`
Same pattern. Records `EnsureBucket`, `EnsureLifecycleRule` calls.
Supports configurable error injection.

## Vault policy generator — `internal/vault/policies.go`
```go
func ServicesPolicy(clusterName string) string {
    p := Paths(clusterName)
    return fmt.Sprintf(`
path "%s/data/*" { capabilities = ["read"] }
path "pki/issue/openchami-services" { capabilities = ["create", "update"] }
`, p.SecretPrefix)
}
```

## Real Vault client — `internal/vault/client_vault.go`
Implement the `Client` interface using `github.com/hashicorp/vault/api`.
Token acquisition: use K8s SA token (authMethod=kubernetes) or read
role_id + secret_id from the AppRoleSecretRef secret (authMethod=appRole).

## Vault sub-reconciler — `internal/reconcilers/vault.go`
Steps (all idempotent):
1. `IsReachable` → fail: `VaultConfigured=False,Reason=Unreachable`, Event, `RequeueAfter:30s`
2. Acquire token
3. `EnsureKVMount("openchami")`
4. For each credential path: `EnsureSecret(..., overwrite=false)`
   - `paths.DBCredentials`: keys `SMD_DB_PASSWORD`, `BOOT_SERVICE_DB_PASSWORD`
   - `paths.S3Credentials`: keys `access_key`, `secret_key`
   - `paths.LogCredentials`: keys `access_key`, `secret_key`
   - `paths.TokensmithOIDC`: key `client_secret`
   Generate random values (crypto/rand hex) — never overwrite existing.
5. `EnsurePolicy(paths.PolicyServices, ServicesPolicy(clusterName))`
6. If appRole: `EnsureAppRole`
7. If kubernetes: `EnsureKubernetesRole`
8. If tokensmith.oidcProvider=vault: `EnsureOIDCConfig`
9. Apply VSO resources in cluster namespace:
   - `VaultConnection` (one per cluster)
   - `VaultAuth` (kubernetes or appRole based on spec)
   - `VaultStaticSecret` × 4 (db, s3, logs, oidc)
   All with `refreshAfter: 1h`, `destination.create: true`
10. Set `VaultConfigured=True`; write `status.vaultPathPrefix`

## Bucket sub-reconciler — `internal/reconcilers/bucket.go`
1. Wait for VSO to sync `openchami-{clusterName}-s3-creds` Secret (requeue 10s if absent)
2. `s3Client.EnsureBucket(helpers.BootBucketName(cluster))`
3. Set `BucketReady=True`

## Tests
Write `internal/reconcilers/vault_test.go` covering:
- Happy path creates all secrets (verify with FakeVaultClient.AssertSecretExists)
- Vault unreachable → condition=False, requeue
- overwrite=false: existing secrets are not clobbered
- Two clusters produce non-overlapping paths

```bash
tools/check-phase.sh 3
```
