package pbconv

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Review round 3 finding 2: the repository inventory proves the only raw
// first-arm-wins converter is private to the checked implementation. This
// scans every PRODUCTION (non-test) Go file under internal/ and cmd/:
//
//   - `pbconv.ToPBChatRequest` / `pbconv.toPBChatRequest` may not be called
//     anywhere (the unchecked projector is unexported AND unreferenced);
//   - `engine.MessageToPB` may only be called from
//     internal/engine/conversation.go (the cache projection, which is
//     itself a checked conversion) — any other production caller would be a
//     new projection path that must go through the checked boundary.
//
// The compiler already enforces the unexported symbol; this test makes the
// boundary explicit and catches accidental re-exports or new unchecked
// projection sites at review time.
func TestProjectionCallSiteInventory(t *testing.T) {
	// The test runs with the package dir as cwd; walk up to the module root.
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("module root not found")
		}
		root = parent
	}
	roots := []string{filepath.Join(root, "internal"), filepath.Join(root, "cmd")}
	var files []string
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				files = append(files, path)
			}
			return nil
		})
	}
	if len(files) == 0 {
		t.Fatal("no production Go files scanned")
	}

	checkedCalls := 0
	for _, path := range files {
		fset := token.NewFileSet()
		node, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(node, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch pkg.Name + "." + sel.Sel.Name {
			case "pbconv.ToPBChatRequest", "pbconv.toPBChatRequest":
				t.Errorf("%s: the unchecked full-request projector is called; only ToPBChatRequestChecked may be used", fset.Position(call.Pos()))
			case "pbconv.ToPBChatRequestChecked":
				checkedCalls++
			case "engine.MessageToPB":
				if rel, rerr := filepath.Rel(root, path); rerr != nil || rel != "internal/engine/conversation.go" {
					t.Errorf("%s: MessageToPB called outside the cache projection; use a checked boundary", fset.Position(call.Pos()))
				}
			}
			return true
		})
	}
	if checkedCalls == 0 {
		t.Fatal("inventory found no checked-projection call sites — the boundary is not wired")
	}
}
