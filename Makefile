BINARY_NAME = cishift
INSTALL_DIR = $(HOME)/.local/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X github.com/viniciusdc/ci-shift/internal/buildinfo.version=$(VERSION)

.PHONY: all build clean fmt lint test golden install uninstall install-tools schemas ci \
	docs banner diagrams examples examples-check generate

all: build

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY_NAME) ./cmd/cishift

clean:
	rm -f $(BINARY_NAME)

fmt:
	gofmt -w .

lint:
	golangci-lint run ./...

test:
	go test ./...

# Regenerate golden conversion outputs after an intentional change.
golden:
	go test ./convert/ -run TestGolden -update

install: build
	mkdir -p $(INSTALL_DIR)
	cp $(BINARY_NAME) $(INSTALL_DIR)/
	@echo "Installed $(BINARY_NAME) to $(INSTALL_DIR). Ensure $(INSTALL_DIR) is in your PATH."

uninstall:
	rm -f $(INSTALL_DIR)/$(BINARY_NAME)

install-tools:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	go install github.com/charmbracelet/freeze@latest

# Refresh the embedded schema snapshots from their upstream sources.
schemas:
	go run ./internal/tools/updateschemas

ci: lint test build

# ── Documentation ────────────────────────────────────────────────────────────
# Generators in internal/tools/gen*, configured by docsmith.toml.
# freeze (make install-tools) renders the examples; it is usually in GOBIN.

GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)

# CLI reference in docs/CLI.md and the project layout in README.md.
docs:
	go run ./internal/tools/gendocs

# docs/assets/banner-{dark,light}.svg
banner:
	go run ./internal/tools/genbanner

# docs/assets/diagrams/*.svg from docs/diagrams/*.toml
diagrams:
	go run ./internal/tools/gendiagram

# Run testdata/examples/*.txtar, then render them into the docs.
examples:
	go test -count=1 -run TestScript ./cmd/cishift
	go run ./internal/tools/genexamples

# Fail when the rendered examples are out of date (used by CI).
examples-check:
	go test -count=1 -run TestScript ./cmd/cishift
	go run ./internal/tools/genexamples
	git diff --exit-code -- '*.md'

# Regenerate every generated file.
generate: banner diagrams docs examples
