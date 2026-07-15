package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"

	"github.com/torana-edge/torana-edge/internal/provider"
)

// failoverRoundTripper wraps the default transport and retries failed
// requests against configured fallback providers (429 / 5xx).
type failoverRoundTripper struct {
	base        http.RoundTripper
	cfg         func() provider.Config
	rateLimiter *RateLimiter
}

// rateLimitBody wraps the response body to release the concurrency token on close.
type rateLimitBody struct {
	io.ReadCloser
	identity    string
	rateLimiter *RateLimiter
}

func (r *rateLimitBody) Close() error {
	r.rateLimiter.Release(r.identity)
	return r.ReadCloser.Close()
}

func (t *failoverRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Find the provider name from the context.
	provName, fallbacks := extractFallbacks(req, t.cfg())

	rc, _ := req.Context().Value(routeContextKey{}).(*RouteContext)
	identity := "default"
	if rc != nil && rc.Identity != "" {
		identity = rc.Identity
	}

	if !t.rateLimiter.Acquire(identity) {
		return &http.Response{
			StatusCode:    http.StatusTooManyRequests,
			Status:        "429 Too Many Requests",
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Body:          io.NopCloser(bytes.NewReader([]byte(`{"error":"rate limit exceeded"}`))),
			ContentLength: -1,
			Request:       req,
			Header:        make(http.Header),
		}, nil
	}

	// If there are fallbacks, we MUST buffer the body before the first attempt
	// because RoundTrip consumes the body.
	var bodyBytes []byte
	if len(fallbacks) > 0 && req.Body != nil {
		lr := io.LimitReader(req.Body, maxBodySize+1)
		bodyBytes, _ = io.ReadAll(lr)
		req.Body.Close()
		if len(bodyBytes) > maxBodySize {
			// Cannot retry safely if body is oversized. We will let the first attempt fail or pass,
			// but we won't have the body for fallbacks.
			bodyBytes = nil
			fallbacks = nil
		} else {
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}
	}

	// First attempt.
	resp, err := t.base.RoundTrip(req)
	if err == nil && !shouldRetry(resp) {
		resp.Body = &rateLimitBody{ReadCloser: resp.Body, identity: identity, rateLimiter: t.rateLimiter}
		return resp, nil
	}

	if len(fallbacks) == 0 {
		// Still release the concurrency token: on a transport error there
		// is no body whose Close would release it, and a retryable status
		// returned unwrapped would leak it the same way.
		if resp != nil && resp.Body != nil {
			resp.Body = &rateLimitBody{ReadCloser: resp.Body, identity: identity, rateLimiter: t.rateLimiter}
		} else {
			t.rateLimiter.Release(identity)
		}
		return resp, err
	}

	var lastResp *http.Response = resp
	var lastErr error = err

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

		// Reconstruct retry URL using the fallback base URL and original stripped path.
		rc, _ := req.Context().Value(routeContextKey{}).(*RouteContext)
		retryReq.URL.Scheme = fbURL.Scheme
		retryReq.URL.Host = fbURL.Host
		retryReq.Host = fbURL.Host // Finding 3: update Host header
		if rc != nil {
			retryReq.URL.Path = joinURLPath(fbURL.Path, rc.StrippedPath)
		} else {
			retryReq.URL.Path = fbURL.Path
		}
		retryReq.URL.RawPath = ""

		retryResp, retryErr := t.base.RoundTrip(retryReq)

		// Close previous response since we are going to try the next one or we got a new one
		if lastResp != nil {
			lastResp.Body.Close()
		}

		lastResp = retryResp
		lastErr = retryErr

		if retryErr != nil {
			log.Printf("[failover] %s failed: %v", fbName, retryErr)
			continue
		}
		if shouldRetry(retryResp) {
			log.Printf("[failover] %s also returned %d — trying next", fbName, retryResp.StatusCode)
			continue
		}
		log.Printf("[failover] %s succeeded", fbName)
		retryResp.Body = &rateLimitBody{ReadCloser: retryResp.Body, identity: identity, rateLimiter: t.rateLimiter}
		return retryResp, nil
	}

	if lastResp != nil && lastResp.Body != nil {
		lastResp.Body = &rateLimitBody{ReadCloser: lastResp.Body, identity: identity, rateLimiter: t.rateLimiter}
	} else {
		// If lastResp is nil (only error returned), we need to release token
		t.rateLimiter.Release(identity)
	}

	return lastResp, lastErr
}

func shouldRetry(resp *http.Response) bool {
	return resp.StatusCode == 429 || resp.StatusCode >= 500
}

func extractFallbacks(req *http.Request, cfg provider.Config) (string, []string) {
	rc, ok := req.Context().Value(routeContextKey{}).(*RouteContext)
	if !ok || rc.ProviderName == "" {
		return "", nil
	}
	name := rc.ProviderName
	p, ok := cfg.Providers[name]
	if !ok {
		return name, nil
	}

	fallbacks := p.Fallback

	// Also check plugins.config.failover for allowed_fallbacks.
	if failoverCfg, hasCfg := cfg.Plugins.Config["failover"]; hasCfg {
		var fc struct {
			AllowedFallbacks []string `json:"allowed_fallbacks"`
		}
		if err := json.Unmarshal(failoverCfg, &fc); err == nil && len(fc.AllowedFallbacks) > 0 {
			fallbacks = fc.AllowedFallbacks
		}
	}

	if len(fallbacks) == 0 {
		return name, nil
	}
	return name, fallbacks
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
