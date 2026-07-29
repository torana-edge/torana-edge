package plugincmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The README's Official Plugins table must agree with what `--official`
// actually installs.
//
// It listed `auth`, which the catalog deliberately excludes: auth's own
// manifest says it is not published to the public registry, because it
// demonstrates the identity capability surface and performs no verification.
// A table that presents it alongside the rest invites someone to install it
// into an access-control position it was explicitly not built for — the single
// most consequential way for this table to be wrong.
func TestREADMEOfficialTableMatchesCatalog(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	readme := string(raw)

	// The table rows start with | `name` |.
	listed := map[string]bool{}
	for _, line := range strings.Split(readme, "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		name, _, ok := strings.Cut(strings.TrimPrefix(line, "| `"), "`")
		if !ok {
			continue
		}
		listed[name] = true
	}
	if len(listed) == 0 {
		t.Fatal("found no plugin rows in the README — has the table moved?")
	}

	for _, p := range officialCatalog {
		switch {
		case p.install && !listed[p.name]:
			t.Errorf("%q is installed by --official but is missing from the README table", p.name)
		case !p.install && listed[p.name]:
			t.Errorf("%q is listed in the README table but --official does not install it: %s",
				p.name, p.excludedBecause)
		}
	}
}
