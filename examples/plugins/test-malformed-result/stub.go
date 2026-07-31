//go:build !wasip1

// Package main is a wasip1-only guest. This stub exists so host-side
// `go build ./...` and `go vet ./...` do not fail with "build constraints
// exclude all Go files" — the real guest uses //go:wasmimport and
// //go:wasmexport, which only compile for wasm.
package main

func main() {}
