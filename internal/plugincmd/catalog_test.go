package plugincmd

import (
	"os"
	"strings"
	"testing"
)

// The official plugin set was a bare list of names, and two plugins were simply
// missing from it — cache_tier_selector and cache_warmer shipped while
// `torana plugin install --official` quietly did not install them.
//
// Nothing in the list was wrong. The entries just were not there, which is the
// one failure a list of names cannot show you, and no test could catch it
// without something independent to compare against.

// TestEveryExclusionIsJustified: an entry may be left out of --official, but
// not silently. This is what turns "forgot to add it" into a visible decision.
func TestEveryExclusionIsJustified(t *testing.T) {
	for _, p := range officialCatalog {
		if p.install {
			if p.excludedBecause != "" {
				t.Errorf("%s is installed but carries an exclusion reason: %q", p.name, p.excludedBecause)
			}
			continue
		}
		if strings.TrimSpace(p.excludedBecause) == "" {
			t.Errorf("%s is excluded from --official with no reason given.\n"+
				"Say why, so the next person can tell a deliberate exclusion from an omission.", p.name)
		}
	}
}

func TestCatalogEntriesAreWellFormed(t *testing.T) {
	seen := make(map[string]bool)
	for _, p := range officialCatalog {
		if strings.TrimSpace(p.name) == "" {
			t.Error("catalog contains an entry with no name")
		}
		if seen[p.name] {
			t.Errorf("duplicate catalog entry %q", p.name)
		}
		seen[p.name] = true
	}
	if len(officialPlugins()) == 0 {
		t.Error("--official would install nothing")
	}
}

// TestCatalogMatchesThePluginRepository is the check that would have caught the
// original gap. It compares the catalog against the plugin repository itself
// when one is checked out beside this repo — the only independent source of
// truth for which plugins exist.
//
// torana-edge deliberately keeps no copy of the plugins, so this cannot be a
// hard requirement here. It runs for anyone with both repos checked out, and in
// torana-plugins CI, whose behaviour-suite step includes this package for
// exactly that reason — it is the only place both repositories exist.
func TestCatalogMatchesThePluginRepository(t *testing.T) {
	const pluginsRoot = "../../../torana-plugins/plugins"
	entries, err := os.ReadDir(pluginsRoot)
	if err != nil {
		t.Skip("torana-plugins not checked out beside this repo")
	}

	inCatalog := make(map[string]bool, len(officialCatalog))
	for _, p := range officialCatalog {
		inCatalog[p.name] = true
	}

	onDisk := make(map[string]bool)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		onDisk[e.Name()] = true
		if !inCatalog[e.Name()] {
			t.Errorf("plugin %q exists in torana-plugins but is absent from officialCatalog.\n"+
				"`torana plugin install --official` will not install it, and nothing else would "+
				"have told you — this is exactly how cache_tier_selector and cache_warmer were "+
				"missed. Add it, or add it with install:false and a reason.", e.Name())
		}
	}

	for _, p := range officialCatalog {
		if !onDisk[p.name] {
			t.Errorf("officialCatalog names %q, which does not exist in torana-plugins; "+
				"--official would fail to install it", p.name)
		}
	}
}
