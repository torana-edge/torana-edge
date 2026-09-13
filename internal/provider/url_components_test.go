package provider

import "testing"

func TestProviderURLRejectsIgnoredComponents(t *testing.T) {
	for _, suffix := range []string{"?api-version=example", "?", "#fragment"} {
		c := Config{Port: 8080, Providers: map[string]Provider{"p": {URL: "https://example.invalid/base" + suffix}}}
		if err := c.Validate(); err == nil {
			t.Fatalf("accepted ignored URL component %q", suffix)
		}
	}
	c := Config{Port: 8080, Providers: map[string]Provider{"p": {URL: "https://example.invalid/base"}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
