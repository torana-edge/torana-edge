.PHONY: build install clean test test-race release official-plugins testdata lint

BINARY := torana
VERSION ?= $(shell git describe --tags --exact-match 2>/dev/null || git rev-parse --short HEAD 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)

# WASM fixtures are build artifacts — never committed (*.wasm is gitignored).
#
# The official plugins are NOT built here. They live in torana-plugins, which is
# the only tree that has them; this repo tests the host against the fixtures
# below. `make official-plugins` builds them from a sibling checkout when you
# want the plugin-behaviour suite to run locally.
TESTDATA_DIRS := examples/plugins/test-stream-mutator examples/plugins/test-stream-mutator-nogrant examples/plugins/test-stream-fanout examples/plugins/test-stream-fanout-boundaries examples/plugins/test-stream-reindex-nogrant examples/plugins/test-blocker examples/plugins/test-blocker-nogrant examples/plugins/test-observer examples/plugins/test-responder examples/plugins/test-responder-nogrant examples/plugins/test-original examples/plugins/test-router examples/plugins/test-ticker examples/plugins/test-http-server examples/plugins/test-metrics examples/plugins/test-mutator examples/plugins/test-hostcall examples/plugins/test-fragment-buffer examples/plugins/test-inert-a examples/plugins/test-inert-b examples/plugins/test-inert-c examples/plugins/test-trapper examples/plugins/test-block-then-trap examples/plugins/test-invalid-replacement examples/plugins/test-forge-response-fields examples/plugins/test-invented-content examples/plugins/test-forge-host-meta examples/plugins/test-stale-bind examples/plugins/test-verdict-then-invalid examples/plugins/test-records-invocation examples/plugins/test-trapper-response examples/plugins/test-trapper-stream examples/plugins/test-tool-rewriter examples/plugins/test-malformed-result examples/plugins/test-trapper-after-stream examples/plugins/test-slow-after-stream
WASM_BUILD = GOOS=wasip1 GOARCH=wasm go build -trimpath -buildvcs=false -buildmode=c-shared

build:
	go build -buildvcs=false -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/torana/

# Build the official plugins from a sibling torana-plugins checkout, and print
# the directory to hand to the plugin-behaviour suite:
#   TORANA_PLUGIN_BUNDLES_DIR=$(shell pwd)/../torana-plugins/dist go test ./...
official-plugins:
	@test -d ../torana-plugins || { echo "clone torana-plugins alongside this repo first" >&2; exit 1; }
	@for dir in ../torana-plugins/plugins/*/; do \
		../torana-plugins/scripts/build.sh "$$(basename $$dir)" >/dev/null || exit 1; \
	done
	@echo "bundles in ../torana-plugins/dist"

testdata:
	@for dir in $(TESTDATA_DIRS); do \
		echo "building $$dir/plugin.wasm"; \
		(cd $$dir && $(WASM_BUILD) -o plugin.wasm .) || exit 1; \
	done
	@echo "building testdata/hello.wasm"
	@cd testdata && $(WASM_BUILD) -o hello.wasm .

install:
	go install -buildvcs=false -ldflags "$(LDFLAGS)" ./cmd/torana/

# test: the everyday iteration gate — fixtures plus the ordinary full suite
# (a few minutes). test-race is the slow pre-merge gate: the same suite under
# -race takes ~15 minutes (the proxy package alone is ~13), so it is a
# deliberate, separate step rather than the default command.
test: testdata
	go test ./... -timeout 600s

test-race: testdata
	go test ./... -race -timeout 1800s

lint:
	golangci-lint run

clean:
	rm -f $(BINARY)
	rm -f $(foreach d,$(TESTDATA_DIRS),$(d)/plugin.wasm) testdata/hello.wasm

release:
	GOOS=linux   GOARCH=amd64 go build -buildvcs=false -ldflags "$(LDFLAGS)" -o dist/torana-linux-amd64   ./cmd/torana/
	GOOS=linux   GOARCH=arm64 go build -buildvcs=false -ldflags "$(LDFLAGS)" -o dist/torana-linux-arm64   ./cmd/torana/
	GOOS=darwin  GOARCH=amd64 go build -buildvcs=false -ldflags "$(LDFLAGS)" -o dist/torana-darwin-amd64  ./cmd/torana/
	GOOS=darwin  GOARCH=arm64 go build -buildvcs=false -ldflags "$(LDFLAGS)" -o dist/torana-darwin-arm64  ./cmd/torana/
