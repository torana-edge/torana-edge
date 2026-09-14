package wasm

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/proto"
)

func TestABICompleteStaticSurface(t *testing.T) {
	for _, name := range []string{"memory", "alloc", "dealloc", "run_hook", "supported_hooks", "abi_version"} {
		t.Run("missing_"+name, func(t *testing.T) {
			raw := bytes.Replace(MinimalModule(false), []byte(name), []byte(strings.Repeat("x", len(name))), 1)
			r := NewRuntime(context.Background())
			defer r.Close()
			if _, err := r.LoadPlugin("invalid", raw); err == nil {
				t.Fatalf("missing %s accepted", name)
			}
		})
	}
	// Repoint function exports at a differently typed, otherwise valid function.
	for _, tc := range []struct {
		name     string
		old, new byte
	}{{"alloc", 0, 1}, {"run_hook", 1, 0}, {"supported_hooks", 2, 4}, {"dealloc", 3, 1}, {"abi_version", 4, 2}} {
		t.Run("signature_"+tc.name, func(t *testing.T) {
			from := append(append([]byte{byte(len(tc.name))}, []byte(tc.name)...), 0, tc.old)
			to := append(append([]byte{}, from[:len(from)-1]...), tc.new)
			raw := bytes.Replace(MinimalModule(false), from, to, 1)
			r := NewRuntime(context.Background())
			defer r.Close()
			if _, err := r.LoadPlugin("invalid", raw); err == nil || !strings.Contains(err.Error(), "signature") {
				t.Fatalf("wrong %s signature result: %v", tc.name, err)
			}
		})
	}
}

// replaceABIBody keeps the module valid while changing one function's behavior.
func replaceABIBody(t *testing.T, module []byte, index int, body []byte) []byte {
	t.Helper()
	out := append([]byte{}, module[:8]...)
	for remaining := module[8:]; len(remaining) > 0; {
		id := remaining[0]
		size, n := binary.Uvarint(remaining[1:])
		if n <= 0 {
			t.Fatal("invalid section")
		}
		payload := remaining[1+n : 1+n+int(size)]
		remaining = remaining[1+n+int(size):]
		if id == 10 {
			count, m := binary.Uvarint(payload)
			codes := payload[m:]
			replacement := binary.AppendUvarint(nil, count)
			for i := 0; i < int(count); i++ {
				length, k := binary.Uvarint(codes)
				one := codes[k : k+int(length)]
				codes = codes[k+int(length):]
				if i == index {
					one = body
				}
				replacement = binary.AppendUvarint(replacement, uint64(len(one)))
				replacement = append(replacement, one...)
			}
			payload = replacement
		}
		out = append(out, id)
		out = binary.AppendUvarint(out, uint64(len(payload)))
		out = append(out, payload...)
	}
	return out
}
func TestABIRejectsRevisionAndBitmapMismatch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		index int
		body  []byte
	}{{"revision", 4, []byte{0, 0x42, 0x82, 0x80, 0x80, 0x80, 0x10, 0x0b}}, {"unknown_hooks", 2, []byte{0, 0x41, 0xc0, 0, 0x0b}}} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRuntime(context.Background())
			defer r.Close()
			if _, err := r.LoadPlugin("invalid", replaceABIBody(t, MinimalModule(false), tc.index, tc.body)); err == nil {
				t.Fatal("incompatible artifact accepted")
			}
		})
	}
}
func TestABIDeallocationFailuresAreErrors(t *testing.T) {
	for _, oob := range []bool{false, true} {
		t.Run(map[bool]string{false: "after_hook", true: "input_write_failed"}[oob], func(t *testing.T) {
			raw := replaceABIBody(t, MinimalModule(false), 3, []byte{0, 0, 0x0b})
			if oob {
				raw = replaceABIBody(t, raw, 0, []byte{0, 0x41, 0xff, 0xff, 0x03, 0x0b})
			}
			r := NewRuntime(context.Background())
			defer r.Close()
			p, err := r.LoadPlugin("trap-dealloc", raw)
			if err != nil {
				t.Fatal(err)
			}
			input, err := proto.Marshal(&pb.HookInput{ContractRevision: sdk.ContractRevision, RequestId: 1, Payload: &pb.HookInput_ChatRequest{ChatRequest: &pb.ChatRequest{Model: "m"}}})
			if err != nil {
				t.Fatal(err)
			}
			var out []byte
			if err = p.CallRequest(context.Background(), pb.Hook_HOOK_BEFORE_REQUEST, 1, input, &out); err == nil || !strings.Contains(err.Error(), "dealloc") {
				t.Fatalf("deallocation trap silently accepted: %v", err)
			}
		})
	}
}

func TestABIExecutionInfoUsesEffectiveLimitsAndDeadline(t *testing.T) {
	// Return the exact input bytes so the test can inspect the host-owned envelope.
	echo := replaceABIBody(t, MinimalModule(false), 1, []byte{0, 0x20, 0, 0xad, 0x42, 32, 0x86, 0x20, 1, 0xad, 0x84, 0x0b})
	for _, callback := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_callback", true: "callback"}[callback], func(t *testing.T) {
			r := NewRuntimeWithOptions(context.Background(), RuntimeOptions{MemoryLimitPages: 2, MaxHostResponseBytes: 321, MaxStreamBufferBytes: 765, CallTimeout: time.Second})
			defer r.Close()
			shared := &pb.ExecutionInfo{Provider: "bound", Model: "model", MaxMemoryBytes: 1, DeadlineUnixMs: proto.Int64(1)}
			if callback {
				r.ExecutionInfoFunc = func(context.Context) *pb.ExecutionInfo { return shared }
			}
			p, err := r.LoadPlugin("echo", echo)
			if err != nil {
				t.Fatal(err)
			}
			input, err := proto.Marshal(&pb.HookInput{ContractRevision: sdk.ContractRevision, RequestId: 7, Execution: &pb.ExecutionInfo{Provider: "forged"}, Payload: &pb.HookInput_ChatRequest{ChatRequest: &pb.ChatRequest{Model: "m"}}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			var out []byte
			if err = p.CallRequest(ctx, pb.Hook_HOOK_BEFORE_REQUEST, 7, input, &out); err != nil {
				t.Fatal(err)
			}
			envelope, err := pb.DecodeHookInput(out)
			if err != nil {
				t.Fatal(err)
			}
			info := envelope.Execution
			deadline, _ := ctx.Deadline()
			if info == nil || info.MaxMemoryBytes != 2*65536 || info.MaxHostResponseBytes != 321 || info.MaxStreamBufferBytes != 765 || info.GetDeadlineUnixMs() != deadline.UnixMilli() {
				t.Fatalf("incorrect effective execution info: %v", info)
			}
			if callback {
				if info.Provider != "bound" || shared.MaxMemoryBytes != 1 || shared.GetDeadlineUnixMs() != 1 {
					t.Fatal("callback not cloned")
				}
			} else if info.Provider != "" {
				t.Fatal("untrusted input execution survived")
			}
		})
	}
}
