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
func applyProviderCredential(ctx context.Context, req *http.Request, target provider.Provider, caller callerCredentials, resolve func(context.Context, string) ([]byte, error)) error {
	req.Header.Del("Authorization")
	req.Header.Del("X-Api-Key")
	req.Header.Del("X-Goog-Api-Key")
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
