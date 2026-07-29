# Multi-arch Dockerfile for Torana Edge
# Build: docker buildx build --platform linux/amd64,linux/arm64 -t torana-edge .
FROM golang:1.26.4-alpine AS builder
RUN apk add --no-cache make
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o torana ./cmd/torana/

FROM gcr.io/distroless/static-debian12
COPY --from=builder /app/torana /torana
# No plugins are baked in. They live in torana-plugins and are installed by the
# operator on a machine with Go and git. Mount the resulting bundle directory
# read-only at /plugins and point plugins.dir at it; the minimal runtime image
# intentionally contains neither a compiler nor git.
EXPOSE 8080
ENTRYPOINT ["/torana"]
