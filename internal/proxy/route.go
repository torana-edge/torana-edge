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
// The target provider's explicit auth policy is applied after routing.
func (s *Server) applyRoute(req *http.Request, chat *engine.ChatRequest, origFormat, origName string, v *wasm.RouteVerdict, cfg provider.Config) {
	if v.Provider == "" || v.Provider == origName {
		// Model-only override (or no-op): there is no provider to validate,
		// so the model stands on its own.
		if v.Model != "" {
			chat.Model = v.Model
		}
		return
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
	authCandidate := req.Clone(req.Context())
	authCandidate.Header = req.Header.Clone()
	caller := callerCredentials{}
	if rs := reqStateFrom(req.Context()); rs != nil {
		caller = rs.CallerCredentials
	}
	if err := applyProviderCredential(req.Context(), authCandidate, target, caller, s.resolveCredential); err != nil {
		log.Printf("[route] provider %q credential unavailable — keeping %q", v.Provider, origName)
		return
	}
	req.Header = authCandidate.Header

	// Only now, with every check passed. A verdict is ONE decision: "send this
	// to provider X as model Y". Applying the model up front meant a verdict
	// rejected for an unknown provider, a format mismatch, an unparseable URL
	// or a missing credential still changed it — so the request went to the
	// ORIGINAL provider carrying a model chosen for a different one, which
	// that provider does not serve. Failing open has to mean the original
	// route, not half of a route nobody asked for.
	if v.Model != "" {
		chat.Model = v.Model
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

	metrics.RecordRoutedRequest(req.Context(), origName, v.Provider)
	log.Printf("[route] %s → %s (model %q)", origName, v.Provider, chat.Model)
}
