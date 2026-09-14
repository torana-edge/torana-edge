package wasm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/cache"
	"github.com/torana-edge/torana-edge/internal/pluginstate"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/proto"
)

func foundationRefusal(t *testing.T, result *pb.HostCallResult, code pb.ErrorCode) {
	t.Helper()
	if result.GetError() == nil || result.GetError().Code != code {
		t.Fatalf("got %v, want refusal %s", result, code)
	}
}
func foundationDecode(t *testing.T, result *pb.HostCallResult, dest proto.Message) {
	t.Helper()
	raw := hostCallValue(t, result)
	if err := unmarshalClosed(raw, dest); err != nil {
		t.Fatal(err)
	}
}
func TestFoundationStateCommands(t *testing.T) {
	r, p := newGrantedPlugin(t, "env.state_get", "env.state_set", "env.state_keys")
	state, err := pluginstate.New(pluginstate.Options{Path: t.TempDir() + "/state.json"})
	if err != nil {
		t.Fatal(err)
	}
	r.StateGetVersionedFunc = state.GetVersioned
	r.StateCompareAndSetFunc = state.CompareAndSet
	r.StateCompareAndDeleteFunc = state.CompareAndDelete
	r.StateScanFunc = state.Scan
	call := func(cmd string, args proto.Message) *pb.HostCallResult {
		return hostCallDirect(t, r, p, cmd, marshalHostArgs(t, args))
	}
	foundationRefusal(t, call("env.state_get_versioned", &pb.StateGetArgs{Key: "missing"}), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	var created pb.StateMutationResult
	foundationDecode(t, call("env.state_compare_and_set", &pb.StateCompareAndSetArgs{Key: "k", Value: ""}), &created)
	if !created.Applied || created.GetVersion() == "" {
		t.Fatalf("create not applied: %v", &created)
	}
	var got pb.StateValue
	foundationDecode(t, call("env.state_get_versioned", &pb.StateGetArgs{Key: "k"}), &got)
	if got.Value != "" || got.Version != created.GetVersion() {
		t.Fatalf("empty value/version lost: %v", &got)
	}
	var conflict pb.StateMutationResult
	foundationDecode(t, call("env.state_compare_and_set", &pb.StateCompareAndSetArgs{Key: "k", Value: "bad"}), &conflict)
	if conflict.Applied || conflict.Version != nil {
		t.Fatalf("CAS conflict not represented: %v", &conflict)
	}
	var updated pb.StateMutationResult
	foundationDecode(t, call("env.state_compare_and_set", &pb.StateCompareAndSetArgs{Key: "k", Value: "new", ExpectedVersion: created.Version}), &updated)
	if !updated.Applied || updated.GetVersion() == created.GetVersion() {
		t.Fatalf("version not advanced: %v", &updated)
	}
	var deleted pb.StateMutationResult
	foundationDecode(t, call("env.state_compare_and_delete", &pb.StateCompareAndDeleteArgs{Key: "k", ExpectedVersion: created.GetVersion()}), &deleted)
	if deleted.Applied {
		t.Fatal("stale delete applied")
	}
	foundationDecode(t, call("env.state_compare_and_delete", &pb.StateCompareAndDeleteArgs{Key: "k", ExpectedVersion: updated.GetVersion()}), &deleted)
	if !deleted.Applied || deleted.Version != nil {
		t.Fatalf("delete result malformed: %v", &deleted)
	}
	foundationRefusal(t, call("env.state_scan", &pb.StateScanArgs{Limit: 257}), pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT)
	for _, key := range []string{"a", "b", "c"} {
		hostCallValue(t, call("env.state_compare_and_set", &pb.StateCompareAndSetArgs{Key: key, Value: key}))
	}
	var first, second pb.StateScanResult
	foundationDecode(t, call("env.state_scan", &pb.StateScanArgs{Limit: 2}), &first)
	if len(first.Entries) != 2 || first.NextCursor == "" {
		t.Fatalf("unbounded first page: %v", &first)
	}
	foundationDecode(t, call("env.state_scan", &pb.StateScanArgs{Limit: 2, Cursor: first.NextCursor}), &second)
	if len(second.Entries) != 1 || second.Entries[0].Key != "c" || second.NextCursor != "" {
		t.Fatalf("bad next page: %v", &second)
	}
	p.SetGrants(nil)
	foundationRefusal(t, call("env.state_compare_and_set", &pb.StateCompareAndSetArgs{Key: "denied"}), pb.ErrorCode_ERROR_CODE_PERMISSION_DENIED)
}

type foundationCache struct {
	cache.Store
	failure error
	ttl     time.Duration
}

func (c *foundationCache) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	c.ttl = ttl
	if c.failure != nil {
		return c.failure
	}
	return c.Store.Set(ctx, key, value, ttl)
}
func (c *foundationCache) Get(ctx context.Context, key string) (string, bool, error) {
	if c.failure != nil {
		return "", false, c.failure
	}
	return c.Store.Get(ctx, key)
}
func (c *foundationCache) Delete(ctx context.Context, key string) error {
	if c.failure != nil {
		return c.failure
	}
	return c.Store.Delete(ctx, key)
}
func TestFoundationCacheCommands(t *testing.T) {
	r, p := newGrantedPlugin(t, "env.cache_set", "env.cache_get")
	now := time.Unix(100, 0)
	store := &foundationCache{Store: cache.NewLocalCacheWithClock(time.Minute, 100, 10000, func() time.Time { return now })}
	r.cache = store
	call := func(cmd string, args proto.Message) *pb.HostCallResult {
		return hostCallDirect(t, r, p, cmd, marshalHostArgs(t, args))
	}
	ttl := uint64(1000)
	hostCallValue(t, call("env.cache_set", &pb.CacheSetArgs{Key: "k", Value: "", TtlMs: &ttl}))
	if store.ttl != time.Second {
		t.Fatalf("TTL ignored: %s", store.ttl)
	}
	if got := hostCallValue(t, call("env.cache_get", &pb.CacheGetArgs{Key: "k"})); len(got) != 0 {
		t.Fatal("empty cache value changed")
	}
	now = now.Add(time.Second)
	foundationRefusal(t, call("env.cache_get", &pb.CacheGetArgs{Key: "k"}), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	ttl = 0
	foundationRefusal(t, call("env.cache_set", &pb.CacheSetArgs{Key: "k", TtlMs: &ttl}), pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT)
	ttl = uint64((time.Hour).Milliseconds())
	foundationRefusal(t, call("env.cache_set", &pb.CacheSetArgs{Key: "k", TtlMs: &ttl}), pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT)
	hostCallValue(t, call("env.cache_set", &pb.CacheSetArgs{Key: "k", Value: "v"}))
	hostCallValue(t, call("env.cache_delete", &pb.CacheDeleteArgs{Key: "k"}))
	foundationRefusal(t, call("env.cache_get", &pb.CacheGetArgs{Key: "k"}), pb.ErrorCode_ERROR_CODE_NOT_FOUND)
	store.failure = errors.New("backend unavailable")
	for _, tc := range []struct {
		cmd  string
		args proto.Message
	}{{"env.cache_get", &pb.CacheGetArgs{Key: "k"}}, {"env.cache_set", &pb.CacheSetArgs{Key: "k"}}, {"env.cache_delete", &pb.CacheDeleteArgs{Key: "k"}}} {
		foundationRefusal(t, call(tc.cmd, tc.args), pb.ErrorCode_ERROR_CODE_INTERNAL)
	}
}

func TestFoundationResourceInfoAvailability(t *testing.T) {
	r, p := newGrantedPlugin(t, "env.resource_info")
	p.SetResources(PluginResources{Credentials: map[string]string{"key": "secret-credential"}, HTTP: map[string]HTTPResource{"api": {Origin: "https://secret-host", Methods: map[string]bool{"POST": true}, Timeout: time.Second, MaxRequestBytes: 100, MaxResponseBytes: 200}}, Files: map[string]FileResource{"logs/events.jsonl": {Operations: map[string]bool{"append": true}, MaxBytes: 123}}})
	call := func(kind, name string) *pb.HostCallResult {
		return hostCallDirect(t, r, p, "env.resource_info", marshalHostArgs(t, &pb.ResourceInfoArgs{Kind: kind, Name: name}))
	}
	for _, kind := range []string{"credential", "file", "http", "model", "pricing", "cache_policy"} {
		foundationRefusal(t, call(kind, "missing"), pb.ErrorCode_ERROR_CODE_PERMISSION_DENIED)
	}
	for _, tc := range []struct{ kind, name string }{{"credential", "key"}, {"file", "logs/events.jsonl"}, {"http", "api"}} {
		var info pb.ResourceInfo
		foundationDecode(t, call(tc.kind, tc.name), &info)
		if info.Available {
			t.Fatalf("missing handler reported available: %v", &info)
		}
	}
	r.HTTPRequestFunc = func(context.Context, string, HTTPResource, *pb.OutboundHTTPRequestArgs) (*pb.OutboundHTTPResponse, error) {
		return nil, nil
	}
	var info pb.ResourceInfo
	res := call("http", "api")
	foundationDecode(t, res, &info)
	if !info.Available || info.GetMaxInputBytes() != 100 || info.GetMaxOutputBytes() != 200 || info.GetTimeoutMs() != 1000 || len(info.Operations) != 1 || info.Operations[0] != "POST" {
		t.Fatalf("incorrect effective HTTP metadata: %v", &info)
	}
	if strings.Contains(string(res.GetValue()), "secret-") {
		t.Fatal("resource metadata leaked binding secrets")
	}
}

func TestFoundationCommandHookRestriction(t *testing.T) {
	r, p := newGrantedPlugin(t, "env.block_request")
	args := string(marshalHostArgs(t, &pb.BlockRequestArgs{Status: 403, Code: "denied", Message: "blocked"}))
	for _, hook := range []pb.Hook{pb.Hook_HOOK_AFTER_RESPONSE, pb.Hook_HOOK_ON_STREAM_CHUNK, pb.Hook_HOOK_ON_TICK, pb.Hook_HOOK_ON_HTTP_REQUEST} {
		raw := r.dispatchHostCall(context.WithValue(context.Background(), invocationHookKey{}, hook), p.name, "env.block_request", args)
		res, err := pb.DecodeHostCallResult(raw)
		if err != nil {
			t.Fatal(err)
		}
		foundationRefusal(t, res, pb.ErrorCode_ERROR_CODE_PERMISSION_DENIED)
	}
}

func TestFoundationSyntheticRefusalDoesNotRecordVerdict(t *testing.T) {
	r, p := newGrantedPlugin(t, "env.respond_request")
	r.ValidateSyntheticResponseFunc = func(context.Context, *pb.SyntheticResponse) *pb.HostError {
		return &pb.HostError{Code: pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, Message: "unrepresentable response"}
	}
	response := &pb.SyntheticResponse{Message: &pb.ResponseMessage{Blocks: []*pb.ResponseBlock{{Kind: &pb.ResponseBlock_Text{Text: &pb.ResponseTextBlock{Text: "text"}}}}}, FinishReason: "stop"}
	result := hostCallDirect(t, r, p, "env.respond_request", marshalHostArgs(t, &pb.RespondRequestArgs{Response: response}))
	foundationRefusal(t, result, pb.ErrorCode_ERROR_CODE_INVALID_ARGUMENT)
	if r.verdictsBucket(0).Respond() != nil {
		t.Fatal("refused response recorded as accepted verdict")
	}
}

func TestFoundationHostResponseByteLimit(t *testing.T) {
	r, p := newGrantedPlugin(t, "env.original_response")
	r.options.MaxHostResponseBytes = 3
	r.OriginalResponseFunc = func(context.Context) ([]byte, bool) { return []byte("oversized"), true }
	raw := r.dispatchHostCall(context.WithValue(context.Background(), invocationHookKey{}, pb.Hook_HOOK_AFTER_RESPONSE), p.name, "env.original_response", "")
	result, err := pb.DecodeHostCallResult(raw)
	if err != nil {
		t.Fatal(err)
	}
	foundationRefusal(t, result, pb.ErrorCode_ERROR_CODE_UNAVAILABLE)
}
