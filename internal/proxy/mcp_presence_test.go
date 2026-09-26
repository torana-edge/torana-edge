package proxy

import (
	"github.com/torana-edge/torana-edge/internal/bridge"
	"github.com/torana-edge/torana-edge/internal/mcpserver"
	"strings"
	"testing"
)

func TestMCPToolPresenceAcrossRequestShapes(t *testing.T) {
	for _, shape := range bridgeProtocols {
		for _, name := range []string{"torana_search", "mcp__torana__torana_search", "mcp__other__torana_search", "weather"} {
			t.Run(string(shape)+"/"+name, func(t *testing.T) {
				body := strings.ReplaceAll(bridgeClientBody(shape, false, false), `"name":"weather"`, `"name":"`+name+`"`)
				path, _, _ := bridge.Endpoint(shape, "client-model", false)
				chat, err := bridge.ParseRequest(shape, []byte(body), path)
				if err != nil {
					t.Fatal(err)
				}
				want := "absent"
				if name == "torana_search" || name == "mcp__torana__torana_search" {
					want = "present"
				}
				if got := mcpserver.ToolPresence(chat, []string{"torana"}); got != want {
					t.Fatalf("got %s, want %s", got, want)
				}
			})
		}
	}
}
