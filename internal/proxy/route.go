package proxy

import (
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"

	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/metrics"
	"github.com/torana-edge/torana-edge/internal/provider"
)

// routeVerdict is the plugin-supplied routing override carried in
// ToranaMeta["_route"] (set via sdk.RouteRequest).
type routeVerdict struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// applyRoute validates and applies a plugin routing verdict: rewrite the
// upstream URL to the target provider, swap credentials, and override the
// model. Every violation fails OPEN to the original route (log + keep going)
// — a bad verdict must not take the request down.
//
// Credential rule (mirrors the offload provider override): the caller's
// credential is NEVER forwarded to a rerouted provider. Auth comes from the
// target's api_key_env; empty means no auth (local endpoints).
func applyRoute(req *http.Request, chat *engine.ChatRequest, origFormat, origName string, raw any, cfg provider.Config) {
	b, _ := json.Marshal(raw)
	var v routeVerdict
	_ = json.Unmarshal(b, &v)

	if v.Model != "" {
		chat.Model = v.Model
	}
	if v.Provider == "" || v.Provider == origName {
		return // model-only override (or no-op)
	}

	target, ok := cfg.Providers[v.Provider]
	if !ok {
		log.Printf("[route] plugin routed to unknown provider %q — keeping %q", v.Provider, origName)
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
	reqStateFrom(req.Context()).Provider = v.Provider

	// Never forward the caller's credential to a rerouted provider.
	req.Header.Del("Authorization")
	req.Header.Del("X-Api-Key")
	if target.APIKeyEnv != "" {
		if k := os.Getenv(target.APIKeyEnv); k != "" {
			// Cover both auth conventions; providers ignore the one they
			// don't use.
			req.Header.Set("Authorization", "Bearer "+k)
			req.Header.Set("X-Api-Key", k)
		} else {
			log.Printf("[route] provider %q api_key_env %q is empty — sending unauthenticated", v.Provider, target.APIKeyEnv)
		}
	}

	metrics.RecordRoutedRequest(req.Context(), origName, v.Provider)
	log.Printf("[route] %s → %s (model %q)", origName, v.Provider, chat.Model)
}
