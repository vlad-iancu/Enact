GO       ?= go
DIST_DIR := dist

# DEBUG=1 builds the binaries without optimizations/inlining and makes
# `make start` / `make restart` launch each service under a headless Delve
# debugger exposing a per-service port to attach an IDE to (see
# scripts/start-services.sh for the port assignments).
DEBUG ?= 0
export DEBUG

ifeq ($(DEBUG),1)
GCFLAGS := -gcflags=all="-N -l"
else
GCFLAGS :=
endif

CMDS := $(notdir $(wildcard cmd/*))
BINS := $(addprefix $(DIST_DIR)/,$(CMDS))

GO_FILES := $(shell find . -type f -name '*.go' -not -path './$(DIST_DIR)/*')

# Files embedded with go:embed are inputs to the build as much as source is,
# but they are not *.go and so are invisible to the rule below. Without this,
# editing a documentation page rebuilds nothing and the running service keeps
# serving the previous text — a confusing failure, because the file on disk is
# plainly correct.
EMBED_FILES := $(shell find docs/app -type f 2>/dev/null)

INFRA := ./scripts/infrastructure.sh
START := ./scripts/start-services.sh
STOP  := ./scripts/stop-services.sh

# WordNet 3.0, used by the crawl orchestrator. See the `wordnet` target.
WORDNET_URL := https://wordnetcode.princeton.edu/3.0/WordNet-3.0.tar.gz
WORDNET_DIR := $(DIST_DIR)/wordnet/WordNet-3.0/dict

# Named-entity recognition for crawls: an int8-quantised BERT token classifier
# plus the ONNX Runtime it needs. Both are optional — NER_ENABLED defaults to
# false and the orchestrator runs without them — and far too large to keep in
# the repository, so `make ner-model` fetches them the way `make wordnet` does.
NER_DIR      := $(DIST_DIR)/models/bert-base-NER
ORT_DIR      := $(DIST_DIR)/onnxruntime
ORT_VERSION  := 1.29.0
NER_REPO     := https://huggingface.co/Xenova/bert-base-NER/resolve/main
ORT_URL      := https://github.com/microsoft/onnxruntime/releases/download/v$(ORT_VERSION)
# The runtime is published per platform; pick by what we are building on.
UNAME_S := $(shell uname -s)
UNAME_M := $(shell uname -m)
ifeq ($(UNAME_S),Darwin)
ORT_PKG := onnxruntime-osx-arm64-$(ORT_VERSION)
ORT_LIB := libonnxruntime.dylib
else
ORT_PKG := onnxruntime-linux-x64-$(ORT_VERSION)
ORT_LIB := libonnxruntime.so
endif

.PHONY: all build clean test vet tidy wordnet ner-model infrastructure-up infrastructure-down infrastructure-clean observability-up observability-down start stop restart s2s-keygen wsd-diag docker-build docker-build-local docker-push docker-deploy FORCE

LGTM := docker compose -f deploy/docker-compose.lgtm.yml

all: vet test build

build: $(BINS)

# Stamp file recording the build mode (debug vs release). Its recipe runs on
# every make invocation but only rewrites the file when the mode changed, so
# flipping DEBUG on or off forces the binaries to rebuild with the right
# flags while repeated builds in the same mode stay cached.
BUILD_MODE_FILE := $(DIST_DIR)/.buildmode

$(BUILD_MODE_FILE): FORCE
	@mkdir -p $(DIST_DIR)
	@[ "$$(cat $@ 2>/dev/null)" = "debug=$(DEBUG)" ] || echo "debug=$(DEBUG)" > $@

FORCE:

$(DIST_DIR)/%: $(GO_FILES) $(EMBED_FILES) go.mod go.sum $(BUILD_MODE_FILE)
	@mkdir -p $(DIST_DIR)
	$(GO) build $(GCFLAGS) -o $@ ./cmd/$*

clean:
	rm -f $(BINS) $(BUILD_MODE_FILE)

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

# Download the WordNet 3.0 database, which the crawl orchestrator needs for
# the taxonomy behind Wu-Palmer similarity and for Morphy lemmatisation.
#
# Not vendored: it is 36 MB of third-party data that changes never, so it is
# fetched once into dist/ (already gitignored) rather than committed or
# embedded in every binary. WORDNET_DIR points the service at it.
#
# The full WordNet-3.0 tarball is used rather than the smaller WNdb-3.0,
# because only the former carries the *.exc exception lists that irregular
# forms (geese -> goose, ran -> run) are looked up in.
wordnet: $(WORDNET_DIR)/data.noun

$(WORDNET_DIR)/data.noun:
	@echo "==> Downloading WordNet 3.0 to $(WORDNET_DIR)"
	@mkdir -p $(DIST_DIR)/wordnet
	@curl -fsSL -o $(DIST_DIR)/wordnet/WordNet-3.0.tar.gz $(WORDNET_URL)
	@tar xzf $(DIST_DIR)/wordnet/WordNet-3.0.tar.gz -C $(DIST_DIR)/wordnet
	@rm -f $(DIST_DIR)/wordnet/WordNet-3.0.tar.gz
	@echo "==> WordNet ready at $(WORDNET_DIR)"

# Start infra containers and create OpenSearch indices (from the mappings/
# index templates) and the Redis stream/consumer group if they don't exist.
infrastructure-up:
	$(INFRA) up

# Stop the infra containers (data is preserved).
infrastructure-down:
	$(INFRA) down

# Delete and recreate the OpenSearch indices and the Redis stream.
infrastructure-clean:
	$(INFRA) clean

# Start the local LGTM observability stack (Loki, Grafana, Tempo, Mimir).
# Grafana is then at http://localhost:3000 (anonymous admin). The OTEL_*
# defaults in .env already point the services at it.
observability-up:
	$(LGTM) up -d

# Stop the observability stack (telemetry data in named volumes is preserved).
observability-down:
	$(LGTM) down

# Start the built service binaries in dist/ in the background. With DEBUG=1
# each service runs under a headless Delve listener for IDE attachment.
start:
	$(START)

# Stop any locally-running enact services (SIGTERM by service name).
stop:
	$(STOP)

# Stop running services, rebuild the binaries, then start them again.
# `make restart DEBUG=1` rebuilds debug binaries and starts them under Delve.
restart: stop build start

# Generate the local S2S key material: one Ed25519 keypair per service under
# s2s/keys/ (gitignored) and the public JWKS at s2s/jwks.yaml. Idempotent.
s2s-keygen:
	go run ./scripts/s2s-keygen

# Explain how a crawl query is disambiguated, and whether a wrong sense is the
# ant colony's fault or the objective's. Needs WordNet; `make wordnet` fetches
# it. Pass flags through ARGS, and see scripts/wsd-diag for what they do.
#
#   make wsd-diag ARGS='-query "opensearch indices and security" -seed https://dev.to/t/opensearch'
ner-model: $(NER_DIR)/model_int8.onnx $(ORT_DIR)/$(ORT_LIB)

$(NER_DIR)/model_int8.onnx:
	@mkdir -p $(NER_DIR)
	@echo "Fetching the NER model (~109 MB)..."
	curl -fsSL -o $(NER_DIR)/model_int8.onnx "$(NER_REPO)/onnx/model_int8.onnx"
	curl -fsSL -o $(NER_DIR)/vocab.txt       "$(NER_REPO)/vocab.txt"
	curl -fsSL -o $(NER_DIR)/config.json     "$(NER_REPO)/config.json"

$(ORT_DIR)/$(ORT_LIB):
	@mkdir -p $(ORT_DIR)
	@echo "Fetching ONNX Runtime $(ORT_VERSION) ($(ORT_PKG))..."
	curl -fsSL -o $(DIST_DIR)/ort.tgz "$(ORT_URL)/$(ORT_PKG).tgz"
	tar xzf $(DIST_DIR)/ort.tgz -C $(DIST_DIR)
	cp $(DIST_DIR)/$(ORT_PKG)/lib/libonnxruntime*.dylib $(ORT_DIR)/ 2>/dev/null || \
	  cp $(DIST_DIR)/$(ORT_PKG)/lib/libonnxruntime.so* $(ORT_DIR)/
	rm -rf $(DIST_DIR)/ort.tgz $(DIST_DIR)/$(ORT_PKG)

wsd-diag: $(WORDNET_DIR)/data.noun
	WORDNET_DIR=$(WORDNET_DIR) go run ./scripts/wsd-diag $(ARGS)

# --- Docker images ----------------------------------------------------------
# One image per service from the single root Dockerfile (--build-arg SERVICE).
# REGISTRY is the image prefix (e.g. ghcr.io/<owner>); TAG defaults to the
# short git SHA. Deployment images target linux/amd64 (cloud VMs) — the Go
# toolchain cross-compiles natively, no emulation.
REGISTRY ?=
TAG      ?= $(shell git rev-parse --short HEAD)

# Fails fast when REGISTRY is unset instead of producing images named "/x".
check-registry:
	@[ -n "$(REGISTRY)" ] || { echo "ERROR: set REGISTRY, e.g. make docker-build REGISTRY=ghcr.io/<owner>"; exit 1; }
.PHONY: check-registry

# Build linux/amd64 images for every service, tagged :$(TAG) and :latest.
docker-build: check-registry
	for svc in $(CMDS); do \
		docker buildx build --platform linux/amd64 --build-arg SERVICE=$$svc \
			-t $(REGISTRY)/$$svc:$(TAG) -t $(REGISTRY)/$$svc:latest --load . || exit 1; \
	done

# Native-architecture images loaded into the local docker engine, tagged
# :dev — for smoke-testing the compose stack on this machine.
docker-build-local: check-registry
	for svc in $(CMDS); do \
		docker buildx build --build-arg SERVICE=$$svc \
			-t $(REGISTRY)/$$svc:dev --load . || exit 1; \
	done

# Build and push linux/amd64 images (buildx builds and pushes in one step).
docker-push: check-registry
	for svc in $(CMDS); do \
		docker buildx build --platform linux/amd64 --build-arg SERVICE=$$svc \
			-t $(REGISTRY)/$$svc:$(TAG) -t $(REGISTRY)/$$svc:latest --push . || exit 1; \
	done

# Sync the compose file and S2S material to the VM and roll the stack.
# Env files (app.env, enact-main.env, .env) are deliberately NOT synced —
# they are created once on the VM from the deploy/*.example files. The s2s
# mount must be readable by the distroless nonroot uid (65532), so the
# remote step re-applies ownership after every sync (needs passwordless
# sudo; otherwise run the chown on the VM manually).
# Usage: make docker-deploy VM=user@host [VM_DIR=/opt/enact]
VM     ?=
VM_DIR ?= /opt/enact
docker-deploy:
	@[ -n "$(VM)" ] || { echo "ERROR: set VM, e.g. make docker-deploy VM=user@host"; exit 1; }
	rsync -av deploy/docker-compose.app.yml $(VM):$(VM_DIR)/
	# The Caddyfile (no secrets, unlike the env files) syncs when present
	# locally — deploy/Caddyfile is the source of truth, VM edits get
	# overwritten. Its presence on the VM is also what enables the web
	# profile below.
	@[ ! -f deploy/Caddyfile ] || rsync -av deploy/Caddyfile $(VM):$(VM_DIR)/
	# Reclaim directories other uids may own before rsync writes to them:
	# s2s belongs to the container uid (65532) after a deploy, and docker
	# creates the www bind-mount source as root if caddy starts before the
	# first upload. s2s is re-chowned by the post-step; www stays with the
	# ssh user (caddy only reads it).
	ssh $(VM) "cd $(VM_DIR) && for d in s2s www; do [ ! -d \$$d ] || sudo -n chown -R \$$(id -un):\$$(id -gn) \$$d; done"
	# The frontend build staged in deploy/www (copy the SPA's dist/ there)
	# mirrors to the VM web root — --delete keeps removed assets from
	# accumulating, so the VM copy is exactly the staged build.
	@[ ! -d deploy/www ] || rsync -av --delete deploy/www/ $(VM):$(VM_DIR)/www/
	rsync -av scripts/infrastructure.sh $(VM):$(VM_DIR)/scripts/
	rsync -av mappings $(VM):$(VM_DIR)/
	rsync -av s2s $(VM):$(VM_DIR)/
	ssh $(VM) "cd $(VM_DIR) \
		&& { sudo -n chown -R 65532:65532 s2s && sudo -n chmod -R u=rX,go= s2s || echo 'WARNING: fix s2s ownership manually: sudo chown -R 65532:65532 $(VM_DIR)/s2s'; } \
		&& PROFILES=''; [ -f Caddyfile ] && PROFILES='--profile web'; \
		docker compose -f docker-compose.app.yml \$$PROFILES pull && docker compose -f docker-compose.app.yml \$$PROFILES up -d \
		&& docker image prune -af"
