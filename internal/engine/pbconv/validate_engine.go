package pbconv

import (
	"fmt"
	"math"

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
		if err := engine.ValidateMessage(&c.Messages[i]); err != nil {
			return fmt.Errorf("message %d: %w", i, err)
		}
	}
	for i := range c.Tools {
		// The SHARED rule, exactly: the SDK's replacement validator rejects
		// an empty name (name == ""), so the accepted-input side must use
		// the same predicate — a whitespace-only name is either valid on
		// both sides or invalid on both sides, never host-rejected and
		// plugin-accepted. (The SDK table is the single normative statement;
		// Edge does not invent stricter host-only rules.)
		if c.Tools[i].Name == "" {
			return fmt.Errorf("tool %d: empty name", i)
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
	out := toPBChatRequest(c)
	if err := out.ValidateReplacement(); err != nil {
		return nil, fmt.Errorf("engine request fails the replacement contract: %w", err)
	}
	return out, nil
}
