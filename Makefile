BINARY := continuum-plugin-audiobookbay-requests
GO ?= go

.PHONY: build test clean
build:
	$(GO) build -o $(BINARY) ./cmd/continuum-plugin-audiobookbay-requests
test:
	$(GO) test ./...
clean:
	rm -f $(BINARY)
