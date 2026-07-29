# Multi-arch Dockerfile for Torana Edge
# Build: docker buildx build --platform linux/amd64,linux/arm64 -t torana-edge .
FROM golang:1.26.4-alpine AS builder
RUN apk add --no-cache make
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# VERSION stamps main.version, the same way the Makefile does. Without it every
# container reports "dev", which is not just cosmetic: an untagged build
# deliberately skips the plugin product-version compatibility gates, so a
# released image would silently stop enforcing minimum_torana_version.
#   docker build --build-arg VERSION=v0.3.0 .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o torana ./cmd/torana/

FROM gcr.io/distroless/static-debian12
COPY --from=builder /app/torana /torana
# No plugins are baked in. They live in torana-plugins and are installed by the
# operator on a machine with Go and git. Mount the resulting bundle directory
# read-only at /plugins and point plugins.dir at it; the minimal runtime image
# intentionally contains neither a compiler nor git.
EXPOSE 8080
# Torana binds 127.0.0.1 by default, because the control plane shares this
# listener and must not be reachable off-host. In a container that means a
# published port answers nothing, so bind all interfaces here and let the
# container boundary be the isolation. Override at run time if you disagree.
ENV TORANA_BIND=0.0.0.0
ENTRYPOINT ["/torana"]
