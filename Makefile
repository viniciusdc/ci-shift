BINARY_NAME = cifusion

.PHONY: all build clean fmt lint test run

all: build

build:
	go build -o $(BINARY_NAME) .

run:
	./$(BINARY_NAME)

clean:
	rm -f $(BINARY_NAME)

fmt:
	go fmt ./...

lint:
	golangci-lint run

test:
	go test -v ./...

install-tools:
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

ci: fmt lint test build
