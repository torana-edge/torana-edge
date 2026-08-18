package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/auditlog"
	"github.com/torana-edge/torana-edge/internal/engine"
	"github.com/torana-edge/torana-edge/internal/provider"
)

func TestCaptureAuditRequestUsesOrderedToolCallsAndStringIntentOnly(t *testing.T) {
	args := func(raw string) engine.RequiredJSONObject {
		out, err := engine.ParseRequiredJSONObject([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	rs := &reqState{Intercepted: true}
	rs.captureAuditRequest(&engine.ChatRequest{
		Model: "m-1",
		Messages: []engine.Message{
			{Role: engine.RoleAssistant, Blocks: []engine.Block{
				{Text: &engine.TextBlock{Text: "before"}},
				{ToolUse: &engine.ToolUseBlock{ID: "c-1", Name: "read", Arguments: args(`{"path":"a","i":"inspect retries"}`)}},
			}},
			{Role: engine.RoleAssistant, Blocks: []engine.Block{
				{ToolUse: &engine.ToolUseBlock{ID: "c-2", Name: "grep", Arguments: args(`{"i":{"not":"text"}}`)}},
			}},
		},
	})
	want := []auditlog.ToolCall{
		{ID: "c-1", Name: "read", Intent: "inspect retries"},
		{ID: "c-2", Name: "grep"},
	}
	if !reflect.DeepEqual(rs.AuditToolCalls, want) {
		t.Fatalf("tool calls = %#v, want %#v", rs.AuditToolCalls, want)
	}
	if rs.InitialModel != "m-1" || rs.Model != "m-1" {
		t.Fatalf("models = %q/%q", rs.InitialModel, rs.Model)
	}

	rs.captureAuditRequest(&engine.ChatRequest{Model: "m-2", Messages: []engine.Message{{
		Role: engine.RoleAssistant, Blocks: []engine.Block{{ToolUse: &engine.ToolUseBlock{
			ID: "c-3", Name: "write", Arguments: args(`{}`),
		}}},
	}}})
	if rs.InitialModel != "m-1" || rs.Model != "m-2" {
		t.Fatalf("updated models = %q/%q", rs.InitialModel, rs.Model)
	}
	if len(rs.AuditToolCalls) != 1 || rs.AuditToolCalls[0].ID != "c-3" {
		t.Fatalf("updated calls = %#v", rs.AuditToolCalls)
	}
}

func TestAuditRecordIsDefensiveAndUsesBoundedErrorClass(t *testing.T) {
	rs := &reqState{
		ID:                        7,
		Start:                     time.Date(2026, 8, 18, 1, 2, 3, 4, time.UTC),
		inboundBody:               []byte("caller"),
		Intercepted:               true,
		InitialProvider:           "a",
		Provider:                  "b",
		InitialFormat:             "openai",
		Path:                      "/v1/chat/completions",
		InitialModel:              "m1",
		Model:                     "m2",
		AuditUpstreamRequestBytes: 9,
		AuditToolCalls:            []auditlog.ToolCall{{ID: "c", Name: "read"}},
	}
	plugins := []string{"intent"}
	record := rs.auditRecord(httpStatusTooManyRequests, plugins)
	plugins[0] = "tampered"
	rs.AuditToolCalls[0].ID = "tampered"
	if record.ErrorCode != "upstream_error" || record.Plugins[0] != "intent" || record.ToolCalls[0].ID != "c" {
		t.Fatalf("record aliased input or error class drifted: %#v", record)
	}
	b, err := json.Marshal(record)
	if err != nil || !json.Valid(b) {
		t.Fatalf("record JSON = %q, %v", b, err)
	}
}

func TestAuditRecordCarriesEveryHostAttributedVerdict(t *testing.T) {
	for _, verdict := range []string{"block", "respond", "route", "host_error"} {
		t.Run(verdict, func(t *testing.T) {
			rs := &reqState{
				Intercepted:    true,
				Start:          time.Unix(1, 0),
				Verdict:        verdict,
				VerdictPlugin:  "policy",
				AuditErrorCode: verdict + "_code",
			}
			record := rs.auditRecord(422, []string{"observer", "policy"})
			if record.Verdict != verdict || record.VerdictPlugin != "policy" ||
				record.ErrorCode != verdict+"_code" ||
				!reflect.DeepEqual(record.Plugins, []string{"observer", "policy"}) {
				t.Fatalf("record = %#v", record)
			}
		})
	}
}

func TestAuditRecordDoesNotHideLatePluginFailureBehindHTTP200(t *testing.T) {
	rs := &reqState{Intercepted: true, Start: time.Unix(1, 0), PluginFailure: true}
	record := rs.auditRecord(http.StatusOK, []string{"filter"})
	if !record.PluginFailure || record.ErrorCode != "plugin_failure" || record.Status != http.StatusOK {
		t.Fatalf("late plugin failure record = %#v", record)
	}

	// An upstream error remains the response outcome; the separate boolean
	// still reports that an observational plugin also failed.
	record = rs.auditRecord(http.StatusBadGateway, []string{"observer"})
	if !record.PluginFailure || record.ErrorCode != "upstream_error" {
		t.Fatalf("upstream + observational plugin failure record = %#v", record)
	}
}

const httpStatusTooManyRequests = 429

func TestPreserveAuditConfigOnlyWhenMemberOmitted(t *testing.T) {
	current := provider.Config{Audit: &auditlog.Config{Enabled: true, Path: "/old.jsonl"}}
	for _, tc := range []struct {
		name string
		body string
		want *auditlog.Config
	}{
		{"omitted", `{"port":8080}`, current.Audit},
		{"explicit null", `{"port":8080,"audit":null}`, nil},
		{"explicit replacement", `{"port":8080,"audit":{"enabled":true,"path":"/new.jsonl"}}`, &auditlog.Config{Enabled: true, Path: "/new.jsonl"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var incoming provider.Config
			if err := json.Unmarshal([]byte(tc.body), &incoming); err != nil {
				t.Fatal(err)
			}
			preserveAuditConfigIfOmitted([]byte(tc.body), &current, &incoming)
			if !reflect.DeepEqual(incoming.Audit, tc.want) {
				t.Fatalf("audit = %#v, want %#v", incoming.Audit, tc.want)
			}
		})
	}
}

func TestAuditReconfigurationKeepsLastKnownGoodWriter(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.jsonl")
	providers := testProviderConfig("http://127.0.0.1:1", "test", "openai")
	providers.Port = 8080
	providers.Audit = &auditlog.Config{Enabled: true, Path: oldPath}
	srv, err := New(Config{Port: "0", ConfigPath: filepath.Join(dir, "config.json"), Providers: providers})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(t.Context())

	appendRecord := func(id uint64) {
		srv.appendAudit(&reqState{ID: id, Intercepted: true, Start: time.Unix(int64(id), 0)}, 200, nil)
	}
	appendRecord(1)

	current := srv.GetConfig().Providers
	next := current
	newPath := filepath.Join(dir, "new.jsonl")
	next.Audit = &auditlog.Config{Enabled: true, Path: newPath}
	if err := srv.applyProviderConfigTransaction(current, next); err != nil {
		t.Fatal(err)
	}
	appendRecord(2)

	unsafePath := filepath.Join(dir, "unsafe.jsonl")
	if err := os.WriteFile(unsafePath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	bad := next
	bad.Audit = &auditlog.Config{Enabled: true, Path: unsafePath}
	if err := srv.applyProviderConfigTransaction(next, bad); err == nil {
		t.Fatal("unsafe replacement sink accepted")
	}
	appendRecord(3)

	// Opening a candidate is not enough to make it live. If the durable config
	// save fails after that open, requests must continue on the last known-good
	// writer and the unused candidate must be closed.
	thirdPath := filepath.Join(dir, "third.jsonl")
	third := next
	third.Audit = &auditlog.Config{Enabled: true, Path: thirdPath}
	srv.configPath = dir // provider.Save cannot atomically replace a directory.
	if err := srv.applyProviderConfigTransaction(next, third); err == nil {
		t.Fatal("audit replacement survived a failed config persistence")
	}
	appendRecord(4)

	readIDs := func(path string) []uint64 {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		var ids []uint64
		s := bufio.NewScanner(f)
		for s.Scan() {
			var record auditlog.Record
			if err := json.Unmarshal(s.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, record.RequestID)
		}
		if err := s.Err(); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	if got := readIDs(oldPath); !reflect.DeepEqual(got, []uint64{1}) {
		t.Fatalf("old sink IDs = %v", got)
	}
	if got := readIDs(newPath); !reflect.DeepEqual(got, []uint64{2, 3, 4}) {
		t.Fatalf("new sink IDs = %v; failed reconfiguration displaced it", got)
	}
	if info, err := os.Stat(thirdPath); err != nil || info.Size() != 0 {
		t.Fatalf("unused candidate sink = (%v, %v), want closed empty file", info, err)
	}
}

func TestAuditAppendFailureIsCountedExactlyAndLoggedWithoutValues(t *testing.T) {
	dir := t.TempDir()
	providers := testProviderConfig("http://127.0.0.1:1", "test", "openai")
	providers.Audit = &auditlog.Config{Enabled: true, Path: filepath.Join(dir, "audit.jsonl")}
	srv, err := New(Config{Port: "0", ConfigPath: filepath.Join(dir, "config.json"), Providers: providers})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(t.Context())

	srv.auditMu.Lock()
	if err := srv.auditWriter.Close(); err != nil {
		srv.auditMu.Unlock()
		t.Fatal(err)
	}
	srv.auditMu.Unlock()

	var logs bytes.Buffer
	oldWriter, oldFlags, oldPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(&logs)
	log.SetFlags(0)
	log.SetPrefix("")
	defer func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
	}()

	rs := &reqState{
		ID:             1,
		Start:          time.Unix(1, 0),
		Intercepted:    true,
		Provider:       "SECRET-provider",
		AuditErrorCode: "SECRET-error",
	}
	srv.appendAudit(rs, 500, []string{"SECRET-plugin"})
	srv.appendAudit(rs, 500, []string{"SECRET-plugin"})
	if got := srv.stats.Snapshot().AuditWriteFailures; got != 2 {
		t.Fatalf("audit write failures = %d, want 2", got)
	}
	if got := logs.String(); got != "audit: append failed for configured path\n" {
		t.Fatalf("rate-limited audit diagnostic = %q", got)
	}
	for _, secret := range []string{"SECRET-provider", "SECRET-error", "SECRET-plugin"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("audit diagnostic leaked %q: %q", secret, logs.String())
		}
	}
}
