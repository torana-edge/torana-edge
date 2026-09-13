package plugin

import (
	"encoding/json"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"testing"
)

func TestHeaderInjectionPreservesLargeNumbers(t *testing.T) {
	r := &pbv1.ChatRequest{ToranaMetaJson: []byte(`{"id":9007199254740993,"nested":{"huge":1e999},"decimal":1.00}`)}
	injectRequestHeaders(r, map[string]any{"Authorization": "test"})
	var got map[string]json.RawMessage
	if err := json.Unmarshal(r.ToranaMetaJson, &got); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"id": "9007199254740993", "nested": `{"huge":1e999}`, "decimal": "1.00"} {
		if string(got[key]) != want {
			t.Fatalf("%s=%s want %s", key, got[key], want)
		}
	}
	if len(got[requestHeadersKey]) == 0 {
		t.Fatal("headers not injected")
	}
	r.ToranaMetaJson = []byte("null")
	injectRequestHeaders(r, map[string]any{})
}
