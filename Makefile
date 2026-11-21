VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO_BUILD_FLAGS := -trimpath -ldflags "-s -w -X main.version=$(VERSION)"

# These targets expect the toolchain of dev/Dockerfile. From the host, run
# them through the development container: bin/dev make <target>.

.PHONY: all build web test test-integration lint clean

all: build

# What the frontend build reads. When none of it changed since the last
# build, `make build` reuses web/dist instead of running Vite again, which
# takes most of the build's time.
WEB_SOURCES := $(shell find web/app web/public web/config -type f 2>/dev/null) \
	web/index.html web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml \
	web/vite.config.mjs web/babel.config.mjs web/ember-cli-build.mjs
# The install is current while pnpm's own record is newer than the lockfile;
# deleting node_modules removes it, too.
WEB_INSTALLED := web/node_modules/.modules.yaml
# The stamp of the last build lives in web/tmp (ignored by git). It is not
# enough on its own: `pnpm test` writes a development build into the same
# web/dist, so the build runs again whenever dist/index.html is newer than
# the stamp, that is, when anything else wrote web/dist since.
WEB_BUILT := web/tmp/make-built

# Build the Ember app into web/dist, which the Go binary embeds.
web: $(WEB_BUILT)

$(WEB_INSTALLED): web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml
	cd web && pnpm install --frozen-lockfile
	touch $@

$(WEB_BUILT): $(WEB_INSTALLED) $(WEB_SOURCES) web/dist/index.html
	cd web && pnpm build
	touch web/dist/.keep
	mkdir -p $(@D) && touch $@

# Missing before the first build; the rule above then builds.
web/dist/index.html: ;

# Build bin/koroku for the target host (linux/amd64) with the frontend embedded.
build: web
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GO_BUILD_FLAGS) -o bin/koroku ./cmd/koroku

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
	rm -f bin/koroku $(WEB_BUILT)
