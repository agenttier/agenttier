# AgentTier Makefile
# Build, test, generate, and deploy targets

# Go parameters
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
GO_BUILD_FLAGS ?= -trimpath
LDFLAGS ?= -s -w -X github.com/agenttier/agenttier/pkg/version.Version=$(VERSION)

# Version
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo "unknown")

# Container
REGISTRY ?= ghcr.io/agenttier
CONTROLLER_IMG ?= $(REGISTRY)/controller:$(VERSION)
ROUTER_IMG ?= $(REGISTRY)/router:$(VERSION)
WEBUI_IMG ?= $(REGISTRY)/web-ui:$(VERSION)

# Tools
CONTROLLER_GEN ?= $(shell which controller-gen 2>/dev/null)
GOLANGCI_LINT ?= $(shell which golangci-lint 2>/dev/null)

##@ General

.PHONY: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: build
build: build-controller build-router build-cli ## Build all binaries

.PHONY: build-controller
build-controller: ## Build the controller binary
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GO_BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o bin/controller ./cmd/controller/

.PHONY: build-router
build-router: ## Build the router binary
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GO_BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o bin/router ./cmd/router/

.PHONY: build-cli
build-cli: ## Build the CLI binary
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GO_BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o bin/agenttier ./cmd/cli/

.PHONY: run-controller
run-controller: ## Run the controller locally
	go run ./cmd/controller/ --metrics-bind-address=:8081 --health-probe-bind-address=:8082

.PHONY: run-router
run-router: ## Run the router locally
	go run ./cmd/router/ --listen-addr=:8080

##@ Testing

.PHONY: test
test: ## Run unit tests
	go test -race -coverprofile=coverage.out ./pkg/... ./api/...

# test-integration / test-e2e / test-property are planned test tiers. The
# test/ tree does not exist yet, so each target no-ops with a notice instead of
# failing on a missing package path. CI today runs only `make test` (unit).
.PHONY: test-integration
test-integration: ## Run integration tests (requires a K8s cluster); skips if test/integration/ is absent
	@if [ -d ./test/integration ]; then \
		go test -race -tags=integration ./test/integration/...; \
	else \
		echo "test-integration: no test/integration/ yet — skipping (planned tier)"; \
	fi

.PHONY: test-e2e
test-e2e: ## Run end-to-end tests (requires Kind cluster); skips if test/e2e/ is absent
	@if [ -d ./test/e2e ]; then \
		go test -race -tags=e2e -timeout=30m ./test/e2e/...; \
	else \
		echo "test-e2e: no test/e2e/ yet — skipping (planned tier)"; \
	fi

.PHONY: test-property
test-property: ## Run property-based tests; skips if test/property/ is absent
	@if [ -d ./test/property ]; then \
		go test -race -tags=property -count=1 ./test/property/...; \
	else \
		echo "test-property: no test/property/ yet — skipping (planned tier)"; \
	fi

.PHONY: test-all
test-all: test test-integration test-property ## Run unit tests plus any present integration/property tiers

.PHONY: coverage
coverage: test ## Generate coverage report
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

##@ Code Quality

.PHONY: lint
lint: ## Run linters
	golangci-lint run ./...

.PHONY: fmt
fmt: ## Format Go code
	gofmt -s -w .
	goimports -w -local github.com/agenttier/agenttier .

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: tidy
tidy: ## Run go mod tidy
	go mod tidy

##@ Code Generation

.PHONY: generate
generate: ## Generate deepcopy functions
	controller-gen object:headerFile="scripts/boilerplate.go.txt" paths="./api/..."

.PHONY: manifests
manifests: ## Generate CRD manifests
	@# No rbac:/webhook: markers exist under api/ (RBAC is defined statically in
	@# helm/agenttier/templates/rbac.yaml) — only crd: output is real; asking
	@# controller-gen for rbac/webhook here would write to config/rbac/config/webhook,
	@# directories that don't exist in this repo and never should.
	controller-gen crd:generateEmbeddedObjectMeta=true paths="./api/..." output:crd:artifacts:config=config/crd
	@# Keep the controller's embedded copy (pkg/crds, applied on startup) and
	@# the Helm chart's crds/ dir (registered by `helm install` before any CR
	@# renders) in lockstep with the generated source of truth in config/crd.
	cp config/crd/*.yaml pkg/crds/
	cp config/crd/*.yaml helm/agenttier/crds/

.PHONY: verify-codegen
verify-codegen: generate manifests ## Verify generated code is up to date
	@# Scope to the actual generated files, not whole directories — a stray
	@# untracked file elsewhere under api/ (e.g. a new _test.go from unrelated
	@# work) must not be misreported as stale codegen.
	@if [ -n "$$(git status --porcelain api/v1alpha1/zz_generated.deepcopy.go config/crd/ pkg/crds/*.yaml helm/agenttier/crds/*.yaml)" ]; then \
		echo "Generated files are out of date. Run 'make generate manifests' and commit."; \
		git diff api/v1alpha1/zz_generated.deepcopy.go config/crd/ pkg/crds/*.yaml helm/agenttier/crds/*.yaml; \
		exit 1; \
	fi

##@ Container Images

.PHONY: docker-build
docker-build: docker-build-controller docker-build-router docker-build-webui ## Build all container images

.PHONY: docker-build-controller
docker-build-controller: ## Build controller image
	docker build -t $(CONTROLLER_IMG) -f docker/Dockerfile.controller .

.PHONY: docker-build-router
docker-build-router: ## Build router image
	docker build -t $(ROUTER_IMG) -f docker/Dockerfile.router .

.PHONY: docker-build-webui
docker-build-webui: ## Build web-ui image
	docker build -t $(WEBUI_IMG) -f web-ui/Dockerfile web-ui/

.PHONY: docker-push
docker-push: ## Push all container images
	docker push $(CONTROLLER_IMG)
	docker push $(ROUTER_IMG)
	docker push $(WEBUI_IMG)

.PHONY: docker-buildx
docker-buildx: ## Build and push multi-arch images
	docker buildx build --platform linux/amd64,linux/arm64 -t $(CONTROLLER_IMG) -f docker/Dockerfile.controller --push .
	docker buildx build --platform linux/amd64,linux/arm64 -t $(ROUTER_IMG) -f docker/Dockerfile.router --push .
	docker buildx build --platform linux/amd64,linux/arm64 -t $(WEBUI_IMG) -f web-ui/Dockerfile --push web-ui/

##@ Local Cluster

.PHONY: kind-load
kind-load: docker-build ## Load all images into the kind cluster (CLUSTER=<name> to override)
	kind load docker-image $(CONTROLLER_IMG) --name $${CLUSTER:-agenttier-local}
	kind load docker-image $(ROUTER_IMG) --name $${CLUSTER:-agenttier-local}
	kind load docker-image $(WEBUI_IMG) --name $${CLUSTER:-agenttier-local}

.PHONY: minikube-load
minikube-load: docker-build ## Load all images into the minikube cluster (PROFILE=<name> to override)
	minikube image load $(CONTROLLER_IMG) $${PROFILE:+--profile=$$PROFILE}
	minikube image load $(ROUTER_IMG) $${PROFILE:+--profile=$$PROFILE}
	minikube image load $(WEBUI_IMG) $${PROFILE:+--profile=$$PROFILE}

.PHONY: smoke
smoke: ## Run the smoke test against the currently configured cluster
	bash scripts/smoke-test.sh

##@ Helm

.PHONY: helm-lint
helm-lint: ## Lint Helm chart
	helm lint helm/agenttier/

.PHONY: helm-template
helm-template: ## Render Helm templates
	helm template agenttier helm/agenttier/

.PHONY: helm-package
helm-package: ## Package Helm chart
	helm package helm/agenttier/ -d _output/

##@ Documentation

.PHONY: docs-serve
docs-serve: ## Serve documentation locally
	cd docs && mkdocs serve

.PHONY: docs-build
docs-build: ## Build documentation site
	cd docs && mkdocs build

##@ Cleanup

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin/ _output/ coverage.out coverage.html
	rm -rf web-ui/dist/ web-ui/node_modules/

##@ Install Tools

.PHONY: install-tools
install-tools: ## Install development tools
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest
	go install golang.org/x/tools/cmd/goimports@latest
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $$(go env GOPATH)/bin
