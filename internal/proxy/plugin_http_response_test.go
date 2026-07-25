package proxy

import (
	"net/http"
	"strings"
	"testing"
)

func TestApplyPluginResponseHeadersFiltersSecurityAndCookieHeaders(t *testing.T) {
	headers := make(http.Header)
	err := applyPluginResponseHeaders(headers, []byte(`{
		"Content-Type":["text/html; charset=utf-8"],
		"Set-Cookie":["session=secret"],
		"Content-Security-Policy":["default-src *"],
		"X-Frame-Options":["ALLOWALL"]
	}`))
	if err != nil {
		t.Fatalf("apply headers: %v", err)
	}
	if got := headers.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("content type = %q", got)
	}
	for _, forbidden := range []string{"Set-Cookie", "Content-Security-Policy", "X-Frame-Options"} {
		if got := headers.Get(forbidden); got != "" {
			t.Fatalf("%s escaped filter: %q", forbidden, got)
		}
	}
}

func TestApplyPluginResponseHeadersRejectsInvalidAndOversizedValues(t *testing.T) {
	if err := applyPluginResponseHeaders(make(http.Header), []byte(`{"Bad Header":["x"]}`)); err == nil {
		t.Fatal("invalid header name was accepted")
	}
	oversized := `{"X-Plugin":["` + strings.Repeat("x", maxPluginResponseHeaderBytes+1) + `"]}`
	if err := applyPluginResponseHeaders(make(http.Header), []byte(oversized)); err == nil {
		t.Fatal("oversized headers were accepted")
	}
}
