GO       ?= go
DIST_DIR := dist

CMDS := $(notdir $(wildcard cmd/*))
BINS := $(addprefix $(DIST_DIR)/,$(CMDS))

GO_FILES := $(shell find . -type f -name '*.go' -not -path './$(DIST_DIR)/*')

.PHONY: all build clean test vet tidy

all: vet test build

build: $(BINS)

$(DIST_DIR)/%: $(GO_FILES) go.mod go.sum
	@mkdir -p $(DIST_DIR)
	$(GO) build -o $@ ./cmd/$*

clean:
	rm -rf $(DIST_DIR)

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy
