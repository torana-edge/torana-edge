package proxy

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// callerCredentials is captured once at the ingress boundary. Authentication
// is never derived from a request after Torana has rewritten or retried it:
// doing so can mistake provider A's managed credential for the caller's and
// leak it to provider B during routing or failover.
//
// It covers EVERY header caller mode forwards. It used to hold three fields
// while the strip list grew to eight, so widening that list to close a leak in
// credential mode silently broke authentication in caller mode: an Azure
// `api-key`, an AWS session token, and the Google client identity were removed
// on the way through and never put back.
type callerCredentials struct {
	headers http.Header
}

// callerForwardedHeaders are the caller's own credentials. They are stripped
// before a provider-managed credential is installed, and restored verbatim in
// caller mode — where forwarding them is the entire point.
var callerForwardedHeaders = []string{
	"Authorization",
	"X-Api-Key",
	"X-Goog-Api-Key",
	"Api-Key",              // Azure OpenAI
	"X-Amz-Security-Token", // AWS session credentials, alongside SigV4
	"X-Goog-Api-Client",    // Google client identity
}

// neverForwardedHeaders are ambient browser/proxy credentials that are not the
// caller authenticating to a model provider and have no business reaching one,
// in ANY mode. They are dropped and never restored.
var neverForwardedHeaders = []string{
	"Cookie",
	"Proxy-Authorization",
}

func callerCredentialsFrom(req *http.Request) callerCredentials {
	snapshot := make(http.Header, len(callerForwardedHeaders))
	if req == nil {
		return callerCredentials{headers: snapshot}
	}
	for _, name := range callerForwardedHeaders {
		if values := req.Header.Values(name); len(values) > 0 {
			snapshot[http.CanonicalHeaderKey(name)] = slices.Clone(values)
		}
	}
	return callerCredentials{headers: snapshot}
}

// applyProviderCredential enforces the target provider's explicit auth mode.
// It strips credentials first, then either restores the intercepted caller
// values, installs one host-resolved credential, or sends no credential.
// Both lists are stripped before any mode decides what goes back. The strip
// used to name three headers, so a caller's Azure `api-key`, a Cookie, or a
// Proxy-Authorization travelled to whatever upstream the operator had
// configured — precisely what auth.mode=credential exists to prevent. The
// point of that mode is that the caller's secrets do not leave the machine.

func applyProviderCredential(ctx context.Context, req *http.Request, target provider.Provider, caller callerCredentials, resolve func(context.Context, string) ([]byte, error)) error {
	for _, name := range callerForwardedHeaders {
		req.Header.Del(name)
	}
	for _, name := range neverForwardedHeaders {
		req.Header.Del(name)
	}
	switch target.Auth.EffectiveMode() {
	case "caller":
		// Restored from the immutable ingress snapshot, never from the live
		// request: by now it may carry the PRIMARY provider's managed
		// credential, which must not be handed to a fallback.
		for name, values := range caller.headers {
			for _, v := range values {
				req.Header.Add(name, v)
			}
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
