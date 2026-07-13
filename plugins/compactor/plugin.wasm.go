package main

import (
	"strings"
	sdk "github.com/torana-edge/torana-edge/pkg/plugin-sdk"
	"github.com/torana-edge/torana-edge/pkg/pb"
)

func main() {}

func init() {
	sdk.OnChatRequest(func(req *pb.ChatRequest) (*pb.ChatRequest, error) {
		modified := false
		for _, m := range req.Messages {
			if m.Role == "tool" && len(m.Content) > 2000 {
				m.Content = compact(m.Content)
				modified = true
			}
		}

		if !modified {
			return nil, nil
		}

		return req, nil
	})
}

func compact(s string) string {
	lines := strings.Split(s, "\n")
	for _, l := range lines { _ = l }
	return s[:min(500, len(s))]
}
func min(a, b int) int { if a < b { return a }; return b }
