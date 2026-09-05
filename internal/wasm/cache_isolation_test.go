package wasm

import (
	"context"
	"math"
	"testing"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/proto"
)

func cacheSetCall(t *testing.T, r *Runtime, p *Plugin, cmd, key, value string) *pbv1.HostCallResult {
	t.Helper()
	raw, err := proto.Marshal(&pbv1.CacheSetArgs{Key: key, Value: value})
	if err != nil {
		t.Fatal(err)
	}
	return hostCallDirect(t, r, p, cmd, raw)
}

func cacheGetCall(t *testing.T, r *Runtime, p *Plugin, cmd, key string) *pbv1.HostCallResult {
	t.Helper()
	raw, err := proto.Marshal(&pbv1.CacheGetArgs{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	return hostCallDirect(t, r, p, cmd, raw)
}

func cacheValue(t *testing.T, result *pbv1.HostCallResult) string {
	t.Helper()
	arm, ok := result.Result.(*pbv1.HostCallResult_Value)
	if !ok {
		t.Fatalf("cache call failed: %+v", result.Result)
	}
	return string(arm.Value)
}

func TestPrivateAndSharedCacheDomainsAreEnforcedByTheHost(t *testing.T) {
	r := NewRuntime(context.Background())
	defer r.Close()
	load := func(name string, grants ...string) *Plugin {
		p, err := r.LoadPlugin(name, MinimalModule(false))
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		p.SetGrants(grants)
		return p
	}
	a := load("alpha", "env.cache_get", "env.cache_set", "env.shared_cache_get", "env.shared_cache_set")
	b := load("beta", "env.cache_get", "env.cache_set", "env.shared_cache_get", "env.shared_cache_set")

	cacheValue(t, cacheSetCall(t, r, a, "env.cache_set", "same", "alpha-private"))
	if _, ok := cacheGetCall(t, r, b, "env.cache_get", "same").Result.(*pbv1.HostCallResult_Error); !ok {
		t.Fatal("beta read alpha's private cache entry")
	}
	cacheValue(t, cacheSetCall(t, r, b, "env.cache_set", "same", "beta-private"))
	if got := cacheValue(t, cacheGetCall(t, r, a, "env.cache_get", "same")); got != "alpha-private" {
		t.Fatalf("alpha private value = %q", got)
	}

	cacheValue(t, cacheSetCall(t, r, a, "env.shared_cache_set", "intent:call", "shared"))
	if got := cacheValue(t, cacheGetCall(t, r, b, "env.shared_cache_get", "intent:call")); got != "shared" {
		t.Fatalf("explicit shared value = %q", got)
	}

	// Guest keys cannot escape the host-owned cache domain framing.
	attack := privateCacheKey("alpha", "same")
	cacheValue(t, cacheSetCall(t, r, b, "env.shared_cache_set", attack, "poison"))
	if got := cacheValue(t, cacheGetCall(t, r, a, "env.cache_get", "same")); got != "alpha-private" {
		t.Fatalf("shared key poisoned private domain: %q", got)
	}
	cacheValue(t, cacheSetCall(t, r, b, "env.cache_set", sharedCacheKey("intent:call"), "poison"))
	if got := cacheValue(t, cacheGetCall(t, r, a, "env.shared_cache_get", "intent:call")); got != "shared" {
		t.Fatalf("private key poisoned shared domain: %q", got)
	}
}

func TestPrivateCacheIsScopedToApprovedResources(t *testing.T) {
	r := NewRuntime(context.Background())
	defer r.Close()
	p, err := r.LoadPlugin("scanner", MinimalModule(false))
	if err != nil {
		t.Fatal(err)
	}
	p.SetGrants([]string{"env.cache_get", "env.cache_set"})
	resources := PluginResources{ModelServices: map[string]ModelServiceResource{
		"scanner": {Name: "scanner", Provider: "local", Model: "model-a", Path: "/v1/chat"},
	}}
	p.SetResources(resources)
	cacheValue(t, cacheSetCall(t, r, p, "env.cache_set", "clean", "model-a-verdict"))

	resources.ModelServices["scanner"] = ModelServiceResource{Name: "scanner", Provider: "local", Model: "model-b", Path: "/v1/chat"}
	p.SetResources(resources)
	miss := cacheGetCall(t, r, p, "env.cache_get", "clean")
	if arm, ok := miss.Result.(*pbv1.HostCallResult_Error); !ok || arm.Error.Code != pbv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("resource rebind reused old cache entry: %+v", miss.Result)
	}
	cacheValue(t, cacheSetCall(t, r, p, "env.cache_set", "clean", "model-b-verdict"))
	p.SetResources(resources)
	if got := cacheValue(t, cacheGetCall(t, r, p, "env.cache_get", "clean")); got != "model-b-verdict" {
		t.Fatalf("unchanged resource snapshot lost cache entry: %q", got)
	}

	nan := math.NaN()
	p.SetResources(PluginResources{PricingResources: map[string]PricingResource{
		"invalid": {Name: "invalid", Prices: map[string]*pbv1.ModelPricing{PricingCoordinate("p", "m"): {InputUsdPerMtok: &nan}}},
	}})
	invalid := cacheGetCall(t, r, p, "env.cache_get", "clean")
	if arm, ok := invalid.Result.(*pbv1.HostCallResult_Error); !ok || arm.Error.Code != pbv1.ErrorCode_ERROR_CODE_INTERNAL {
		t.Fatalf("invalid resource graph did not fail cache access closed: %+v", invalid.Result)
	}
}

func TestSharedCacheRequiresItsOwnGrant(t *testing.T) {
	r, p := newGrantedPlugin(t, "env.cache_get", "env.cache_set")
	for _, call := range []*pbv1.HostCallResult{
		cacheSetCall(t, r, p, "env.shared_cache_set", "k", "v"),
		cacheGetCall(t, r, p, "env.shared_cache_get", "k"),
	} {
		errArm, ok := call.Result.(*pbv1.HostCallResult_Error)
		if !ok || errArm.Error.Code != pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
			t.Fatalf("private cache grant opened shared cache: %+v", call.Result)
		}
	}
}
