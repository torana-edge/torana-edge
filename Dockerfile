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
# TORANA_BIND is deliberately NOT set here.
#
# Setting it to 0.0.0.0 makes the published port serve proxy traffic, but the
# control plane still refuses every request: its guard requires a loopback
# SOURCE address, and traffic crossing a Docker bridge arrives from the gateway
# (172.17.0.1 or similar). Measured:
#
#   /health              from a bridge address -> 200
#   /_torana/api/config  from a bridge address -> 403 control plane is localhost-only
#
# So the container would proxy fine and be impossible to administer — no plugin
# approvals, no configuration. That is worse than a port that plainly does not
# answer, because the failure looks like a permissions bug rather than a
# deployment one.
#
# Until the control plane moves to its own listener with real authentication,
# run with `--network host` if you need it, which makes the source address
# loopback again:
#
#   docker run --network host torana-edge
#
# With a published port instead (`-p 8080:8080`), set TORANA_BIND=0.0.0.0
# yourself and accept that only the data plane works.
ENTRYPOINT ["/torana"]
