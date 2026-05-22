# OpenCHAMI Operator Makefile

BINARY_DIR   := bin
OPERATOR_BIN := $(BINARY_DIR)/operator
ADMIN_BIN    := $(BINARY_DIR)/ochami-admin
PROBE_BIN    := $(BINARY_DIR)/probe

PKG          := github.com/openchami/openchami-operator
VERSION_PKG  := $(PKG)/internal/version
VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT       ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE         ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := \
  -X $(VERSION_PKG).Version=$(VERSION) \
  -X $(VERSION_PKG).GitCommit=$(COMMIT) \
  -X $(VERSION_PKG).BuildDate=$(DATE)

# Tool versions
CONTROLLER_GEN_VERSION := v0.21.0
ENVTEST_VERSION        := latest

# Image
IMAGE_REGISTRY ?= ghcr.io/openchami
IMAGE_NAME     ?= openchami-operator
IMAGE_TAG      ?= $(VERSION)
IMG            ?= $(IMAGE_REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)

.PHONY: all build generate manifests fmt vet lint lint-config test test-e2e docker-build \
        dev-up dev-down e2e migrate-storage-version \
        check-tools install-tools help

all: generate manifests build

##@ Build

build: $(OPERATOR_BIN) $(ADMIN_BIN) $(PROBE_BIN) ## Build all binaries

$(OPERATOR_BIN): $(shell find cmd/operator internal -name '*.go' 2>/dev/null)
	@mkdir -p $(BINARY_DIR)
	go build -ldflags "$(LDFLAGS)" -o $@ ./cmd/operator

$(ADMIN_BIN): $(shell find cmd/ochami-admin internal/admin -name '*.go' 2>/dev/null)
	@mkdir -p $(BINARY_DIR)
	go build -ldflags "$(LDFLAGS)" -o $@ ./cmd/ochami-admin

$(PROBE_BIN): $(shell find cmd/probe internal -name '*.go' 2>/dev/null)
	@mkdir -p $(BINARY_DIR)
	go build -ldflags "$(LDFLAGS)" -o $@ ./cmd/probe 2>/dev/null || true

docker-build: ## Build operator container image
	docker build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg DATE=$(DATE) \
	  -t $(IMG) .

##@ Code generation

generate: controller-gen ## Run controller-gen for deepcopy
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" \
	  paths="./api/..."

manifests: controller-gen ## Generate CRD and webhook manifests
	$(CONTROLLER_GEN) \
	  rbac:roleName=manager-role \
	  crd \
	  webhook \
	  paths="./api/...;./internal/controller/..." \
	  output:crd:artifacts:config=config/crd/bases \
	  output:rbac:artifacts:config=config/rbac \
	  output:webhook:artifacts:config=config/webhook
	@echo "Verifying generated files are committed..."

##@ Quality

fmt: ## Run goimports
	@which goimports > /dev/null || go install golang.org/x/tools/cmd/goimports@latest
	goimports -w .

vet: ## Run go vet
	go vet ./...

lint-config: ## Verify golangci-lint configuration is valid
	@which golangci-lint > /dev/null || \
	  GOBIN=$$(go env GOPATH)/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	golangci-lint config verify

lint: ## Run golangci-lint
	@which golangci-lint > /dev/null || \
	  GOBIN=$$(go env GOPATH)/bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	golangci-lint run ./...

##@ Testing

ENVTEST_ASSETS_DIR := $(shell pwd)/testbin
ENVTEST_K8S_VERSION := 1.31.x

test: envtest ## Run unit tests with envtest
	@mkdir -p $(ENVTEST_ASSETS_DIR)
	KUBEBUILDER_ASSETS=$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_ASSETS_DIR) -p path) \
	  go test -race -count=1 -timeout 5m ./internal/... ./api/... ./cmd/...

test-verbose: ## Run unit tests with verbose output
	@mkdir -p $(ENVTEST_ASSETS_DIR)
	KUBEBUILDER_ASSETS=$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_ASSETS_DIR) -p path) \
	  go test -race -v -count=1 -timeout 5m ./internal/... ./api/... ./cmd/...

test-cover: ## Run tests with coverage report
	@mkdir -p $(ENVTEST_ASSETS_DIR)
	KUBEBUILDER_ASSETS=$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_ASSETS_DIR) -p path) \
	  go test -race -count=1 -timeout 5m -coverprofile=cover.out ./internal/... ./api/... ./cmd/...
	go tool cover -html=cover.out -o cover.html
	@echo "Coverage report: cover.html"

e2e: ## Run end-to-end tests (requires make dev-up first)
	go test -race -v -count=1 -timeout 30m -tags e2e ./test/e2e/...

test-e2e: e2e ## Alias for e2e target (used by CI workflow)

##@ Local development

dev-up: ## Start local development environment
	@echo "Starting Vault dev + localstack..."
	docker compose -f hack/local-dev/docker-compose.yaml up -d
	@echo "Creating kind cluster..."
	@if kind get clusters 2>/dev/null | grep -qx openchami-dev; then \
	  echo "Cluster openchami-dev already exists; ensuring kubeconfig is exported."; \
	  kind export kubeconfig --name openchami-dev; \
	else \
	  kind create cluster --config hack/local-dev/kind-config.yaml --name openchami-dev; \
	fi
	@echo "Waiting for control-plane API to become reachable..."
	@for i in $$(seq 1 30); do \
	  kubectl --context kind-openchami-dev get --raw=/livez >/dev/null 2>&1 && break; \
	  sleep 2; \
	done; kubectl --context kind-openchami-dev get --raw=/livez >/dev/null
	@# Attach the compose-based Vault and LocalStack containers to the
	@# 'kind' docker network so pods inside the cluster (VSO,
	@# operator-managed services) can resolve them by container hostname.
	@# Without this step, in-cluster traffic to 'http://localhost:8200'
	@# resolves to the pod itself, not the host's Vault.
	@echo "Attaching compose containers to kind network..."
	-docker network connect kind openchami-vault-dev 2>/dev/null
	-docker network connect kind openchami-localstack-dev 2>/dev/null
	@echo "Installing prerequisites..."
	$(MAKE) dev-install-prereqs
	@echo "Installing OpenCHAMI CRDs..."
	$(MAKE) install
	@echo "Seeding Vault..."
	hack/local-dev/seed-vault.sh
	@echo ""
	@echo "Dev environment ready. The CRDs are installed; the operator is NOT running yet."
	@echo "Pick one:"
	@echo "  make dev-run                # run the operator on your laptop against the kind cluster"
	@echo "  make dev-deploy             # build + load + deploy the operator INTO the kind cluster"
	@echo ""
	@echo "Then apply a test cluster:"
	@echo "  kubectl apply -f test/fixtures/minimal-controlplane.yaml"
	@echo "  kubectl get openchamicontrolplane testcluster -w"

dev-down: ## Tear down local development environment
	kind delete cluster --name openchami-dev 2>/dev/null || true
	docker compose -f hack/local-dev/docker-compose.yaml down -v

CNPG_VERSION ?= 1.29.0
ENVOY_GATEWAY_VERSION ?= v1.5.1

dev-install-prereqs: ## Install operator prerequisites into dev cluster
	@echo "Installing cert-manager..."
	kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
	@echo "Installing CloudNativePG $(CNPG_VERSION)..."
	kubectl apply --server-side -f https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-$(basename $(CNPG_VERSION))/releases/cnpg-$(CNPG_VERSION).yaml
	@echo "Installing Vault Secrets Operator..."
	helm repo add hashicorp https://helm.releases.hashicorp.com 2>/dev/null || true
	helm upgrade --install vault-secrets-operator hashicorp/vault-secrets-operator \
	  --namespace vault-secrets-operator-system --create-namespace \
	  --set defaultVaultConnection.enabled=false
	@echo "Installing Envoy Gateway $(ENVOY_GATEWAY_VERSION)..."
	helm upgrade --install envoy-gateway oci://docker.io/envoyproxy/gateway-helm \
	  --version $(ENVOY_GATEWAY_VERSION) \
	  --namespace envoy-gateway-system --create-namespace
	@# The Envoy Gateway helm chart installs the controller + CRDs but
	@# does NOT create a default GatewayClass. The operator's fixtures
	@# use spec.networking.gatewayClass: envoy, so dev-up materializes a
	@# GatewayClass of that name pointing at the chart's controller.
	@# Without this, the Gateway CR stays Programmed=Unknown and
	@# GatewayReady never reaches True.
	@echo "Creating envoy GatewayClass..."
	kubectl --context kind-openchami-dev apply -f hack/local-dev/envoy-gatewayclass.yaml
	@# selfsigned-dev ClusterIssuer: the dev fixtures
	@# (test/fixtures/*-cluster.yaml) set spec.networking.tls.issuer to
	@# 'selfsigned-dev'. cert-manager itself doesn't ship a default
	@# ClusterIssuer, so without this step the gateway Certificate stays
	@# in Issuing forever and CertificatesValid never reaches True.
	@# Production deployments configure their own issuer (e.g. an ACME
	@# ClusterIssuer pointing at Let's Encrypt); this is dev only.
	@echo "Waiting for cert-manager webhook to be ready..."
	@kubectl --context kind-openchami-dev wait --for=condition=Available --timeout=120s \
	  -n cert-manager deployment/cert-manager-webhook
	@# Deployment-Available is satisfied as soon as a pod is Ready, but
	@# the cert-manager-webhook Service endpoints can still be empty —
	@# kube-proxy and the webhook server's TLS warmup both lag the pod
	@# readiness probe. Applying a cert-manager resource against an
	@# empty Endpoints slice surfaces as `connection refused` from the
	@# kube-apiserver's webhook call. Wait until the Service has at
	@# least one ready endpoint before proceeding.
	@echo "Waiting for cert-manager-webhook Service endpoints to populate..."
	@for i in $$(seq 1 60); do \
	  if kubectl --context kind-openchami-dev get endpoints -n cert-manager cert-manager-webhook \
	       -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null | grep -q .; then \
	    break; \
	  fi; \
	  sleep 2; \
	done; kubectl --context kind-openchami-dev get endpoints -n cert-manager cert-manager-webhook \
	  -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null | grep -q .
	@echo "Creating selfsigned-dev ClusterIssuer..."
	@# Even with populated endpoints, the first ValidatingWebhookConfiguration
	@# call can still race the apiserver's TLS handshake with the webhook.
	@# A few short retries cover the gap without masking a genuine failure.
	@for i in $$(seq 1 10); do \
	  if kubectl --context kind-openchami-dev apply -f hack/local-dev/selfsigned-issuer.yaml 2>/tmp/cm-apply-err; then \
	    rm -f /tmp/cm-apply-err; \
	    break; \
	  fi; \
	  if [ $$i -eq 10 ]; then \
	    cat /tmp/cm-apply-err; exit 1; \
	  fi; \
	  echo "  selfsigned-issuer apply retry $$i/10..."; \
	  sleep 3; \
	done
	@echo "Prerequisites installed."

##@ Cluster install (kustomize)

# IMG is the container image the in-cluster Deployment runs. Override on the
# CLI for releases: `make deploy IMG=ghcr.io/openchami/openchami-operator:v1.2.3`.
IMG ?= controller:latest

install: manifests ## Install operator CRDs into the current kubectl context
	kubectl apply --server-side -k config/crd

uninstall: ## Remove operator CRDs from the current kubectl context
	kubectl delete --ignore-not-found -k config/crd

deploy: manifests ## Install CRDs + RBAC + manager Deployment into the current context (uses IMG)
	@# Apply with kubectl's built-in kustomize (no external `kustomize` CLI
	@# required), then rewrite the manager image to $(IMG) on the live
	@# Deployment. The two-step approach matters because the kubebuilder
	@# scaffold leaves `image: controller:latest` in config/manager/manager.yaml
	@# as a placeholder — historically rewritten by a `kustomize edit set
	@# image` pre-apply step that silently no-ops when the kustomize CLI
	@# isn't installed, leaving the cluster pulling a non-existent image.
	@# `kubectl set image` works regardless of which CLI is available.
	@# --force-conflicts because every invocation of `kubectl set image` below
	@# claims field-manager ownership of the container image. On the next
	@# `make deploy` the SSA apply contends with that ownership; without
	@# --force the apply fails with a conflict and the user has to clean up
	@# the field manager by hand. Forcing is the right call here: this
	@# Makefile target is the authoritative source for the deployment.
	kubectl apply --server-side --force-conflicts -k config/default
	kubectl -n openchami-operator-system set image deploy/openchami-operator-controller-manager manager=$(IMG)
	kubectl -n openchami-operator-system rollout status deploy/openchami-operator-controller-manager --timeout=120s

undeploy: ## Remove CRDs + RBAC + manager Deployment from the current context
	kubectl delete --ignore-not-found -k config/default

dev-deploy: docker-build ## Build operator image, load into kind, and deploy in-cluster against the dev compose stack
	@# Ensure the dev compose containers (vault + localstack) are reachable
	@# from inside the kind network. Without this, the operator Pod can't
	@# resolve the Vault/S3 endpoints by container name. Errors are non-fatal
	@# (already-attached returns non-zero); the `||true` keeps re-runs idempotent.
	-docker network connect kind openchami-vault-dev 2>/dev/null
	-docker network connect kind openchami-localstack-dev 2>/dev/null
	kind load docker-image $(IMG) --name openchami-dev
	$(MAKE) deploy IMG=$(IMG)
	@# `make deploy` already does kubectl set image + rollout-status. Inject
	@# the dev-only env vars on top so the in-cluster operator reaches Vault
	@# and LocalStack via container DNS instead of 127.0.0.1 (which would
	@# resolve to the Pod's own loopback). Production deployments override
	@# these via a normal kustomize overlay or external-secrets injection.
	kubectl -n openchami-operator-system set env deploy/openchami-operator-controller-manager \
	  VAULT_ADDR=http://openchami-vault-dev:8200 \
	  VAULT_TOKEN=dev-root-token \
	  VAULT_AUTH_METHOD=token \
	  AWS_ENDPOINT_URL=http://openchami-localstack-dev:4566 \
	  AWS_ACCESS_KEY_ID=test \
	  AWS_SECRET_ACCESS_KEY=test \
	  AWS_REGION=us-east-1
	@# Restart unconditionally: when IMG keeps the same tag across rebuilds
	@# (the common dev case — `:VERSION-dirty` is stable until the commit
	@# changes), `kubectl set image` is a no-op because the spec already
	@# references that tag, so kubelet keeps the old container running with
	@# the previously-loaded image content. `kind load docker-image` updates
	@# the tag-to-digest mapping, but only a Pod restart re-resolves it.
	@# Restart-then-rollout-status guarantees the new code is actually live.
	kubectl -n openchami-operator-system rollout restart deploy/openchami-operator-controller-manager
	kubectl -n openchami-operator-system rollout status deploy/openchami-operator-controller-manager --timeout=120s
	@echo ""
	@echo "Operator deployed. Trigger a reconcile or apply a fixture:"
	@echo "  kubectl apply -f test/fixtures/minimal-controlplane.yaml"
	@echo "  kubectl get openchamicontrolplane testcluster -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{\"\\n\"}{end}'"

dev-run: build install ## Run operator locally against dev cluster (installs CRDs first)
	@# Notes on the env vars below:
	@# - ENABLE_WEBHOOKS=false: the webhook server expects TLS certs at
	@#   /tmp/k8s-webhook-server/serving-certs/ that cert-manager provisions
	@#   only for the in-cluster Deployment. Off-cluster runs skip webhook
	@#   setup; admission policies are exercised by `make dev-deploy`.
	@# - VAULT_*: matches what `hack/local-dev/seed-vault.sh` writes. Without
	@#   these the vault sub-reconciler reports VaultConfigured=False/Error
	@#   and the dependent paths (db creds, OIDC) never materialise.
	@# - AWS_*: points the bucket reconcilers at LocalStack (the dev S3
	@#   gateway brought up by `hack/local-dev/docker-compose.yaml`). The
	@#   keys are the LocalStack defaults; AWS_S3_TLS_INSECURE is unset
	@#   because LocalStack serves plain HTTP on 4566.
	@# - OPENCHAMI_BEST_EFFORT_DNS=true: the off-cluster operator can't
	@#   resolve kind-network or compose-network internal hostnames
	@#   (e.g. openchami-vault-dev). The NetworkPolicies reconciler
	@#   downgrades unresolvable Vault/VersityGW hosts to a 0.0.0.0/0
	@#   placeholder peer when this is set, so the rest of the cluster
	@#   can reach Ready. Production (in-cluster operator) leaves this
	@#   unset and stays strict.
	ENABLE_WEBHOOKS=false \
	VAULT_ADDR=http://127.0.0.1:8200 \
	VAULT_TOKEN=dev-root-token \
	VAULT_AUTH_METHOD=token \
	AWS_ENDPOINT_URL=http://127.0.0.1:4566 \
	AWS_ACCESS_KEY_ID=test \
	AWS_SECRET_ACCESS_KEY=test \
	AWS_REGION=us-east-1 \
	OPENCHAMI_BEST_EFFORT_DNS=true \
	OPENCHAMI_DRY_RUN=false \
	KUBECONFIG=~/.kube/config \
	$(OPERATOR_BIN) --zap-encoder=json

dev-run-dry: build install ## Run operator in dry-run mode
	ENABLE_WEBHOOKS=false \
	OPENCHAMI_DRY_RUN=true \
	KUBECONFIG=~/.kube/config \
	$(OPERATOR_BIN) --zap-encoder=json

##@ Storage version

migrate-storage-version: ## Migrate all OpenCHAMIControlPlane objects to current storage version
	hack/migrate-storage-version.sh

##@ Phase validation

check-phase: ## Validate current phase: make check-phase PHASE=0
	tools/check-phase.sh $(PHASE)

validate-invariants: ## Check invariant compliance in source
	tools/validate-invariants.sh

##@ Tools

CONTROLLER_GEN ?= $(shell which controller-gen 2>/dev/null || echo "$(shell go env GOPATH)/bin/controller-gen")
ENVTEST        ?= $(shell which setup-envtest 2>/dev/null || echo "$(shell go env GOPATH)/bin/setup-envtest")

controller-gen:
	@test -x $(CONTROLLER_GEN) || \
	  go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

envtest:
	@test -x $(ENVTEST) || \
	  go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

install-tools: controller-gen envtest ## Install all required tools
	@which golangci-lint > /dev/null || \
	  curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b $(go env GOPATH)/bin v2.12.1
	@which goimports > /dev/null || go install golang.org/x/tools/cmd/goimports@latest
	@which kind > /dev/null || go install sigs.k8s.io/kind@latest
	@echo "All tools installed."

##@ Help

help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
	  /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-28s\033[0m %s\n", $$1, $$2 } \
	  /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
