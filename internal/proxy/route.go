package proxy

import (
	"log"
	"net/http"
	"net/url"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/metrics"
	"github.com/torana-edge/torana-edge/internal/provider"
	"github.com/torana-edge/torana-edge/internal/wasm"
)

// applyRoute validates and applies a plugin routing verdict: rewrite the
// upstream URL to the target provider, swap credentials, and override the
// model. Every violation fails OPEN to the original route (log + keep going)
// — a bad verdict must not take the request down.
//
// Credential rule (mirrors the offload provider override): the caller's
// credential is NEVER forwarded to a rerouted provider. Auth comes from the
// target's api_key_env; empty means no auth (local endpoints).
func (s *Server) applyRoute(req *http.Request, chat *engine.ChatRequest, origFormat, origName string, v *wasm.RouteVerdict, cfg provider.Config) {
	if v.Model != "" {
		chat.Model = v.Model
	}
	if v.Provider == "" || v.Provider == origName {
		return // model-only override (or no-op)
	}

	target, ok := cfg.Providers[v.Provider]
	if !ok {
		log.Printf("[route] %s routed to unknown provider %q — keeping %q", v.Plugin, v.Provider, origName)
		return
	}
	if target.Format != origFormat {
		log.Printf("[route] provider %q format %q != %q — cross-format routing unsupported, keeping %q",
			v.Provider, target.Format, origFormat, origName)
		return
	}
	turl, err := url.Parse(target.URL)
	if err != nil {
		log.Printf("[route] provider %q has invalid URL: %v — keeping %q", v.Provider, err, origName)
		return
	}

	rc, _ := req.Context().Value(routeContextKey{}).(*RouteContext)
	if rc == nil {
		return
	}

	req.URL.Scheme = turl.Scheme
	req.URL.Host = turl.Host
	req.Host = turl.Host
	req.URL.Path = joinURLPath(turl.Path, rc.StrippedPath)
	req.URL.RawPath = ""
	// Failover fallbacks and metrics now follow the target.
	rc.ProviderName = v.Provider
	if rs := reqStateFrom(req.Context()); rs != nil {
		rs.Provider = v.Provider
	}

	// Never forward the caller's credential to a rerouted provider.
	applyProviderCredential(req, target, v.Provider, "route", s.resolveSecret)

	metrics.RecordRoutedRequest(req.Context(), origName, v.Provider)
	log.Printf("[route] %s → %s (model %q)", origName, v.Provider, chat.Model)
}
