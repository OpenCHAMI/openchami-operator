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
CONTROLLER_GEN_VERSION := v0.16.0
ENVTEST_VERSION        := latest

# Image
IMAGE_REGISTRY ?= ghcr.io/openchami
IMAGE_NAME     ?= openchami-operator
IMAGE_TAG      ?= $(VERSION)
IMG            ?= $(IMAGE_REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)

.PHONY: all build generate manifests fmt vet lint test docker-build \
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

lint: ## Run golangci-lint
	@which golangci-lint > /dev/null || \
	  curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | \
	  sh -s -- -b $$(go env GOPATH)/bin
	golangci-lint run ./...

##@ Testing

ENVTEST_ASSETS_DIR := $(shell pwd)/testbin
ENVTEST_K8S_VERSION := 1.31.x

test: ## Run unit tests with envtest
	@mkdir -p $(ENVTEST_ASSETS_DIR)
	KUBEBUILDER_ASSETS=$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_ASSETS_DIR) -p path) \
	  go test -race -count=1 -timeout 5m ./internal/... ./api/...

test-verbose: ## Run unit tests with verbose output
	@mkdir -p $(ENVTEST_ASSETS_DIR)
	KUBEBUILDER_ASSETS=$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_ASSETS_DIR) -p path) \
	  go test -race -v -count=1 -timeout 5m ./internal/... ./api/...

test-cover: ## Run tests with coverage report
	@mkdir -p $(ENVTEST_ASSETS_DIR)
	KUBEBUILDER_ASSETS=$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_ASSETS_DIR) -p path) \
	  go test -race -count=1 -timeout 5m -coverprofile=cover.out ./internal/... ./api/...
	go tool cover -html=cover.out -o cover.html
	@echo "Coverage report: cover.html"

e2e: ## Run end-to-end tests (requires make dev-up first)
	go test -race -v -count=1 -timeout 30m -tags e2e ./test/e2e/...

##@ Local development

dev-up: ## Start local development environment
	@echo "Starting Vault dev + localstack..."
	docker compose -f hack/local-dev/docker-compose.yaml up -d
	@echo "Creating kind cluster..."
	kind create cluster --config hack/local-dev/kind-config.yaml --name openchami-dev 2>/dev/null || \
	  echo "Cluster already exists"
	@echo "Installing prerequisites..."
	$(MAKE) dev-install-prereqs
	@echo "Seeding Vault..."
	hack/local-dev/seed-vault.sh
	@echo ""
	@echo "Dev environment ready."
	@echo "Apply a test cluster: kubectl apply -f test/fixtures/minimal-cluster.yaml"
	@echo "Watch it:            kubectl get openchamicluster testcluster -w"

dev-down: ## Tear down local development environment
	kind delete cluster --name openchami-dev 2>/dev/null || true
	docker compose -f hack/local-dev/docker-compose.yaml down -v

dev-install-prereqs: ## Install operator prerequisites into dev cluster
	@echo "Installing cert-manager..."
	kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
	@echo "Installing CloudNativePG..."
	kubectl apply -f https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/main/releases/cnpg-latest.yaml
	@echo "Installing Vault Secrets Operator..."
	helm repo add hashicorp https://helm.releases.hashicorp.com 2>/dev/null || true
	helm upgrade --install vault-secrets-operator hashicorp/vault-secrets-operator \
	  --namespace vault-secrets-operator-system --create-namespace \
	  --set defaultVaultConnection.enabled=false
	@echo "Installing Envoy Gateway..."
	helm repo add envoy-gateway https://charts.envoyproxy.io 2>/dev/null || true
	helm upgrade --install envoy-gateway envoy-gateway/gateway-helm \
	  --namespace envoy-gateway-system --create-namespace
	@echo "Prerequisites installed."

dev-run: build ## Run operator locally against dev cluster
	OPENCHAMI_DRY_RUN=false \
	KUBECONFIG=~/.kube/config \
	$(OPERATOR_BIN) --zap-encoder=json

dev-run-dry: build ## Run operator in dry-run mode
	OPENCHAMI_DRY_RUN=true \
	KUBECONFIG=~/.kube/config \
	$(OPERATOR_BIN) --zap-encoder=json

##@ Storage version

migrate-storage-version: ## Migrate all OpenCHAMICluster objects to current storage version
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
	  curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | \
	  sh -s -- -b $$(go env GOPATH)/bin
	@which goimports > /dev/null || go install golang.org/x/tools/cmd/goimports@latest
	@which kind > /dev/null || go install sigs.k8s.io/kind@latest
	@echo "All tools installed."

##@ Help

help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
	  /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-28s\033[0m %s\n", $$1, $$2 } \
	  /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
