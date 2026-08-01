package proxy

import (
	"errors"
	"net/http"
	"net/url"
)

// errTooManyRedirects is returned by redirectPolicy once a same-origin chain
// exceeds the ten-hop bound. It is DETERMINISTIC — repeating the request hits
// the same loop — so callers classify it as a non-retryable refusal rather
// than a transient transport failure.
var errTooManyRedirects = errors.New("stopped after 10 redirects")

// redirectPolicy builds an http.Client.CheckRedirect that confines redirects
// to the configured origin AND enforces the ten-hop bound.
//
// A custom CheckRedirect REPLACES Go's default policy rather than composing
// with it: the standard library runs either the custom callback or
// defaultCheckRedirect, and the default ten-hop cap lives only in the latter.
// This policy therefore enforces both invariants itself:
//
//  1. no more than ten redirects, matching Go's default bound — otherwise a
//     same-origin redirect loop would run until the client timeout while
//     consuming only one budget slot; and
//  2. never follow a scheme/host change — http.ErrUseLastResponse returns the
//     original 3xx as the reached provider outcome, so no request, and no
//     credential (Go strips Authorization but not X-Api-Key on a cross-host
//     redirect), can leave the configured origin.
func redirectPolicy(origin *url.URL) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errTooManyRedirects
		}
		if req.URL.Scheme != origin.Scheme || req.URL.Host != origin.Host || req.URL.Hostname() != origin.Hostname() {
			return http.ErrUseLastResponse
		}
		return nil
	}
}
