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
# operator with `torana plugin install`, which fetches the source, builds it
# locally and prints the digest — so nobody runs a binary they could not have
# read. Mount or install into /plugins and point plugins.dir at it.
EXPOSE 8080
ENTRYPOINT ["/torana"]
