BINARY_NAME = cishift
INSTALL_DIR = $(HOME)/.local/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X github.com/viniciusdc/ci-shift/internal/buildinfo.version=$(VERSION)

.PHONY: all build clean fmt lint test golden install uninstall install-tools schemas ci

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

# Refresh the embedded schema snapshots from their upstream sources.
schemas:
	go run ./internal/tools/updateschemas

ci: lint test build
