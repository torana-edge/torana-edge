package plugincmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The README's Quick Start named a Makefile target the vendored-tree deletion
// removed. Nothing caught it, because docs are not
// compiled — so following the setup instructions failed at step 2.
//
// The same stale target also survived in an operator-facing runtime log and in
// four test skip messages, which a README-only check could not see. This scans
// every file a reader or operator could be told a command by.
func TestReferencedMakeTargetsExist(t *testing.T) {
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	defined := make(map[string]bool)
	for _, line := range strings.Split(string(makefile), "\n") {
		if m := regexp.MustCompile(`^([a-zA-Z0-9_-]+):`).FindStringSubmatch(line); m != nil {
			defined[m[1]] = true
		}
	}
	if len(defined) == 0 {
		t.Fatal("no targets parsed from the Makefile; this check would pass vacuously")
	}

	// Quoted or fenced only. An unquoted pattern also matches ordinary English
	// prose, which is how a check like this becomes noise and then gets
	// deleted. Commands appear in backticks or quotes.
	quoted := regexp.MustCompile("[`'\"]make ([a-zA-Z0-9_-]+)")

	var scanned int
	err = filepath.WalkDir("../..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // unreadable entries are not a doc failure
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".go" && ext != ".md" {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		scanned++
		for _, m := range quoted.FindAllStringSubmatch(string(b), -1) {
			if !defined[m[1]] {
				t.Errorf("%s references `make %s`, which the Makefile does not define. "+
					"Anyone following that instruction — a reader, or an operator reading a "+
					"log line — hits a target that is not there.", path, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatal("scanned no files; the check would pass vacuously")
	}
}
