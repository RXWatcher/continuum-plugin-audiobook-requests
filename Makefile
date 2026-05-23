BINARY := silo-plugin-audiobook-requests
GO ?= go

.PHONY: build test clean
build:
	$(GO) build -o $(BINARY) ./cmd/silo-plugin-audiobook-requests
test:
	$(GO) test ./...
clean:
	rm -f $(BINARY)
