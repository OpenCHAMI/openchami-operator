# Phase 0 — Bootstrap

**Goal:** Two compiling binaries, passing lint and tests, working local dev.

## Steps

### 0.1 Scaffold
```bash
kubebuilder init --domain openchami.org \
  --repo github.com/openchami/openchami-operator --project-version 3
kubebuilder create api --group openchami --version v1alpha1 \
  --kind OpenCHAMIControlPlane --resource --controller
kubebuilder create webhook --group openchami --version v1alpha1 \
  --kind OpenCHAMIControlPlane --defaulting --validation
kubebuilder create webhook --group openchami --version v1alpha1 \
  --kind OpenCHAMIControlPlane --conversion
```

### 0.2 Dependencies
```bash
go get sigs.k8s.io/controller-runtime@v0.19
go get github.com/hashicorp/vault/api@v1.14
go get github.com/cloudnative-pg/cloudnative-pg@v1.23
go get github.com/hashicorp/vault-secrets-operator/api@v0.10
go get sigs.k8s.io/gateway-api@v1.2
go get github.com/cert-manager/cert-manager@v1.16
go get github.com/aws/aws-sdk-go-v2@v1.30
go get github.com/aws/aws-sdk-go-v2/service/s3
go get github.com/vishvananda/netlink
go get github.com/spf13/cobra@v1.8
go get github.com/onsi/ginkgo/v2@v2.20
go get github.com/onsi/gomega@v1.34
go mod tidy
```

### 0.3 Topology schema stub
Create `internal/reconcilers/topology_schema.go` with a `TopologySpec` struct
containing all fields from the Phase 9 JSON example. Add package-level comment:
"TopologySpec is the canonical schema for the openchami-{cluster}-topology
ConfigMap. This schema is owned by the operator. Services consume it."
Do not implement serialization yet — just the struct definition.

### 0.4 ochami-admin skeleton
`cmd/ochami-admin/main.go` — cobra root with stubs:
`init`, `describe`, `backup`, `restore`, `logs`. All exit 0.

### 0.5 cmd/probe skeleton
`cmd/probe/main.go` — stub binary.

### 0.6 Verify pre-written files compile
Do not recreate these files. Verify `go build ./...` succeeds with them present:
- `internal/conditions/conditions.go`
- `internal/logging/logger.go`
- `internal/reconcilers/reconciler.go`
- `internal/reconcilers/helpers.go`
- `internal/vault/client.go` / `paths.go`
- `internal/s3/client.go`
- `internal/version/version.go` / `images.go`

## Validation
```bash
tools/check-phase.sh 0
```
