package provider

import "strings"

// RoutePrefix is the URL namespace for provider-routed requests.
const RoutePrefix = "/provider/"

// Resolve extracts the provider name from a URL path and returns the
// matching Provider plus the path with the provider prefix stripped.
//
// Paths look like: /provider/deepseek/v1/chat/completions
// Resolve returns the "deepseek" provider and stripped path "/v1/chat/completions".
//
// Returns nil if the path doesn't start with RoutePrefix or names an
// unknown provider.
func Resolve(path string, cfg Config) (*Provider, string, string) {
	if !strings.HasPrefix(path, RoutePrefix) {
		return nil, "", path
	}

	rest := strings.TrimPrefix(path, RoutePrefix)
	// rest = "deepseek/v1/chat/completions"
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		// /provider/deepseek (no trailing path)
		name := rest
		p, ok := cfg.Providers[name]
		if !ok {
			return nil, "", path
		}
		return &p, name, "/"
	}

	name := rest[:slash]
	stripped := rest[slash:] // "/v1/chat/completions"

	p, ok := cfg.Providers[name]
	if !ok {
		return nil, "", path
	}
	return &p, name, stripped
}
