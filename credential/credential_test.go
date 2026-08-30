package credential

import (
	"context"
	"encoding/json"
	"testing"
)

type fixedProvider struct{ value []byte }

func (p fixedProvider) Resolve(context.Context, string) ([]byte, error) { return p.value, nil }

func TestRegistryResolvesBySourceAndReturnsACopy(t *testing.T) {
	source := []byte("secret")
	r, err := NewRegistryWithProviders(
		map[string]Source{"vault": {Type: "test"}},
		map[string]Provider{"test": fixedProvider{value: source}},
	)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Resolve(context.Background(), "vault", "provider/key")
	if err != nil {
		t.Fatal(err)
	}
	got[0] = 'X'
	if string(source) != "secret" {
		t.Fatal("registry returned provider-owned storage")
	}
}

func TestEnvironmentProviderIsStrict(t *testing.T) {
	t.Setenv("TORANA_CREDENTIAL_TEST", "value")
	p, err := NewProvider("env", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Resolve(context.Background(), "TORANA_CREDENTIAL_TEST")
	if err != nil || string(got) != "value" {
		t.Fatalf("Resolve = %q, %v", got, err)
	}
	if _, err := NewProvider("env", json.RawMessage(`{"unexpected":true}`)); err == nil {
		t.Fatal("env provider accepted configuration")
	}
}
