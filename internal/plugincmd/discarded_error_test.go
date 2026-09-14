package plugincmd

import (
	"strings"
	"testing"
)

func TestLintRejectsDiscardedCheckedSDKErrors(t *testing.T) {
	for _, body := range []string{`sdk.BlockRequest(403,"blocked","no")`, `_ = sdk.CacheSet("k","v")`, `_, _, _ = sdk.CacheGet("k")`, `_, _ = sdk.HostCallExtension("torana_record_savings", nil)`} {
		src := `package main
import sdk "github.com/torana-edge/torana-plugin-sdk"
func main() {}
func init(){ ` + body + ` }`
		msgs := lintMessages(t, writePlugin(t, validManifest, src))
		found := false
		for _, m := range msgs {
			if strings.Contains(m, "discards its error") {
				found = true
			}
		}
		if !found {
			t.Fatalf("body %q not diagnosed: %v", body, msgs)
		}
	}
}

func TestLintAllowsVoidCallsAndHandledErrors(t *testing.T) {
	for _, body := range []string{`sdk.Log("log", 1)`, `sdk.EmitMetric("count", 1, 1, nil)`, `sdk.MustBlockRequest(403,"blocked","no")`, `_, _, err := sdk.CacheGet("k"); if err != nil { panic(err) }`} {
		source := `package main
import sdk "github.com/torana-edge/torana-plugin-sdk"
func main(){}
func init(){ ` + body + ` }`
		for _, message := range lintMessages(t, writePlugin(t, validManifest, source)) {
			if strings.Contains(message, "discards its error") {
				t.Fatalf("valid call %q flagged: %s", body, message)
			}
		}
	}
}
