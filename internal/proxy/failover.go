package proxy

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/url"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// failoverRoundTripper wraps the default transport and retries failed
// requests against configured fallback providers (429 / 5xx).
type failoverRoundTripper struct {
	base     http.RoundTripper
	cfg      func() provider.Config
}

func (t *failoverRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// First attempt — no body buffering needed for the normal path.
	resp, err := t.base.RoundTrip(req)
	if err == nil && !shouldRetry(resp) {
		return resp, nil
	}
	if resp != nil {
		resp.Body.Close()
	}

	// Find the provider name from the original URL path.
	provName, fallbacks := extractFallbacks(req.URL, t.cfg())
	if len(fallbacks) == 0 {
		if resp != nil {
			return resp, nil
		}
		return nil, err
	}

	// Only buffer body when we actually need to retry.
	var bodyBytes []byte
	if req.Body != nil {
		bodyBytes, _ = io.ReadAll(req.Body)
		req.Body.Close()
	}

	log.Printf("[failover] %s returned %v — trying fallbacks: %v",
		provName, statusOrErr(resp, err), fallbacks)

	for _, fbName := range fallbacks {
		fb, ok := t.cfg().Providers[fbName]
		if !ok {
			continue
		}
		fbURL, uErr := url.Parse(fb.URL)
		if uErr != nil {
			log.Printf("[failover] invalid fallback URL for %s: %v", fbName, uErr)
			continue
		}

		retryReq := cloneWithBody(req, bodyBytes)
		retryReq.URL.Scheme = fbURL.Scheme
		retryReq.URL.Host = fbURL.Host

		retryResp, retryErr := t.base.RoundTrip(retryReq)
		if retryErr != nil {
			log.Printf("[failover] %s failed: %v", fbName, retryErr)
			continue
		}
		if shouldRetry(retryResp) {
			retryResp.Body.Close()
			log.Printf("[failover] %s also returned %d — trying next", fbName, retryResp.StatusCode)
			continue
		}
		log.Printf("[failover] %s succeeded", fbName)
		return retryResp, nil
	}

	return resp, err
}

func shouldRetry(resp *http.Response) bool {
	return resp.StatusCode == 429 || resp.StatusCode >= 500
}

func extractFallbacks(u *url.URL, cfg provider.Config) (string, []string) {
	_, name, _ := provider.Resolve(u.Path, cfg)
	if name == "" {
		return "", nil
	}
	p, ok := cfg.Providers[name]
	if !ok || len(p.Fallback) == 0 {
		return name, nil
	}
	return name, p.Fallback
}

func statusOrErr(resp *http.Response, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	if resp != nil {
		return http.StatusText(resp.StatusCode)
	}
	return "unknown"
}

func cloneWithBody(req *http.Request, body []byte) *http.Request {
	r2 := req.Clone(req.Context())
	if body != nil {
		r2.Body = io.NopCloser(bytes.NewReader(body))
		r2.ContentLength = int64(len(body))
	}
	return r2
}
