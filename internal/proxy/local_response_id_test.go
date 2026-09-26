package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/torana-edge/torana-edge/internal/annotate"
	"github.com/torana-edge/torana-edge/internal/secret"
)

func TestRewriteLocalPreviousResponseIDPreservesOtherBytes(t *testing.T) {
	signer, err := secret.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, real, input, want string
	}{
		{"real", "resp_provider", `{"model":"m","previous_response_id":%s,"opaque":  9007199254740993}`, `{"model":"m","previous_response_id":"resp_provider","opaque":  9007199254740993}`},
		{"empty first", "", `{"model":"m","previous_response_id":%s,"opaque":  9007199254740993}`, `{"model":"m","opaque":  9007199254740993}`},
		{"empty last", "", `{"model":"m","opaque":  9007199254740993,"previous_response_id":%s}`, `{"model":"m","opaque":  9007199254740993}`},
		{"only member", "", `{"previous_response_id":%s}`, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, err := annotate.EncodeLocalResponseID(signer, tc.real)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(id)
			body := []byte(strings.Replace(tc.input, "%s", string(encoded), 1))
			got, changed, err := rewriteLocalPreviousResponseID(body, signer)
			if err != nil || !changed || string(got) != tc.want {
				t.Fatalf("got %s, changed %v, err %v; want %s", got, changed, err, tc.want)
			}
		})
	}
}
