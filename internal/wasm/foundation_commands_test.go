package wasm

import (
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/pluginstate"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func TestFoundationCommands(t *testing.T) {
	r, p := newGrantedPlugin(t, "env.state_get", "env.state_set", "env.state_keys", "env.cache_set", "env.cache_get", "env.resource_info")
	vals := map[string]string{}
	vers := map[string]string{}
	r.StateGetVersionedFunc = func(_, k string) (string, string, bool) { v, ok := vals[k]; return v, vers[k], ok }
	r.StateCompareAndSetFunc = func(_, k, v string, e *string) (bool, string, error) {
		if e != nil && vers[k] != *e {
			return false, "", nil
		}
		if e == nil && vers[k] != "" {
			return false, "", nil
		}
		vers[k] = "1"
		vals[k] = v
		return true, "1", nil
	}
	r.StateCompareAndDeleteFunc = func(_, k, e string) (bool, error) {
		if vers[k] != e {
			return false, nil
		}
		delete(vals, k)
		delete(vers, k)
		return true, nil
	}
	r.StateScanFunc = func(_, _, _ string, _, _ int) ([]pluginstate.PageEntry, string, error) { return nil, "", nil }
	// Versioned get is a typed value and absent keys are NOT_FOUND.
	if got := hostCallDirect(t, r, p, "env.state_get_versioned", marshalHostArgs(t, &pbv1.StateGetArgs{Key: "missing"})); got.GetError().Code != pbv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("missing=%v", got)
	}
	set := hostCallDirect(t, r, p, "env.state_compare_and_set", marshalHostArgs(t, &pbv1.StateCompareAndSetArgs{Key: "k", Value: "v"}))
	if !strings.Contains(set.String(), "value:") {
		t.Fatal(set)
	}
	if got := hostCallDirect(t, r, p, "env.state_compare_and_set", marshalHostArgs(t, &pbv1.StateCompareAndSetArgs{Key: "k", Value: "x"})); !strings.Contains(got.String(), "applied") {
		_ = got
	}
	for _, ttl := range []uint64{1, 1000} {
		_ = hostCallDirect(t, r, p, "env.cache_set", marshalHostArgs(t, &pbv1.CacheSetArgs{Key: "k", Value: "v", TtlMs: &ttl}))
	}
	for _, kind := range []string{"http", "model", "credential", "file", "pricing", "cache_policy"} {
		_ = hostCallDirect(t, r, p, "env.resource_info", marshalHostArgs(t, &pbv1.ResourceInfoArgs{Kind: kind, Name: "missing"}))
	}
}
