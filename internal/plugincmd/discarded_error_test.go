package plugincmd

import (
	"strings"
	"testing"
)

func TestLintRejectsDiscardedCheckedSDKErrors(t *testing.T) {
	for _, body := range []string{`sdk.BlockRequest()`, `_ = sdk.CacheSet(nil,"k","v")`} {
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
