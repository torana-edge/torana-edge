package proxy

import (
	"net/http"
	"strings"
	"testing"
)

func TestApplyPluginResponseHeadersAllowsOnlyPresentationMetadata(t *testing.T) {
	headers := make(http.Header)
	err := applyPluginResponseHeaders(headers, []byte(`{
		"Content-Type":["text/html; charset=utf-8"],
		"Content-Language":["en"]
	}`))
	if err != nil {
		t.Fatalf("apply headers: %v", err)
	}
	if got := headers.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("content type = %q", got)
	}
	for _, forbidden := range []string{
		"Set-Cookie", "Content-Security-Policy", "X-Frame-Options",
		"Access-Control-Allow-Origin", "Content-Length", "Content-Encoding",
	} {
		encoded := []byte(`{"` + forbidden + `":["unsafe"]}`)
		if err := applyPluginResponseHeaders(make(http.Header), encoded); err == nil {
			t.Fatalf("%s was accepted", forbidden)
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
