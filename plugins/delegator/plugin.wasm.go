package main

import (
	"encoding/json"
	"unsafe"
)

var heap [131072]byte
var bump uint32

//export alloc
func alloc(size uint32) uint32 {
	if bump+size > uint32(len(heap)) { return 0 }
	ptr := bump; bump += size; return ptr
}

//export dealloc
func dealloc(ptr, size uint32) {}

//export on_chat_request
func on_chat_request(ptr, size uint32) uint64 {
	return jsonRoundTrip(ptr, size, func(msg map[string]any) map[string]any {
		tools, _ := msg["tools"].([]any)
		for _, t := range tools {
			if m, ok := t.(map[string]any); ok && m["name"] == "torana_delegate_task" {
				return msg // already injected
			}
		}

		delegator := map[string]any{
			"name":        "torana_delegate_task",
			"description": "Delegate a sub-task to a cheaper local model",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{
					"task":      map[string]any{"type": "string", "description": "Task description"},
					"max_turns": map[string]any{"type": "integer", "description": "Max tool call turns"},
				},
				"required": []any{"task"},
			},
		}
		msg["tools"] = append(tools, delegator)
		return msg
	})
}

func jsonRoundTrip(ptr, size uint32, fn func(map[string]any) map[string]any) uint64 {
	input := heap[ptr : ptr+size]
	var msg map[string]any
	if err := json.Unmarshal(input, &msg); err != nil {
		return errorResult("unmarshal: " + err.Error())
	}
	msg = fn(msg)
	out, err := json.Marshal(msg)
	if err != nil {
		return errorResult("marshal: " + err.Error())
	}
	outPtr := alloc(uint32(len(out)))
	copy(heap[outPtr:], out)
	return uint64(outPtr)<<32 | uint64(len(out))
}

func errorResult(msg string) uint64 {
	b := []byte(`{"error":"` + msg + `"}`)
	p := alloc(uint32(len(b)))
	copy(heap[p:], b)
	return uint64(p)<<32 | uint64(len(b))
}

// No-op stubs for unused imports.
var _ = unsafe.Sizeof(0)

func main() {}
