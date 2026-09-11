package format

import "testing"

// Lookup must return the registry's own value, stable across calls.
//
// It used to `return &f` from a range over the map — a pointer to a
// per-iteration COPY. Two callers asking for the same format got pointers to
// different values, so nothing could rely on the pointer identity and a write
// through one reached nobody.
func TestLookupReturnsTheSamePointerEveryTime(t *testing.T) {
	first := Lookup("openai")
	if first == nil {
		t.Fatal("openai is not registered; this check cannot see what it guards")
	}
	if second := Lookup("openai"); second != first {
		t.Errorf("Lookup returned %p then %p for the same name; the registry must hand "+
			"back its own value, not a copy", first, second)
	}
}

func TestLookupUnknownFormatIsNil(t *testing.T) {
	if got := Lookup("no-such-format"); got != nil {
		t.Errorf("Lookup(%q) = %+v, want nil", "no-such-format", got)
	}
}

// Every registered format must be reachable under the name providers declare.
func TestEveryRegisteredFormatIsReachableByName(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("no formats registered")
	}
	for _, name := range names {
		f := Lookup(name)
		if f == nil {
			t.Errorf("format %q is registered but Lookup cannot find it", name)
			continue
		}
		if f.Name != name {
			t.Errorf("format registered under %q reports Name %q", name, f.Name)
		}
	}
}

// Two adapters sharing a name used to BOTH survive, with Lookup returning
// whichever Go's randomised map iteration reached first — a different adapter
// run to run for the same configuration. Registration now refuses it, at init,
// before the process serves anything.
func TestRegisterRefusesADuplicateName(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("registering a second format under an existing name was accepted; " +
				"Lookup would then return an arbitrary one of the two")
		}
	}()
	Register(Format{Name: "openai"})
}

func TestRegisterRefusesAnEmptyName(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a format with no name was registered; nothing could ever look it up")
		}
	}()
	Register(Format{})
}
