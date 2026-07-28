package proxy

import (
	"log"
	"net/http"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// applyProviderCredential authenticates an outbound request to target, having
// first removed whatever credential the caller sent.
//
// The rule this enforces: **a request that has been redirected to a different
// provider must never carry the caller's credential.** The caller issued that
// key to one vendor. Forwarding it to another leaks it, and it will not
// authenticate there anyway — so the redirect cannot work either.
//
// Both auth conventions are set because providers disagree about which they
// read, and each ignores the one it does not use.
//
// An empty credential is legitimate: local model servers take none. But a
// provider that names an env var or a stored secret and still resolves to
// nothing is a misconfiguration the operator wants to hear about, because the
// symptom otherwise is an unexplained 401 from a provider they never called
// directly.
//
// Every path that retargets a request must call this: content routing
// (applyRoute), failover, and any future transport. The alternative is what
// failover did for months — clone the headers, change the host, and silently
// hand vendor A's key to vendor B.
func applyProviderCredential(req *http.Request, target provider.Provider, targetName, reason string, resolve func(env, enc string) string) {
	// Captured before the delete, so forwarding below is explicit rather than
	// "whatever happened to still be on the request".
	callerAuth := req.Header.Get("Authorization")
	callerKey := req.Header.Get("X-Api-Key")

	// Unconditional, and first. Every early return below leaves the request
	// with no caller credential on it, so no future edit to this function can
	// reintroduce the leak by taking a branch that skips the strip.
	req.Header.Del("Authorization")
	req.Header.Del("X-Api-Key")

	if resolve != nil {
		if key := resolve(target.APIKeyEnv, target.APIKeyEnc); key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("X-Api-Key", key)
			return
		}
	}

	if target.ForwardCallerCredential {
		// The operator has stated this target may receive the caller's
		// credential — a second endpoint of the same vendor, or a local model
		// server that ignores it.
		if callerAuth != "" {
			req.Header.Set("Authorization", callerAuth)
		}
		if callerKey != "" {
			req.Header.Set("X-Api-Key", callerKey)
		}
		return
	}

	if target.APIKeyEnv != "" || target.APIKeyEnc != "" {
		log.Printf("[%s] provider %q key is empty — sending unauthenticated", reason, targetName)
	}
}
