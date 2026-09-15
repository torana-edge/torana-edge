// Run from Edge: go run scripts/check-plugin-guides.go ../torana-plugins/plugins
// Uses the running host's schema and approval rules without executing guests.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/torana-edge/torana-edge/internal/plugin"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: check-plugin-guides <plugins directory>")
		os.Exit(2)
	}
	paths, err := filepath.Glob(filepath.Join(os.Args[1], "*", "plugin.json"))
	if err != nil {
		panic(err)
	}
	count := 0
	blocks := regexp.MustCompile("(?s)" + "```" + "json\n(.*?)\n" + "```")
	read := func(path string) []byte {
		body, err := os.ReadFile(path)
		if err != nil {
			panic(err)
		}
		return body
	}
	for _, path := range paths {
		dir := filepath.Dir(path)
		if filepath.Base(dir) == "auth" {
			continue
		}
		var manifest plugin.PluginManifest
		if err := json.Unmarshal(read(path), &manifest); err != nil {
			panic(err)
		}
		examples := blocks.FindAllSubmatch(read(filepath.Join(dir, "README.md")), -1)
		if len(examples) < 2 {
			panic("missing settings/approval examples: " + dir)
		}
		schema := &plugin.ConfigSchema{Raw: read(filepath.Join(dir, "schema.json"))}
		if err := plugin.ValidateConfigAgainstSchema(schema, examples[0][1]); err != nil {
			panic(manifest.Name + ": " + err.Error())
		}
		var approval plugin.Approval
		decoder := json.NewDecoder(bytes.NewReader(examples[1][1]))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&approval); err != nil {
			panic(err)
		}
		bundle := plugin.PluginBundle{Manifest: manifest, Digest: approval.Digest}
		if err := plugin.ValidateBundleApproval(bundle, approval); err != nil {
			panic(manifest.Name + ": " + err.Error())
		}
		count++
		fmt.Println("Validated settings and approval: " + manifest.Name)
	}
	if count == 0 {
		panic("no plugin guides found")
	}
	fmt.Printf("Validated %d guide examples against the host contract.\n", count)
}
