VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO_BUILD_FLAGS := -trimpath -ldflags "-s -w -X main.version=$(VERSION)"

# These targets expect the toolchain of dev/Dockerfile. From the host, run
# them through the development container: bin/dev make <target>.

.PHONY: all build web test test-integration lint clean

all: build

# Build the Ember app into web/dist, which the Go binary embeds.
web:
	cd web && pnpm install --frozen-lockfile && pnpm build
	touch web/dist/.keep

# Build bin/paasd for the target host (linux/amd64) with the frontend embedded.
build: web
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GO_BUILD_FLAGS) -o bin/paasd ./cmd/paasd

# Unit tests. Docker is not needed.
test:
	go vet ./...
	go test ./...
	cd web && pnpm lint && pnpm test

# Tests that drive a real Docker daemon.
test-integration:
	go test -tags integration ./...

lint:
	test -z "$$(gofmt -l .)"
	go vet ./...

clean:
	rm -f bin/paasd
