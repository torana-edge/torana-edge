package plugincmd

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The README's Quick Start told readers to run `make plugins`, a target the
// vendored-tree deletion removed. Nothing caught it, because docs are not
// compiled — so following the setup instructions failed at step 2.
//
// This pins every `make <target>` the README tells a reader to run against the
// targets the Makefile actually defines.
func TestReadmeMakeTargetsExist(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
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

	for _, m := range regexp.MustCompile(`make ([a-zA-Z0-9_-]+)`).FindAllStringSubmatch(string(readme), -1) {
		target := m[1]
		if !defined[target] {
			t.Errorf("README tells the reader to run `make %s`, which the Makefile does not "+
				"define. Following the instructions fails.", target)
		}
	}
}
