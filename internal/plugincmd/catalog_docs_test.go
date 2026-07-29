package plugincmd

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// officialPluginsHeading is the README section whose table must match the
// catalog.
const officialPluginsHeading = "## Official Plugins"

// The README's Official Plugins table must be exactly the set `--official`
// installs — no more, no less.
//
// It listed `auth`, which the catalog deliberately excludes: auth's own manifest
// says it is not published to the public registry, because it demonstrates the
// identity capability surface and performs no verification. A table that
// presents it alongside the rest invites someone to install it into an
// access-control position it was explicitly not built for.
//
// The first version of this test only checked the catalog against the table,
// and scanned the whole README for anything shaped like a row. That let a
// fabricated plugin row pass unnoticed, because a name absent from the catalog
// matched neither branch. Set equality against rows parsed from this one
// section is what actually pins the table.
func TestREADMEOfficialTableMatchesCatalog(t *testing.T) {
	listed := officialTableRows(t)

	want := map[string]bool{}
	excluded := map[string]string{}
	for _, p := range officialCatalog {
		if p.install {
			want[p.name] = true
		} else {
			excluded[p.name] = p.excludedBecause
		}
	}

	for name := range want {
		if !listed[name] {
			t.Errorf("%q is installed by --official but is missing from the README table", name)
		}
	}
	for name := range listed {
		if want[name] {
			continue
		}
		if why, isExcluded := excluded[name]; isExcluded {
			t.Errorf("%q is in the README table but --official does not install it: %s", name, why)
			continue
		}
		t.Errorf("%q is in the README table but is not an official plugin at all — "+
			"the table must list exactly what --official installs", name)
	}
}

// officialTableRows returns the plugin names in the Official Plugins table,
// parsed from that section alone. Scanning the whole file would also collect
// the environment-variable and supported-format tables, which is how the
// earlier version of this check ended up comparing against a set that could
// never be wrong.
func officialTableRows(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}

	lines := strings.Split(string(raw), "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == officialPluginsHeading {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("README has no %q section — has it been renamed?", officialPluginsHeading)
	}

	rows := map[string]bool{}
	sawTable := false
	for _, line := range lines[start:] {
		if strings.HasPrefix(line, "## ") {
			break // next section
		}
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		name, _, ok := strings.Cut(strings.TrimPrefix(line, "| `"), "`")
		if !ok {
			continue
		}
		sawTable = true
		rows[name] = true
	}
	if !sawTable {
		t.Fatalf("found no plugin rows under %q — has the table moved?", officialPluginsHeading)
	}

	names := make([]string, 0, len(rows))
	for n := range rows {
		names = append(names, n)
	}
	sort.Strings(names)
	t.Logf("README lists: %s", strings.Join(names, ", "))
	return rows
}
