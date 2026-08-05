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

INFRA := ./scripts/infrastructure.sh
START := ./scripts/start-services.sh
STOP  := ./scripts/stop-services.sh

.PHONY: all build clean test vet tidy infrastructure-up infrastructure-down infrastructure-clean observability-up observability-down start stop restart s2s-keygen FORCE

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

$(DIST_DIR)/%: $(GO_FILES) go.mod go.sum $(BUILD_MODE_FILE)
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
