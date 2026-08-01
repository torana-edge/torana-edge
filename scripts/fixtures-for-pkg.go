// Command fixtures-for-pkg prints the bare fixture names (e.g. test-observer)
// that one package's Go files reference.
//
// The mapping is AST-based: every string literal in the package's Go files is
// collected and mapped to known fixture names (the Makefile TESTDATA_DIRS
// set). This catches dynamically constructed references such as
// []string{"test-blocker"} and fixturesDir+"/test-fragment-buffer/plugin.wasm"
// that a path-pattern grep cannot see. Over-inclusion is safe — building an
// unneeded fixture costs one ~0.4s no-op — under-inclusion is not: a strict
// (TORANA_E2E=1) package run fails on any missing fixture.
//
// internal/wasm is special-cased: its all-fixture ABI inventory legitimately
// requires every v2 fixture.
//
// Usage: go run ./scripts/fixtures-for-pkg.go <internal/<pkg>>
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: fixtures-for-pkg <internal/<pkg>>")
		os.Exit(2)
	}
	pkg := strings.TrimPrefix(os.Args[1], "./")

	names := fixtureNames()
	if pkg == "internal/wasm" {
		for _, n := range names {
			fmt.Println(n)
		}
		return
	}

	referenced := map[string]bool{}
	err := filepath.WalkDir(pkg, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			bl, ok := n.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				return true
			}
			lit, err := strconv.Unquote(bl.Value)
			if err != nil {
				return true
			}
			for _, name := range names {
				// Path-segment-ish containment: the name as a whole literal,
				// as a path segment ("/test-observer"), or as a prefix of a
				// path ("test-observer/plugin.wasm").
				if lit == name || strings.Contains(lit, "/"+name) || strings.Contains(lit, name+"/") {
					referenced[name] = true
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var out []string
	for n := range referenced {
		out = append(out, n)
	}
	sort.Strings(out)
	for _, n := range out {
		fmt.Println(n)
	}
}

// fixtureNames reads the Makefile TESTDATA_DIRS list and returns the bare
// fixture names, sorted.
func fixtureNames() []string {
	b, err := os.ReadFile("Makefile")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var names []string
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "TESTDATA_DIRS := ") {
			continue
		}
		for _, dir := range strings.Fields(strings.TrimPrefix(line, "TESTDATA_DIRS := ")) {
			if strings.HasPrefix(dir, "examples/plugins/") {
				names = append(names, filepath.Base(dir))
			}
		}
	}
	sort.Strings(names)
	return names
}
