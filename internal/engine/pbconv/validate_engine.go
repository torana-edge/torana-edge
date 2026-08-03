package pbconv

import (
	"fmt"
	"math"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// ValidateEngineRequest checks the ENGINE-side structural facts the
// protobuf oneof carries implicitly. The engine block representation is a
// pointer sum: zero/multi-arm states and nested conflicts are REFUSED here —
// the protobuf conversion silently picks the first arm, so validating the
// resulting PB cannot detect facts the conversion already discarded.
//
// The remaining absolute replacement rules (role/blocks presence, UTF-8,
// tool-use identity, JSON shapes, trailing-signature placement) are checked
// by the SDK validator on the converted PB — the checked boundary runs BOTH:
// ValidateEngineRequest first (the facts conversion would discard), then
// ValidateReplacement (the common domain). The host therefore never
// truncates or silently drops a fact: either the request is in the closed
// domain, or it fails before the first hook.
func ValidateEngineRequest(c *engine.ChatRequest) error {
	if c == nil {
		return fmt.Errorf("engine request is nil")
	}
	if c.MaxTokens != nil && (*c.MaxTokens < 1 || *c.MaxTokens > math.MaxInt32) {
		return fmt.Errorf("max_tokens %d is outside 1..%d", *c.MaxTokens, math.MaxInt32)
	}
	if c.Temperature != nil && (math.IsNaN(*c.Temperature) || math.IsInf(*c.Temperature, 0)) {
		return fmt.Errorf("temperature is not finite")
	}
	if c.TopP != nil && (math.IsNaN(*c.TopP) || math.IsInf(*c.TopP, 0)) {
		return fmt.Errorf("top_p is not finite")
	}
	for i := range c.Messages {
		if err := validateEngineMessage(&c.Messages[i]); err != nil {
			return fmt.Errorf("message %d: %w", i, err)
		}
	}
	for i := range c.Tools {
		if strings.TrimSpace(c.Tools[i].Name) == "" {
			return fmt.Errorf("tool %d: empty name", i)
		}
	}
	return nil
}

func validateEngineMessage(m *engine.Message) error {
	if m == nil {
		return fmt.Errorf("message is nil")
	}
	for j := range m.Blocks {
		if err := validateEngineBlock(&m.Blocks[j]); err != nil {
			return fmt.Errorf("block %d: %w", j, err)
		}
	}
	return nil
}

func validateEngineBlock(b *engine.Block) error {
	arms := 0
	if b.Text != nil {
		arms++
	}
	if b.Thinking != nil {
		arms++
	}
	if b.RedactedThinking != nil {
		arms++
	}
	if b.ToolUse != nil {
		arms++
	}
	if b.ToolResult != nil {
		arms++
	}
	if b.CacheBreakpoint != nil {
		arms++
	}
	if b.Unknown != nil {
		arms++
	}
	if b.TrailingSignature != nil {
		arms++
	}
	switch arms {
	case 0:
		return fmt.Errorf("block has zero arms")
	case 1:
		// exactly one arm: the ordered-body invariant
	default:
		return fmt.Errorf("block has %d arms, want exactly one", arms)
	}
	if b.ToolResult != nil {
		if len(b.ToolResult.Content) == 0 {
			return fmt.Errorf("tool result has empty nested content")
		}
		for k := range b.ToolResult.Content {
			c := &b.ToolResult.Content[k]
			nested := 0
			if c.Text != "" {
				nested++
			}
			if c.Unknown != nil {
				nested++
			}
			if c.CacheBreakpoint != nil {
				nested++
			}
			if nested > 1 {
				return fmt.Errorf("nested content element %d has %d conflicting arms", k, nested)
			}
		}
	}
	return nil
}

// ToPBChatRequestChecked is the SINGLE checked Engine->PB boundary: it
// refuses engine states the conversion would silently discard (arms,
// conflicts, floats, domain), converts, then runs the SDK's absolute
// replacement validator on the result. Every adapter/host path that hands an
// engine request to the wire goes through this (or through a PB that already
// passed ValidateReplacement).
func ToPBChatRequestChecked(c *engine.ChatRequest) (*pb.ChatRequest, error) {
	if err := ValidateEngineRequest(c); err != nil {
		return nil, fmt.Errorf("invalid engine request: %w", err)
	}
	out := ToPBChatRequest(c)
	if err := out.ValidateReplacement(); err != nil {
		return nil, fmt.Errorf("engine request fails the replacement contract: %w", err)
	}
	return out, nil
}
