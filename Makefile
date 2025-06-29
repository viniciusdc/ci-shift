BINARY_NAME = cifusion
MODULE_DIR = cifusion
INSTALL_DIR = $(HOME)/.local/bin

.PHONY: all build clean fmt lint test run install uninstall install-tools ci

all: build

build:
	cd $(MODULE_DIR) && go build -o $(BINARY_NAME) .

run: build
	./$(MODULE_DIR)/$(BINARY_NAME)

clean:
	cd $(MODULE_DIR) && rm -f $(BINARY_NAME)

fmt:
	cd $(MODULE_DIR) && go fmt ./...

lint:
	cd $(MODULE_DIR) && golangci-lint run

test:
	cd $(MODULE_DIR) && go test -v ./...

install: build
	mkdir -p $(INSTALL_DIR)
	cp $(MODULE_DIR)/$(BINARY_NAME) $(INSTALL_DIR)/
	@echo "Installed $(BINARY_NAME) to $(INSTALL_DIR). Ensure $(INSTALL_DIR) is in your PATH."

uninstall:
	rm -f $(INSTALL_DIR)/$(BINARY_NAME)
	@echo "Removed $(BINARY_NAME) from $(INSTALL_DIR)."

install-tools:
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

ci: fmt lint test build
