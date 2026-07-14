package format

import "strings"

var byPrefix = map[string]Format{}

// Register adds a format keyed by URL path prefix (e.g. "/openai/").
// Each format package calls this in its init().
func Register(prefix string, f Format) {
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix = prefix + "/"
	}
	byPrefix[prefix] = f
}



// Lookup returns the Format registered under the given name (e.g. "openai").
// Returns nil if no format matches.
func Lookup(name string) *Format {
	for _, f := range byPrefix {
		if f.Name == name {
			return &f
		}
	}
	return nil
}
