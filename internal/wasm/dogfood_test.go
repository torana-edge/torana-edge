package wasm
import ("context";"encoding/json";"os";"testing")

func TestDelegator(t *testing.T) {
	b, _ := os.ReadFile("../../plugins/delegator/plugin.wasm")
	r := NewRuntime(context.Background()); defer r.Close()
	p, _ := r.LoadPlugin("delegator", b)
	
	// New plugin wraps chat JSON — test with a wrapper that has chat
	input := map[string]any{"chat": `{"Model":""}`, "test": 1}
	var out map[string]any
	if err := p.CallRequest(context.Background(), "on_chat_request", input, &out); err != nil {
		t.Fatal(err)
	}
	b2, _ := json.Marshal(out)
	t.Logf("output: %s", b2)
	
	// Plugin should inject default model into the chat JSON
	chatStr, _ := out["chat"].(string)
	if chatStr == "" {
		t.Fatal("no chat in output")
	}
	var chat map[string]any
	json.Unmarshal([]byte(chatStr), &chat)
	if chat["Model"] != "claude-3-5-sonnet-20241022" {
		t.Fatalf("expected model injection, got %v", chat["Model"])
	}
	t.Log("delegator OK")
}
