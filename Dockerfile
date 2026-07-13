# Multi-arch Dockerfile for Torana Edge
# Build: docker buildx build --platform linux/amd64,linux/arm64 -t torana-edge .
FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o torana ./cmd/torana/

FROM gcr.io/distroless/static-debian12
COPY --from=builder /app/torana /torana
COPY --from=builder /app/plugins /plugins
EXPOSE 8080
ENTRYPOINT ["/torana"]
