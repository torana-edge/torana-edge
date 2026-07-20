package openai

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestResponsesOpaqueInputItemsRoundTripInOrder(t *testing.T) {
	original := []byte(`{
  "model":"gpt-5.4",
  "previous_response_id":"resp_previous",
  "store":false,
  "input":[
    {"type":"message","role":"user","content":"inspect this"},
    {"type":"reasoning","id":"rs_1","encrypted_content":"opaque-reasoning","summary":[{"type":"summary_text","text":"looked"}]},
    {"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.go\"}"},
    {"type":"compaction","id":"cmp_1","encrypted_content":"opaque-compaction","custom_future_field":{"nested":[1,2,3]}},
    {"type":"function_call_output","call_id":"call_1","output":"contents"}
  ]
}`)

	chat, err := (&Adapter{}).Unmarshal(original)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got["previous_response_id"] != "resp_previous" || got["store"] != false {
		t.Fatalf("top-level Responses fields changed: %s", encoded)
	}
	items, ok := got["input"].([]any)
	if !ok || len(items) != 5 {
		t.Fatalf("input = %#v, want five ordered items", got["input"])
	}
	wantTypes := []string{"message", "reasoning", "function_call", "compaction", "function_call_output"}
	for i, want := range wantTypes {
		item := items[i].(map[string]any)
		if item["type"] != want {
			t.Fatalf("item %d type = %v, want %s", i, item["type"], want)
		}
	}
	if gotReasoning := items[1].(map[string]any); gotReasoning["encrypted_content"] != "opaque-reasoning" {
		t.Fatalf("reasoning item changed: %#v", gotReasoning)
	}
	gotCompaction := items[3].(map[string]any)
	wantFuture := map[string]any{"nested": []any{float64(1), float64(2), float64(3)}}
	if gotCompaction["encrypted_content"] != "opaque-compaction" || !reflect.DeepEqual(gotCompaction["custom_future_field"], wantFuture) {
		t.Fatalf("compaction item changed: %#v", gotCompaction)
	}
}

func TestResponsesOpaqueItemsSurviveKnownMessageMutation(t *testing.T) {
	chat, err := (&Adapter{}).Unmarshal([]byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":"before"},{"type":"compaction","encrypted_content":"opaque"},{"type":"message","role":"user","content":"after"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	chat.Messages[0].Content = "changed"
	encoded, err := (&Adapter{}).Marshal(chat)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got.Input[0]["content"] != "changed" || got.Input[1]["type"] != "compaction" || got.Input[1]["encrypted_content"] != "opaque" || got.Input[2]["content"] != "after" {
		t.Fatalf("unexpected mutated round trip: %s", encoded)
	}
}
