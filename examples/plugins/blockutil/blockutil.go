// Package blockutil provides ordered-body helpers for the wasm test
// fixtures: text reads use the wire-order projection; text writes replace
// the first text block.
package blockutil

import (
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

// TextBlocks wraps one text value as a single Text block.
func TextBlocks(text string) []*pb.RequestBlock {
	return []*pb.RequestBlock{{
		Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: text}},
	}}
}

// TextOf returns the concatenated text of every text block in wire order.
func TextOf(m *pb.Message) string {
	var out string
	for _, b := range m.Blocks {
		if t := b.GetText(); t != nil {
			out += t.Text
		}
	}
	return out
}

// SetText replaces the first text block's text (creating one when the
// message has none).
func SetText(m *pb.Message, text string) {
	for _, b := range m.Blocks {
		if t := b.GetText(); t != nil {
			t.Text = text
			return
		}
	}
	m.Blocks = append(m.Blocks, &pb.RequestBlock{
		Kind: &pb.RequestBlock_Text{Text: &pb.RequestTextBlock{Text: text}},
	})
}
