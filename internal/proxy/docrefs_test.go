package proxy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A reference to a document that does not exist is worse than no reference:
// it sends a reader looking for an authority, and the one they cannot find is
// assumed to hold the answer they need.
//
// CONTRIBUTING.md step 5 told every contributor to update an ABI document and
// two plugin guides. The guides are real but belong to the SDK
// (torana-plugin-sdk/docs/WRITING_A_PLUGIN.md,
// torana-plugin-sdk/docs/WASM_PLUGIN_GUIDE.md); the ABI document exists in no
// repository at all, so the documented step could not be performed. Comments
// cited three more Markdown files — a handoff note, a marshal-failure
// checkpoint and a guide — that were never here either.
//
// This checks every repository-relative Markdown path named by tracked Go and
// Markdown files. Cross-repository references (torana-plugin-sdk/...) are
// allowed and deliberately not resolved here.
//
// This file is scanned like any other, and is not exempt: a dead reference
// named here would be a dead reference. That is why the three files that exist
// nowhere are described above rather than spelled as paths.
func TestReferencedDocumentsExist(t *testing.T) {
	root := filepath.Join("..", "..")
	out, err := exec.Command("git", "-C", root, "ls-files", "*.go", "*.md").Output()
	if err != nil {
		t.Skipf("git is unavailable, so the tracked file list cannot be built: %v", err)
	}
	files := strings.Fields(string(out))
	if len(files) == 0 {
		t.Fatal("git listed no tracked Go or Markdown files; this check has stopped seeing what it guards")
	}

	// A bare or docs/-relative Markdown filename, in a comment or a link. The
	// leading group captures any repository prefix so a cross-repository path
	// is recognised as one rather than truncated to its tail.
	ref := regexp.MustCompile(`((?:[A-Za-z0-9_.-]+/)*)((?:docs/)?[A-Z][A-Za-z0-9_]*\.md)`)

	checked := 0
	for _, rel := range files {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		for _, m := range ref.FindAllStringSubmatch(string(body), -1) {
			prefix, name := m[1], m[2]
			// Cross-repository references are the other repository's to own.
			if strings.Contains(prefix, "torana-plugin-sdk") || strings.Contains(prefix, "torana-plugins") {
				continue
			}
			checked++
			if fileExists(root, name) {
				continue
			}
			t.Errorf("%s references %s, which does not exist in this repository", rel, name)
		}
	}
	if checked == 0 {
		t.Error("no Markdown references were found at all; the pattern has stopped matching")
	}
}

func fileExists(root, name string) bool {
	for _, candidate := range []string{name, filepath.Join("docs", name)} {
		if _, err := os.Stat(filepath.Join(root, candidate)); err == nil {
			return true
		}
	}
	return false
}
