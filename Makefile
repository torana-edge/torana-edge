.PHONY: build install clean test release

BINARY := torana
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/torana/

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/torana/

test:
	go test ./...

clean:
	rm -f $(BINARY)

release:
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/torana-linux-amd64   ./cmd/torana/
	GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/torana-linux-arm64   ./cmd/torana/
	GOOS=darwin  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/torana-darwin-amd64  ./cmd/torana/
	GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/torana-darwin-arm64  ./cmd/torana/
