# Image and tooling
IMG ?= ghcr.io/lhns/kube-vnet:latest
CONTROLLER_GEN ?= $(shell go env GOPATH)/bin/controller-gen
CONTROLLER_GEN_VERSION ?= v0.16.5
ENVTEST ?= $(shell go env GOPATH)/bin/setup-envtest
ENVTEST_VERSION ?= release-0.19
ENVTEST_K8S_VERSION ?= 1.31.0

.PHONY: all
all: build

##@ Development

.PHONY: tidy
tidy: ## go mod tidy
	go mod tidy

.PHONY: fmt
fmt: ## go fmt
	go fmt ./...

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: test
test: ## Run unit tests
	go test ./... -count=1

.PHONY: envtest
envtest: ## Install setup-envtest if missing
	@command -v $(ENVTEST) >/dev/null 2>&1 || \
		go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)

.PHONY: integration-test
integration-test: envtest manifests ## Run envtest-backed integration tests
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
	go test -tags integration ./internal/controller/... -count=1 -timeout 300s -v

.PHONY: e2e-up
e2e-up: ## Bootstrap a kind cluster with a CNI + the operator (local dev). CNI defaults to kube-router; pass `calico` for Calico.
	./test/e2e/up.sh

.PHONY: e2e-down
e2e-down: ## Tear down the local e2e kind cluster
	./test/e2e/down.sh

.PHONY: e2e-test
e2e-test: ## Run the e2e suite against the running e2e cluster
	go test -tags e2e ./test/e2e/... -count=1 -v -timeout 15m

.PHONY: e2e
e2e: e2e-up e2e-test ## Bootstrap + run e2e (local). Use e2e-down to tear down.

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -s -w \
              -X main.version=$(VERSION) \
              -X main.commit=$(COMMIT) \
              -X main.date=$(BUILD_DATE)

.PHONY: build
build: ## Build the operator binary (stamped with VERSION/COMMIT/BUILD_DATE)
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/manager ./cmd

.PHONY: run
run: ## Run the operator locally against the current kubeconfig context
	go run ./cmd

##@ Codegen

.PHONY: controller-gen
controller-gen:
	@command -v $(CONTROLLER_GEN) >/dev/null 2>&1 || \
		go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

.PHONY: manifests
manifests: controller-gen ## Generate CRD and RBAC manifests
	$(CONTROLLER_GEN) crd paths="./api/v1alpha1/..." output:crd:dir=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=kube-vnet-manager paths="./internal/controller/..." output:rbac:dir=config/rbac

.PHONY: generate
generate: controller-gen ## Generate deepcopy methods
	$(CONTROLLER_GEN) object paths="./api/v1alpha1/..."

##@ Docker

.PHONY: docker-build
docker-build: ## Build the manager image
	docker build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push the manager image
	docker push $(IMG)

##@ Deploy

.PHONY: install
install: manifests ## Install CRDs into the current cluster
	kubectl apply -f config/crd/bases/

.PHONY: uninstall
uninstall: ## Remove CRDs from the current cluster
	kubectl delete -f config/crd/bases/ --ignore-not-found

.PHONY: deploy
deploy: ## Apply the full default kustomization to the cluster
	kubectl apply -k config/default

.PHONY: undeploy
undeploy: ## Remove the operator from the cluster
	kubectl delete -k config/default --ignore-not-found

.PHONY: release-yaml
release-yaml: ## Render config/default/ into a single release.yaml
	kubectl kustomize config/default > release.yaml

.PHONY: help
help:
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
