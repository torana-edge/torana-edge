package proxy

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
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
// This file is scanned like any other, and is not exempt: a dead reference
// named here would be a dead reference. That is why the three files that exist
// nowhere are described above rather than spelled as paths.

// markdownRef matches a reference to a Markdown file. The vocabulary is the
// repository's real one, not a convenient subset: lowercase names (design.md),
// hyphens, dots, nested directories and relative prefixes all appear in
// tracked paths today. An earlier version of
// this check required an uppercase initial and [A-Za-z0-9_] only, which made
// every lowercase or hyphenated path invisible to it.
var markdownRef = regexp.MustCompile(`(?:\.{1,2}/)*(?:[A-Za-z0-9_.-]+/)*[A-Za-z0-9_.-]+\.md\b`)

// urlRef strips absolute URLs before the scan. A link to this repository on
// github.com embeds a path that looks repository-relative but is resolved by
// the branch it names, not by the working tree.
var urlRef = regexp.MustCompile(`(?i)\bhttps?://\S+`)

// crossRepositoryRoots are the sibling repositories whose documents are
// theirs to own. Matched as an exact first path segment: a substring test
// would also exempt a local directory that merely contains the name.
var crossRepositoryRoots = map[string]bool{
	"torana-plugin-sdk": true,
	"torana-plugins":    true,
}

// referenceResolves reports where a Markdown reference lands, given the set of
// paths the repository tracks. fromFile and every path are repository-relative
// and slash-separated.
//
// A reference that names a directory is resolved AS A PATH — relative to the
// referring file, then from the repository root — and never by its basename.
// The earlier version probed only <root>/<basename> and <root>/docs/<basename>,
// so a reference to the wrong nested path passed whenever a file of the same
// name existed anywhere else.
//
// A reference with no directory at all is the one case where a basename is
// all the author gave: a Go comment citing QUICKSTART.md means the one in
// docs/. That fallback is deliberate, limited to a single directory, and is
// the only place this function looks past the path it was given.
func referenceResolves(fromFile, ref string, tracked map[string]bool) (string, bool) {
	if crossRepository(ref) {
		return "", true
	}
	clean := ref
	for strings.HasPrefix(clean, "./") {
		clean = clean[2:]
	}

	var candidates []string
	if strings.Contains(clean, "/") {
		candidates = []string{
			path.Join(path.Dir(fromFile), clean),
			path.Clean(clean),
		}
	} else {
		candidates = []string{
			path.Join(path.Dir(fromFile), clean),
			path.Clean(clean),
			path.Join("docs", clean),
		}
	}
	for _, c := range candidates {
		// A candidate that climbs out of the repository is not a reference
		// this repository can satisfy.
		if c == ".." || strings.HasPrefix(c, "../") {
			continue
		}
		if tracked[c] {
			return c, true
		}
	}
	return "", false
}

// crossRepository reports whether the reference's first path segment is a
// sibling repository.
func crossRepository(ref string) bool {
	clean := ref
	for strings.HasPrefix(clean, "./") || strings.HasPrefix(clean, "../") {
		if strings.HasPrefix(clean, "./") {
			clean = clean[2:]
			continue
		}
		clean = clean[3:]
	}
	first, _, ok := strings.Cut(clean, "/")
	return ok && crossRepositoryRoots[first]
}

// referencesIn returns every Markdown reference in a file's text, with
// absolute URLs removed first.
func referencesIn(text string) []string {
	return markdownRef.FindAllString(urlRef.ReplaceAllString(text, " "), -1)
}

// imaginary builds a path to a document that does not exist. A table that
// defines what a dead reference IS has to name dead references, and this file
// is scanned like every other — so the fixtures are assembled rather than
// spelled, and the scanner sees no literal path here that it should then be
// able to resolve. Real paths in the table below stay literal on purpose:
// those the scanner should resolve, and will.
func imaginary(p string) string { return p + ".md" }

// What a reference means is written down here, independently of the code that
// resolves one. Every row is a claim about the contract; the resolver is
// checked against the claims rather than against itself.
//
// The first four rows are the failures a reviewer demonstrated against the
// previous version of this check, which passed all of them.
func TestReferenceResolutionContract(t *testing.T) {
	tracked := map[string]bool{
		"README.md":                              true,
		"CONTRIBUTING.md":                        true,
		"design.md":                              true,
		"docs/QUICKSTART.md":                     true,
		imaginary("docs/some-guide"):             true,
		imaginary("benchmarks/BENCHMARK_X"):      true,
		imaginary("benchmarks/README"):           true,
		imaginary("torana-plugin-sdk-notes/ABI"): true,
	}

	for _, tc := range []struct {
		name     string
		from     string
		ref      string
		wantOK   bool
		wantPath string
	}{
		{
			name: "a lowercase name that does not exist is dead",
			from: "CONTRIBUTING.md", ref: imaginary("docs/not-real"), wantOK: false,
		},
		{
			name: "a lowercase name that exists resolves",
			from: "CONTRIBUTING.md", ref: "design.md", wantOK: true, wantPath: "design.md",
		},
		{
			name: "a hyphenated name resolves",
			from: "README.md", ref: imaginary("docs/some-guide"), wantOK: true, wantPath: imaginary("docs/some-guide"),
		},
		{
			name: "a hyphenated name that does not exist is dead",
			from: "README.md", ref: imaginary("docs/some-other-guide"), wantOK: false,
		},
		{
			name: "the right basename in the wrong directory is dead",
			from: "README.md", ref: imaginary("internal/QUICKSTART"), wantOK: false,
		},
		{
			name: "a nested relative path out of docs resolves",
			from: "docs/BENCHMARKS.md", ref: imaginary("../benchmarks/BENCHMARK_X"),
			wantOK: true, wantPath: imaginary("benchmarks/BENCHMARK_X"),
		},
		{
			name: "a relative path to a sibling resolves",
			from: imaginary("benchmarks/BENCHMARK_X"), ref: "./README.md",
			wantOK: true, wantPath: imaginary("benchmarks/README"),
		},
		{
			name: "a root-relative path resolves from anywhere",
			from: "internal/proxy/server.go", ref: "docs/QUICKSTART.md",
			wantOK: true, wantPath: "docs/QUICKSTART.md",
		},
		{
			name: "a bare name in a Go comment resolves through docs/",
			from: "internal/proxy/server.go", ref: "QUICKSTART.md",
			wantOK: true, wantPath: "docs/QUICKSTART.md",
		},
		{
			name: "a bare name that exists nowhere is dead",
			from: "internal/proxy/server.go", ref: imaginary("HANDOFF"), wantOK: false,
		},
		{
			name: "an exact cross-repository prefix is exempt",
			from: "CONTRIBUTING.md", ref: "torana-plugin-sdk/docs/WRITING_A_PLUGIN.md", wantOK: true,
		},
		{
			name: "the other sibling repository is exempt too",
			from: "CONTRIBUTING.md", ref: "torana-plugins/README.md", wantOK: true,
		},
		{
			name: "a local directory that merely starts with a sibling's name is not exempt",
			from: "CONTRIBUTING.md", ref: imaginary("torana-plugin-sdk-notes/ABI"),
			wantOK: true, wantPath: imaginary("torana-plugin-sdk-notes/ABI"),
		},
		{
			name: "and is dead when it does not exist",
			from: "CONTRIBUTING.md", ref: imaginary("torana-plugin-sdk-notes/MISSING"), wantOK: false,
		},
		{
			name: "a reference climbing out of the repository is dead",
			from: "README.md", ref: imaginary("../elsewhere/THING"), wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := referenceResolves(tc.from, tc.ref, tracked)
			if ok != tc.wantOK {
				t.Fatalf("referenceResolves(%q, %q) ok = %v, want %v", tc.from, tc.ref, ok, tc.wantOK)
			}
			if tc.wantPath != "" && got != tc.wantPath {
				t.Errorf("referenceResolves(%q, %q) = %q, want %q", tc.from, tc.ref, got, tc.wantPath)
			}
		})
	}
}

// What counts as a reference is written down too, since a pattern that stops
// matching turns this whole check into a silent pass.
func TestReferenceExtractionContract(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want []string
	}{
		{name: "a bare name in prose", text: "See QUICKSTART.md for more.", want: []string{"QUICKSTART.md"}},
		{name: "a lowercase name", text: "See `" + imaginary("docs/not-real") + "`.", want: []string{imaginary("docs/not-real")}},
		{name: "a hyphenated name", text: "[x](" + imaginary("docs/some-guide") + ")", want: []string{imaginary("docs/some-guide")}},
		{name: "a relative path", text: "[x](" + imaginary("../benchmarks/A_B") + ")", want: []string{imaginary("../benchmarks/A_B")}},
		{name: "a dot-slash path", text: "[x](./README.md)", want: []string{"./README.md"}},
		{
			name: "an absolute URL is not a repository reference",
			text: "https://github.com/torana-edge/torana-edge/blob/main/docs/QUICKSTART.md",
			want: nil,
		},
		{
			name: "a URL and a real reference in one line",
			text: "see https://example.com/a/B.md and docs/QUICKSTART.md",
			want: []string{"docs/QUICKSTART.md"},
		},
		{name: "not a markdown file", text: "config.json and main.go", want: nil},
		{name: "a name that merely contains md", text: "something.mdx and a.mdown", want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := referencesIn(tc.text)
			if len(got) != len(tc.want) {
				t.Fatalf("referencesIn(%q) = %v, want %v", tc.text, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("referencesIn(%q)[%d] = %q, want %q", tc.text, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// Every Markdown path named by a tracked Go or Markdown file must resolve to a
// tracked file. Cross-repository references are the sibling repository's to
// own and are not resolved here.
func TestReferencedDocumentsExist(t *testing.T) {
	root := filepath.Join("..", "..")
	all, err := exec.Command("git", "-C", root, "ls-files").Output()
	if err != nil {
		t.Skipf("git is unavailable, so the tracked file list cannot be built: %v", err)
	}
	tracked := map[string]bool{}
	for _, f := range strings.Fields(string(all)) {
		tracked[f] = true
	}
	if len(tracked) == 0 {
		t.Fatal("git listed no tracked files; this check has stopped seeing what it guards")
	}

	var scanned []string
	for f := range tracked {
		if strings.HasSuffix(f, ".go") || strings.HasSuffix(f, ".md") {
			scanned = append(scanned, f)
		}
	}
	sort.Strings(scanned)
	if len(scanned) == 0 {
		t.Fatal("no tracked Go or Markdown files; this check has stopped seeing what it guards")
	}

	checked := 0
	for _, rel := range scanned {
		// The working tree, not HEAD: this must fail on the change being
		// made, not on the one already committed.
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		for _, ref := range referencesIn(string(body)) {
			checked++
			if _, ok := referenceResolves(rel, ref, tracked); !ok {
				t.Errorf("%s references %s, which does not exist in this repository", rel, ref)
			}
		}
	}
	if checked == 0 {
		t.Error("no Markdown references were found at all; the pattern has stopped matching")
	}
}
