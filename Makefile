# Talos Discovery — Makefile

# The binary to build.
APP_NAME = Talos Discovery
BIN := talos-discovery

# Where to push the docker image.
REGISTRY ?= ghcr.io/masudur-rahman

# Git metadata for versioning. Every call tolerates a tree with no commits yet.
git_branch       := $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null)
git_tag          := $(shell git describe --exact-match --abbrev=0 2>/dev/null || echo "")
commit_hash      := $(shell git rev-parse --verify HEAD 2>/dev/null || echo "none")
commit_timestamp := $(shell git show -s --format=%cd --date=format:'%Y-%m-%dT%H:%M:%S' HEAD 2>/dev/null || echo "unknown")

VERSION          := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
version_strategy := commit_hash
ifdef git_tag
	VERSION := $(git_tag)
	version_strategy := tag
endif

DOCKER_IMAGE := $(REGISTRY)/$(BIN)
GO_VERSION   ?= 1.26

# Passed through to buildx by CI; empty for a local build.
DOCKER_CACHE_ARGS ?=

# The config file the binary reads. Empty uses .configs/.talos-discovery.yaml,
# and environment variables override whatever it contains.
CONFIG ?=
CONFIG_ARG := $(if $(CONFIG),--config $(CONFIG),)

LDFLAGS := -s -w -X main.version=$(VERSION)

# ── Build ─────────────────────────────────────────────────────────────────────

all: # @HELP builds the binary
all: build

.PHONY: build
build: # @HELP compiles the binary
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BIN) .

.PHONY: run
run: # @HELP builds and runs the server locally, plaintext on :3000
run: build
	./bin/$(BIN) $(CONFIG_ARG)

# ── Format ───────────────────────────────────────────────────────────────────

.PHONY: fmt
fmt: # @HELP formats Go source files and regroups imports
	@which goimports-reviser >/dev/null 2>&1 || go install github.com/incu6us/goimports-reviser/v3@latest
	goimports-reviser -recursive -company-prefixes=github.com/masudur-rahman -imports-order=std,project,company,general,blanked -format -excludes vendor ./...

# ── Test & Quality ────────────────────────────────────────────────────────────

.PHONY: test
test: # @HELP runs the unit tests and the end-to-end test against the real Talos client
	go test -race -count=1 ./...

.PHONY: test-e2e
test-e2e: # @HELP runs only the end-to-end test
	go test -race -count=1 -v ./e2e/

.PHONY: coverage-html
coverage-html: # @HELP opens the coverage report in a browser
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

.PHONY: vet
vet: # @HELP runs go vet
	go vet ./...

.PHONY: lint
lint: # @HELP runs golangci-lint
	golangci-lint run ./... --timeout=5m

.PHONY: vulncheck
vulncheck: # @HELP runs govulncheck for known vulnerabilities
	@which govulncheck >/dev/null 2>&1 || go install golang.org/x/vuln/cmd/govulncheck@latest
	govulncheck ./...

check: # @HELP runs vet + lint + tests
check: vet lint test

.PHONY: check verify tidy
tidy: # @HELP tidies and verifies Go modules
	go mod tidy
	go mod verify

verify: # @HELP verifies modules are tidy
verify: tidy
	@if ! git diff --exit-code go.mod go.sum; then \
		echo "go.mod/go.sum are out of date; run 'go mod tidy'"; exit 1; \
	fi

# ── Docker ────────────────────────────────────────────────────────────────────

.PHONY: docker-build
docker-build: # @HELP builds a single-arch Docker image
	DOCKER_BUILDKIT=1 docker build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg BUILD_DATE=$(commit_timestamp) \
	  --build-arg GIT_COMMIT=$(commit_hash) \
	  $(DOCKER_CACHE_ARGS) \
	  -t $(DOCKER_IMAGE):$(VERSION) \
	  -t $(DOCKER_IMAGE):latest \
	  -f Dockerfile .

.PHONY: docker-run
docker-run: # @HELP runs the container built from the latest changes, plaintext on :3000
	@if [ -z "$$(docker images -q $(DOCKER_IMAGE):$(VERSION))" ]; then \
		echo "Image not found. Building..."; \
		$(MAKE) docker-build --no-print-directory; \
	fi
	docker run \
	  --rm \
	  --publish 3000:3000 \
	  --publish 3001:3001 \
	  --publish 2122:2122 \
	  $(DOCKER_IMAGE):$(VERSION)

.PHONY: docker-compose-up
docker-compose-up: # @HELP runs the server behind nginx via Docker Compose
	docker compose -f deploy/compose/docker-compose.yml up --build

.PHONY: docker-build-push
docker-build-push: # @HELP builds and pushes a multi-arch image; HEAD must sit on a tag
	@if [ -z "$(git_tag)" ]; then \
		echo "Images are published from tags only, and HEAD is not on one."; \
		echo "  git tag -a v0.1.0 -m v0.1.0"; \
		exit 1; \
	fi
	docker buildx build --platform linux/amd64,linux/arm64 \
	  --provenance=false --sbom=false \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg BUILD_DATE=$(commit_timestamp) \
	  --build-arg GIT_COMMIT=$(commit_hash) \
	  $(DOCKER_CACHE_ARGS) \
	  --output "type=image,push=true" \
	  --tag $(DOCKER_IMAGE):$(VERSION) \
	  --tag $(DOCKER_IMAGE):latest \
	  -f Dockerfile .

.PHONY: release
release: # @HELP builds and pushes the image tagged with the current version
release: docker-build-push

# ── Deploy ────────────────────────────────────────────────────────────────────

.PHONY: deploy
deploy: # @HELP applies the K8s manifests to KUBE_CONTEXT, which must be named explicitly
	@if [ -z "$(KUBE_CONTEXT)" ]; then \
		echo "KUBE_CONTEXT is not set. Name the cluster explicitly:"; \
		echo "  make deploy KUBE_CONTEXT=<context>"; \
		echo "The current context is $$(kubectl config current-context 2>/dev/null), and a bare apply would go there."; \
		exit 1; \
	fi
	kubectl --context "$(KUBE_CONTEXT)" apply -f deploy/k8s/

# ── Misc ──────────────────────────────────────────────────────────────────────

version: # @HELP outputs the version string
version:
	@echo "Application Version Information"
	@echo "==============================="
	@echo ""
	@echo "Application Name:    $(APP_NAME)"
	@echo ""
	@echo "Version Details:"
	@echo "    Version:            $(VERSION)"
	@echo "    Version Strategy:   $(version_strategy)"
	@echo ""
	@echo "Git Information:"
	@echo "    Git Tag:            $(git_tag)"
	@echo "    Git Branch:         $(git_branch)"
	@echo "    Commit Hash:        $(commit_hash)"
	@echo "    Commit Timestamp:   $(commit_timestamp)"
	@echo ""
	@echo "Build Environment:"
	@echo "    Go Version:         $(shell go version | cut -d " " -f 3)"
	@echo "    Platform:           $(shell go env GOOS)/$(shell go env GOARCH)"

.PHONY: clean
clean: # @HELP removes built binaries and temporary files
	rm -rf bin coverage.out data

help: # @HELP prints this message
help:
	@echo "VARIABLES:"
	@echo "  BIN = $(BIN)"
	@echo "  REGISTRY = $(REGISTRY)"
	@echo "  CONFIG = $(CONFIG)"
	@echo
	@echo "TARGETS:"
	@grep -E '^.*: *# *@HELP' $(MAKEFILE_LIST)    \
	    | awk '                                   \
	        BEGIN {FS = ": *# *@HELP"};           \
	        { printf "  %-30s %s\n", $$1, $$2 };  \
	    '
