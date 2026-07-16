.PHONY: build install clean test release plugins testdata proto lint

BINARY := torana
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)

# WASM plugins are build artifacts — never committed (*.wasm is gitignored).
# Every plugin dir builds with the same recipe as `torana-cli plugin build`.
PLUGIN_DIRS := plugins/schema_translator plugins/keyword_compactor plugins/compactor plugins/otel plugins/auth plugins/pii
TESTDATA_DIRS := examples/plugins/test-stream-mutator examples/plugins/test-blocker examples/plugins/test-blocker-nogrant examples/plugins/test-observer examples/plugins/test-responder examples/plugins/test-responder-nogrant examples/plugins/test-original
WASM_BUILD = GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared

build: plugins
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/torana/

plugins:
	@for dir in $(PLUGIN_DIRS); do \
		echo "building $$dir/plugin.wasm"; \
		(cd $$dir && $(WASM_BUILD) -o plugin.wasm .) || exit 1; \
	done

testdata:
	@for dir in $(TESTDATA_DIRS); do \
		echo "building $$dir/plugin.wasm"; \
		(cd $$dir && $(WASM_BUILD) -o plugin.wasm .) || exit 1; \
	done
	@echo "building testdata/hello.wasm"
	@cd testdata && $(WASM_BUILD) -o hello.wasm .

# Regenerate pkg/pb/torana.pb.go. Requires protoc and:
#   go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
proto:
	protoc --go_out=paths=source_relative:. pkg/pb/torana.proto

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/torana/

test: plugins testdata
	go test ./... -race -timeout 600s

lint:
	golangci-lint run

clean:
	rm -f $(BINARY)
	rm -f $(foreach d,$(PLUGIN_DIRS) $(TESTDATA_DIRS),$(d)/plugin.wasm) testdata/hello.wasm

release:
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/torana-linux-amd64   ./cmd/torana/
	GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/torana-linux-arm64   ./cmd/torana/
	GOOS=darwin  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/torana-darwin-amd64  ./cmd/torana/
	GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/torana-darwin-arm64  ./cmd/torana/
