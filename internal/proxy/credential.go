package proxy

import (
	"context"
	"fmt"
	"net/http"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// callerCredentials is captured once at the ingress boundary. Authentication
// is never derived from a request after Torana has rewritten or retried it:
// doing so can mistake provider A's managed credential for the caller's and
// leak it to provider B during routing or failover.
type callerCredentials struct {
	Authorization string
	APIKey        string
	GoogleAPIKey  string
}

func callerCredentialsFrom(req *http.Request) callerCredentials {
	if req == nil {
		return callerCredentials{}
	}
	return callerCredentials{
		Authorization: req.Header.Get("Authorization"),
		APIKey:        req.Header.Get("X-Api-Key"),
		GoogleAPIKey:  req.Header.Get("X-Goog-Api-Key"),
	}
}

// applyProviderCredential enforces the target provider's explicit auth mode.
// It strips credentials first, then either restores the intercepted caller
// values, installs one host-resolved credential, or sends no credential.
// callerCredentialHeaders are stripped before a provider-managed credential is
// installed. It is an ALLOWLIST of what may cross a provider boundary,
// expressed as the complement: anything here is removed, and the three modes
// below decide what goes back.
//
// It was three entries, so a caller's Azure `api-key`, a Cookie, or a
// Proxy-Authorization travelled to whatever upstream the operator had
// configured — precisely what auth.mode=credential exists to prevent. The
// point of that mode is that the caller's secrets do not leave the machine.
var callerCredentialHeaders = []string{
	"Authorization",
	"X-Api-Key",
	"X-Goog-Api-Key",
	"Api-Key",              // Azure OpenAI
	"X-Goog-Api-Client",    // Google client identity
	"Cookie",               // session credentials
	"Proxy-Authorization",  // proxy credentials, never an upstream's business
	"X-Amz-Security-Token", // AWS session credentials
}

func applyProviderCredential(ctx context.Context, req *http.Request, target provider.Provider, caller callerCredentials, resolve func(context.Context, string) ([]byte, error)) error {
	for _, name := range callerCredentialHeaders {
		req.Header.Del(name)
	}
	switch target.Auth.EffectiveMode() {
	case "caller":
		if caller.Authorization != "" {
			req.Header.Set("Authorization", caller.Authorization)
		}
		if caller.APIKey != "" {
			req.Header.Set("X-Api-Key", caller.APIKey)
		}
		if caller.GoogleAPIKey != "" {
			req.Header.Set("X-Goog-Api-Key", caller.GoogleAPIKey)
		}
		return nil
	case "credential":
		if resolve == nil {
			return fmt.Errorf("credential resolver is not configured")
		}
		secret, err := resolve(ctx, target.Auth.Credential)
		if err != nil {
			return err
		}
		if len(secret) == 0 {
			return fmt.Errorf("credential is empty")
		}
		value := string(secret)
		// Use the provider protocol's native credential header. Sending the
		// same secret in several headers needlessly widens its exposure and
		// breaks strict compatible endpoints.
		switch target.Format {
		case "anthropic":
			req.Header.Set("X-Api-Key", value)
		case "gemini":
			req.Header.Set("X-Goog-Api-Key", value)
		default:
			req.Header.Set("Authorization", "Bearer "+value)
		}
		return nil
	case "none":
		return nil
	default:
		return fmt.Errorf("provider auth mode is invalid")
	}
}

func (s *Server) applyUpstreamCredential(req *http.Request, cfg provider.Config, rc *RouteContext) error {
	target, ok := cfg.Providers[rc.ProviderName]
	if !ok {
		return fmt.Errorf("provider is no longer configured")
	}
	caller := callerCredentials{}
	if rs := reqStateFrom(req.Context()); rs != nil {
		caller = rs.CallerCredentials
	}
	return applyProviderCredential(req.Context(), req, target, caller, s.resolveCredential)
}

func markCredentialFailure(req *http.Request, format string, rc *RouteContext) {
	rc.Block = renderCredentialUnavailable(format)
	if rs := reqStateFrom(req.Context()); rs != nil {
		rs.Synthetic = true
		rs.Verdict = "host_error"
		rs.AuditErrorCode = "credential_unavailable"
	}
	req.Body = http.NoBody
	req.ContentLength = 0
}
