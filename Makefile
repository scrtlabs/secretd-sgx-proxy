BINARY      := secretd-billing
GO          := go
PROTOC      := protoc
PROTO_DIR   := proto/billing
PROTO_SRC   := billing.proto

.PHONY: all build proto test clean install

all: proto build

## Build the binary
build:
	$(GO) build -o $(BINARY) .

## Generate protobuf Go code from billing.proto
proto:
	$(PROTOC) \
		--go_out=$(PROTO_DIR) --go_opt=paths=source_relative \
		--go-grpc_out=$(PROTO_DIR) --go-grpc_opt=paths=source_relative \
		$(PROTO_SRC)

## Run all tests
test:
	$(GO) test -v ./...

## Build and run tests
check: build test

## Remove build artifacts
clean:
	rm -f $(BINARY)

## Install protoc Go plugins (one-time setup)
install-tools:
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	$(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
