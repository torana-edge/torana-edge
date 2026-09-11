package format

import "fmt"

// formats holds every registered adapter, keyed by the name providers declare
// in their `format` field — the only key anything ever looks one up by.
//
// It used to be keyed by a URL path prefix that nothing read: Lookup linear-
// scanned the map comparing Name, so the key was decoration. Two formats
// registered under different prefixes with the SAME name both survived, and
// Lookup returned whichever one Go's randomised map iteration reached first —
// a different adapter run to run, for the same configuration. Keying by the
// name makes that collision impossible to express.
var formats = map[string]*Format{}

// Register adds a format under its own Name. Each format package calls this
// from its init().
//
// A missing or duplicate name panics. This runs at init, before the process
// serves anything, so the alternative to a panic is a proxy that starts and
// then routes requests to an arbitrary one of two adapters.
func Register(f Format) {
	if f.Name == "" {
		panic("format: Register requires a Name")
	}
	if existing, dup := formats[f.Name]; dup {
		panic(fmt.Sprintf("format: %q is already registered (%T); two adapters cannot share a name",
			f.Name, existing.Request))
	}
	stored := f
	formats[f.Name] = &stored
}

// Lookup returns the Format registered under the given name (e.g. "openai"),
// or nil if there is none.
//
// The returned pointer is the registry's own, stable for the life of the
// process. Lookup previously returned &f from a range over the map — a pointer
// to a per-iteration COPY, so two callers asking for the same format got
// pointers to different values and a write through one reached nobody.
func Lookup(name string) *Format {
	return formats[name]
}

// Names lists every registered format in no particular order. For diagnostics.
func Names() []string {
	out := make([]string, 0, len(formats))
	for name := range formats {
		out = append(out, name)
	}
	return out
}
