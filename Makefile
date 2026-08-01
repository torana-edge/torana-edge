.PHONY: build install clean test test-race test-pkg test-race-pkg testdata-pkg release official-plugins testdata lint force-fixtures

BINARY := torana
VERSION ?= $(shell git describe --tags --exact-match 2>/dev/null || git rev-parse --short HEAD 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)

# WASM fixtures are build artifacts — never committed (*.wasm is gitignored).
#
# The official plugins are NOT built here. They live in torana-plugins, which is
# the only tree that has them; this repo tests the host against the fixtures
# below. `make official-plugins` builds them from a sibling checkout when you
# want the plugin-behaviour suite to run locally.
TESTDATA_DIRS := examples/plugins/test-extension-contract examples/plugins/test-stream-mutator examples/plugins/test-stream-mutator-nogrant examples/plugins/test-stream-fanout examples/plugins/test-stream-fanout-boundaries examples/plugins/test-stream-reindex-nogrant examples/plugins/test-stream-complete-block-nogrant examples/plugins/test-stream-complete-block-granted examples/plugins/test-stream-complete-signed-tool examples/plugins/test-stream-delay-stop examples/plugins/test-stream-early-stop examples/plugins/test-blocker examples/plugins/test-blocker-nogrant examples/plugins/test-observer examples/plugins/test-responder examples/plugins/test-responder-nogrant examples/plugins/test-original examples/plugins/test-router examples/plugins/test-ticker examples/plugins/test-http-server examples/plugins/test-metrics examples/plugins/test-mutator examples/plugins/test-hostcall examples/plugins/test-fragment-buffer examples/plugins/test-inert-a examples/plugins/test-inert-b examples/plugins/test-inert-c examples/plugins/test-trapper examples/plugins/test-block-then-trap examples/plugins/test-invalid-replacement examples/plugins/test-forge-response-fields examples/plugins/test-invented-content examples/plugins/test-forge-host-meta examples/plugins/test-stale-bind examples/plugins/test-verdict-then-invalid examples/plugins/test-records-invocation examples/plugins/test-trapper-response examples/plugins/test-trapper-stream examples/plugins/test-tool-rewriter examples/plugins/test-malformed-result examples/plugins/test-trapper-after-stream examples/plugins/test-slow-after-stream
WASM_BUILD = GOWORK=off GOOS=wasip1 GOARCH=wasm go build -trimpath -buildvcs=false -buildmode=c-shared

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

# --- WASM fixture builds (content-addressed) ---
#
# Every fixture is a REAL target with real prerequisites. The build decision
# is made by scripts/testdata.sh from a content fingerprint — source paths and
# contents (add/modify/delete), root go.mod/go.sum (SDK pin), the build
# command, and the Go toolchain version — so an unchanged tree runs ZERO go
# builds, a changed fixture rebuilds only itself, and an SDK-pin /
# build-command / toolchain change rebuilds everything. Directory
# prerequisites catch source addition/deletion (the directory mtime moves);
# mtime alone never decides a rebuild.
STAMP_DIR := .cache/fixtures
FIXTURES := $(addsuffix /plugin.wasm,$(TESTDATA_DIRS))

# Per-fixture rule. $1 is the fixture directory.
#
# The force-fixtures prerequisite makes every recipe run on EVERY invocation:
# Make's timestamps are deliberately NOT part of the go-build decision. A
# toolchain change, a content change with a restored old mtime, or a deleted
# stamp changes no prerequisite, so only the fingerprint script — which always
# runs and no-ops when nothing changed — can decide. This keeps an unchanged
# tree at zero go builds while closing the scheduling hole.
.PHONY: force-fixtures
define fixture_rule
$(1)/plugin.wasm: force-fixtures $(1) $(wildcard $(1)/*.go) go.mod go.sum Makefile
	@scripts/testdata.sh $(1) $(STAMP_DIR)/$(notdir $(1)).stamp $$@ -- $(WASM_BUILD) -o plugin.wasm .
endef
$(foreach d,$(TESTDATA_DIRS),$(eval $(call fixture_rule,$(d))))

# Aggregate: conservative parallel cold builds (4 workers); warm no-op runs
# only the cheap fingerprint checks (no go builds).
testdata:
	$(MAKE) -j4 $(FIXTURES)

install:
	go install -buildvcs=false -ldflags "$(LDFLAGS)" ./cmd/torana/

# --- Test targets ---
#
# Persistent local wazero compilation cache: the same mechanism CI uses
# (TORANA_CI_CACHE), defaulting to an ignored repo-local directory so every
# local run — and every correction round — reuses compiled fixtures across
# processes. An environment-provided TORANA_CI_CACHE overrides the default.
CACHE_DIR := $(CURDIR)/.cache/wazero
TORANA_CI_CACHE ?= $(CACHE_DIR)
export TORANA_CI_CACHE

# test: the everyday iteration gate — fixtures plus the strict ordinary full
# suite. TORANA_E2E=1 makes a missing required fixture an actionable FAILURE
# instead of a silent skip; GOWORK=off matches the verified gate. No
# testing.Short or build-tag omissions: this is the complete ./... suite.
test: testdata
	@mkdir -p "$${TORANA_CI_CACHE:-$(CACHE_DIR)}"
	GOWORK=off TORANA_E2E=1 go test ./... -timeout 600s

# test-race is the slow pre-merge gate: the same complete suite under -race.
test-race: testdata
	@mkdir -p "$${TORANA_CI_CACHE:-$(CACHE_DIR)}"
	GOWORK=off TORANA_E2E=1 go test ./... -race -timeout 1800s

# Package-scoped targets for correction rounds: build only the package's
# required fixture set (see scripts/fixtures-for-pkg.sh), strict everywhere.
# usage: make test-pkg PKG=./internal/proxy   (accepted form: ./internal/...)
define check_pkg
	@test -n "$(PKG)" || { echo "usage: make $(1) PKG=./internal/proxy" >&2; exit 1; }
	@case "$(PKG)" in ./internal/*|internal/*) ;; *) echo "PKG must be ./internal/... (got $(PKG))" >&2; exit 1;; esac
endef

PKG_FIXTURES = $(addprefix examples/plugins/,$(shell scripts/fixtures-for-pkg.sh $(PKG)))

test-pkg:
	$(call check_pkg,test-pkg)
	@mkdir -p "$${TORANA_CI_CACHE:-$(CACHE_DIR)}"
	@$(MAKE) $(addsuffix /plugin.wasm,$(PKG_FIXTURES))
	GOWORK=off TORANA_E2E=1 go test $(PKG) -timeout 600s

test-race-pkg:
	$(call check_pkg,test-race-pkg)
	@mkdir -p "$${TORANA_CI_CACHE:-$(CACHE_DIR)}"
	@$(MAKE) $(addsuffix /plugin.wasm,$(PKG_FIXTURES))
	GOWORK=off TORANA_E2E=1 go test $(PKG) -race -timeout 1800s

# Build only a package's required fixture set (used by CI shards and the
# clean-sandbox proof). PKG accepts the same ./internal/... form.
testdata-pkg:
	$(call check_pkg,testdata-pkg)
	@$(MAKE) $(addsuffix /plugin.wasm,$(PKG_FIXTURES))

lint:
	golangci-lint run

clean:
	rm -f $(BINARY)
	rm -f $(foreach d,$(TESTDATA_DIRS),$(d)/plugin.wasm)
	rm -rf .cache

release:
	GOOS=linux   GOARCH=amd64 go build -buildvcs=false -ldflags "$(LDFLAGS)" -o dist/torana-linux-amd64   ./cmd/torana/
	GOOS=linux   GOARCH=arm64 go build -buildvcs=false -ldflags "$(LDFLAGS)" -o dist/torana-linux-arm64   ./cmd/torana/
	GOOS=darwin  GOARCH=amd64 go build -buildvcs=false -ldflags "$(LDFLAGS)" -o dist/torana-darwin-amd64  ./cmd/torana/
	GOOS=darwin  GOARCH=arm64 go build -buildvcs=false -ldflags "$(LDFLAGS)" -o dist/torana-darwin-arm64  ./cmd/torana/
