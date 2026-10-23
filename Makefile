# Image URL to use all building/pushing image targets
IMG ?= controller:latest
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL is the tool that builds, saves and pushes images: Docker when
# it is installed, otherwise Podman. Override with CONTAINER_TOOL=<tool>.
# It is empty when neither exists; only the container targets need it.
CONTAINER_TOOL ?= $(shell command -v docker >/dev/null 2>&1 && echo docker || { command -v podman >/dev/null 2>&1 && echo podman; })
# Kind talks to Docker unless it is told otherwise.
ifeq ($(notdir $(CONTAINER_TOOL)),podman)
export KIND_EXPERIMENTAL_PROVIDER ?= podman
endif

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: format-check
format-check: ## Fail if a Go file is not gofmt-formatted; it writes nothing ('make fmt' formats).
	@files="$$(go list -f '{{$$d := .Dir}}{{range .GoFiles}}{{$$d}}/{{.}} {{end}}{{range .CgoFiles}}{{$$d}}/{{.}} {{end}}{{range .TestGoFiles}}{{$$d}}/{{.}} {{end}}{{range .XTestGoFiles}}{{$$d}}/{{.}} {{end}}{{range .IgnoredGoFiles}}{{$$d}}/{{.}} {{end}}' ./...)"; \
	unformatted="$$(gofmt -l $$files | sed 's|^$(CURDIR)/||')"; \
	if [ -n "$$unformatted" ]; then \
		echo "$$unformatted"; \
		echo "error: the files above are not gofmt-formatted. Run 'make fmt', then commit the result." >&2; \
		exit 1; \
	fi

# check-generated regenerates, formats and tidies a temporary copy of the
# working tree (its tracked and untracked, not ignored, files) and compares that
# copy with the git index, as 'git diff' would, so it never writes the working
# tree. Run it on a clean checkout or after committing.
# Regenerating rewrites the boilerplate year (YEAR defaults to the current
# year), so a header that differs only in its copyright year is ignored;
# otherwise every PR would fail each January.
# git diff does not see files that controller-gen creates for a new type, so
# untracked files under api and config fail too.
.PHONY: check-generated
check-generated: controller-gen ## Fail if generated files, formatting or go.mod/go.sum differ from what is committed. It does not change the working tree.
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	git ls-files -z --cached --others --exclude-standard | \
		while IFS= read -r -d '' f; do if [ -e "$$f" ] || [ -L "$$f" ]; then printf '%s\0' "$$f"; fi; done | \
		tar --null -T - -cf - | { mkdir "$$tmp/tree" && tar -xf - -C "$$tmp/tree"; }; \
	cp "$$(git rev-parse --git-path index)" "$$tmp/index"; \
	gitdir="$$(git rev-parse --absolute-git-dir)"; \
	cd "$$tmp/tree"; \
	$(MAKE) --no-print-directory manifests generate fmt LOCALBIN="$(LOCALBIN)" CONTROLLER_GEN="$(CONTROLLER_GEN)" YEAR="$(YEAR)"; \
	go mod tidy; \
	gitc() { GIT_INDEX_FILE="$$tmp/index" git --git-dir="$$gitdir" --work-tree="$$tmp/tree" "$$@"; }; \
	gitc diff --exit-code -I'^Copyright [0-9]{4}\.$$' || { \
		echo "error: the files above differ from what is committed. Run 'go mod tidy' and 'make manifests generate fmt', then commit the result." >&2; \
		exit 1; }; \
	untracked="$$(gitc ls-files --others --exclude-standard -- api config)"; \
	if [ -n "$$untracked" ]; then \
		echo "$$untracked"; \
		echo "error: the generated files above are not committed." >&2; \
		exit 1; \
	fi

.PHONY: verify
verify: check-generated format-check vet lint test ## Run every non-E2E check: check-generated, format-check, vet, lint and test. CI runs it.

.PHONY: test
test: vet setup-envtest ## Run tests. It does not regenerate or format: run make manifests generate fmt, or make check-generated.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup installs the pinned Kind into bin/ and builds/loads the manager image locally.
# kubectl kuberc is disabled by default for test isolation; enable with:
# - KUBECTL_KUBERC=true
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
KIND_CLUSTER ?= cubrid-kubernetes-operator-test-e2e
# make test-e2e creates the Kind cluster KIND_CLUSTER and deletes it when the
# run ends, also after a failure, a timeout or Ctrl-C (hack/run-e2e.sh). It
# refuses to start when a cluster with that name exists; E2E_REUSE_CLUSTER=1
# runs against that cluster instead and keeps it.
E2E_REUSE_CLUSTER ?=
# E2E_LABEL_FILTER selects scenarios by Ginkgo label, e.g. E2E_LABEL_FILTER=S00
# for one scenario or E2E_LABEL_FILTER='!db' to leave out the real-database ones.
# E2E_EVIDENCE_DIR receives the run's evidence: summary.json, junit.xml and each
# scenario's files (docs/testing/scenario-contract.md). The run fails unless every
# required scenario passed; with E2E_LABEL_FILTER it is a filtered baseline that
# does not validate the Kind lane (section "Required scenarios").
# The scenarios' time limits have defaults for Kind on a GitHub runner and can be
# set through E2E_FORMATION_LIMIT, E2E_FAILOVER_LIMIT and the others listed in
# docs/testing/scenario-contract.md, section "Time limits", e.g. E2E_FAILOVER_LIMIT=2m.
# A run starts with an empty evidence directory. The default directory is
# emptied by `make test-e2e`; a directory that E2E_EVIDENCE_DIR names is left to
# its owner, and the suite does not start while it holds anything.
E2E_LABEL_FILTER ?=
E2E_EVIDENCE_DIR ?= $(CURDIR)/e2e-evidence
# The run's deadlines, innermost first: Ginkgo's suite timeout, the grace each
# cleanup node gets after it, and Go's process timeout, which kills without any
# cleanup. The CI step and job limits lie outside them. See
# docs/testing/scenario-contract.md, section "A run that does not finish".
E2E_SUITE_TIMEOUT ?= 50m
E2E_GRACE_PERIOD ?= 1m
E2E_GO_TIMEOUT ?= 58m

.PHONY: setup-test-e2e
setup-test-e2e: container-tool kind ## Create the Kind cluster KIND_CLUSTER if it does not exist, for E2E_REUSE_CLUSTER=1 runs
	@if $(KIND) get clusters | grep -Fqx -- '$(KIND_CLUSTER)'; then \
		echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation."; \
	else \
		echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
		$(KIND) create cluster --name '$(KIND_CLUSTER)' --image '$(KIND_NODE_IMAGE)'; \
	fi

.PHONY: test-e2e
test-e2e: container-tool kind vet ## Run the e2e tests on a Kind cluster the run creates and deletes (E2E_REUSE_CLUSTER=1 uses an existing one and keeps it).
	$(if $(filter file,$(origin E2E_EVIDENCE_DIR)),rm -rf '$(E2E_EVIDENCE_DIR)')
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) KIND_NODE_IMAGE=$(KIND_NODE_IMAGE) CONTAINER_TOOL=$(CONTAINER_TOOL) \
		E2E_REUSE_CLUSTER='$(E2E_REUSE_CLUSTER)' E2E_EVIDENCE_DIR='$(E2E_EVIDENCE_DIR)' \
		hack/run-e2e.sh go test -tags=e2e ./test/e2e/ -v -timeout $(E2E_GO_TIMEOUT) -ginkgo.timeout=$(E2E_SUITE_TIMEOUT) -ginkgo.grace-period=$(E2E_GRACE_PERIOD) -ginkgo.v $(if $(E2E_LABEL_FILTER),-ginkgo.label-filter='$(E2E_LABEL_FILTER)')

.PHONY: cleanup-test-e2e
cleanup-test-e2e: kind ## Delete the Kind cluster KIND_CLUSTER, for one that setup-test-e2e created
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

# A lab run is started by hand only, against the shared multi-VM lab; see
# docs/testing/lab-runs.md. LAB_CONTEXT is the kubectl context to use and
# LAB_TARGET the lab that context must lead to (its cubrid-lab/lab marker).
# LAB_COMMAND runs while the run holds the lab's campaign Lease, with KUBECONFIG
# pinned to LAB_CONTEXT. `make test-e2e` never uses the lab.
LAB_CONTEXT ?=
LAB_TARGET ?=
LAB_DURATION ?= 2h
LAB_COMMAND ?= kubectl get nodes
# The whole of LAB_COMMAND, `&&`, `;` and `|` included, runs under the Lease,
# as one `sh -c`. Every LAB_ value is passed as written ($(value ...),
# single-quoted), so make expands no `$` in it: write `$KUBECONFIG`, since a
# doubled `$$KUBECONFIG` reaches the shell as is, which reads it as its own
# pid followed by `KUBECONFIG`.
lab-quote = '$(subst ','\'',$(value $(1)))'

.PHONY: test-lab
test-lab: $(LOCALBIN) ## Run LAB_COMMAND against the lab LAB_TARGET through LAB_CONTEXT, holding the lab
	go build -o "$(LOCALBIN)/lab" ./test/lab/cmd/lab
	"$(LOCALBIN)/lab" -context $(call lab-quote,LAB_CONTEXT) -target $(call lab-quote,LAB_TARGET) \
		-duration $(call lab-quote,LAB_DURATION) -- sh -c $(call lab-quote,LAB_COMMAND)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

.PHONY: security
security: govulncheck ## Run govulncheck; fails on a reachable vulnerability.
	"$(GOVULNCHECK)" ./...

##@ Build

.PHONY: build
build: vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: vet ## Run a controller from your host.
	go run ./cmd/main.go --zap-devel

.PHONY: container-tool
container-tool: ## Check that Docker or Podman is available for the container targets.
	@[ -n "$(CONTAINER_TOOL)" ] && command -v "$(CONTAINER_TOOL)" >/dev/null 2>&1 || { \
		echo "No container tool found: install Docker or Podman, or set CONTAINER_TOOL."; \
		exit 1; \
	}

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# Override BASE_IMAGE to build from another registry, e.g.
# make docker-build IMG=<img> BASE_IMAGE=docker.io/library/golang:1.26
.PHONY: docker-build
docker-build: container-tool ## Build the manager image.
	$(CONTAINER_TOOL) build $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) -t ${IMG} .

.PHONY: docker-push
docker-push: container-tool ## Push the manager image.
	$(CONTAINER_TOOL) push ${IMG}

# A registry-qualified name: Docker and Podman store an unqualified one under
# different registries, and a Pod would resolve it as docker.io/...
IM_IMG ?= ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager:dev
.PHONY: docker-build-instance-manager
docker-build-instance-manager: container-tool ## Build the derivative CUBRID + Instance Manager image (ADR-0003).
	$(CONTAINER_TOOL) build -f build/instance-manager/Dockerfile -t ${IM_IMG} .

# A test-only image: the Instance Manager API over a scripted CLI, no CUBRID.
FAKE_IM_IMG ?= example.com/cubrid-fake-instance-manager:e2e
.PHONY: docker-build-fake-im
docker-build-fake-im: container-tool ## Build the test-only fake Instance Manager image (no CUBRID).
	$(CONTAINER_TOOL) build -f test/fakeim/Dockerfile -t ${FAKE_IM_IMG} .

# A test-only image: the SQL workload client on the official CUBRID JDBC driver.
WORKLOAD_IMG ?= example.com/cubrid-workload-client:e2e
.PHONY: docker-build-workload
docker-build-workload: container-tool ## Build the test-only SQL workload client image.
	$(CONTAINER_TOOL) build -f test/workload/client/Dockerfile -t ${WORKLOAD_IMG} .

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: container-tool ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name cubrid-kubernetes-operator-builder
	# The builder and Dockerfile.cross are removed whatever "buildx use" or the
	# build did; the target then exits with that failure's status (#461).
	status=0; \
	$(CONTAINER_TOOL) buildx use cubrid-kubernetes-operator-builder && \
	$(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) --tag ${IMG} -f Dockerfile.cross . || status=$$?; \
	$(CONTAINER_TOOL) buildx rm cubrid-kubernetes-operator-builder || echo "Could not remove the buildx builder cubrid-kubernetes-operator-builder" >&2; \
	rm -f Dockerfile.cross; \
	exit $$status

# Render config/default with the manager image set to ${IMG} through a throwaway
# overlay under the ignored dist/ directory, so no tracked file is edited.
define kustomize-build-with-image
overlay="$$(mktemp -d dist/overlay.XXXXXX)" && trap 'rm -rf "$$overlay"' EXIT && \
	printf 'resources:\n- ../../config/default\n' > "$$overlay/kustomization.yaml" && \
	(cd "$$overlay" && "$(KUSTOMIZE)" edit set image controller=${IMG}) && \
	"$(KUSTOMIZE)" build "$$overlay"
endef

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	$(call kustomize-build-with-image) > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	mkdir -p dist
	$(call kustomize-build-with-image) | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= $(LOCALBIN)/kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
GOVULNCHECK ?= $(LOCALBIN)/govulncheck

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.22.0

#ENVTEST_VERSION is the controller-runtime version to use for setup-envtest, derived from go.mod
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v")

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.13.1
GOVULNCHECK_VERSION ?= v1.8.0

# Kind and its node image are pinned together: the digest is the one the
# release notes of KIND_VERSION list for that Kubernetes version.
# https://github.com/kubernetes-sigs/kind/releases/tag/v0.33.0
KIND_VERSION ?= v0.33.0
KIND_NODE_IMAGE ?= kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5

.PHONY: kind
kind: $(KIND) ## Download kind locally if necessary.
$(KIND): $(LOCALBIN)
	$(call go-install-tool,$(KIND),sigs.k8s.io/kind,$(KIND_VERSION))

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: govulncheck
govulncheck: $(GOVULNCHECK) ## Download govulncheck locally if necessary.
$(GOVULNCHECK): $(LOCALBIN)
	$(call go-install-tool,$(GOVULNCHECK),golang.org/x/vuln/cmd/govulncheck,$(GOVULNCHECK_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
