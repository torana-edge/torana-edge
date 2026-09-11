package engine

import (
	"strings"
	"testing"
)

func TestMessageTextConcatenatesOnlyTextBlocksInWireOrder(t *testing.T) {
	message := &Message{Blocks: []Block{
		{Text: &TextBlock{Text: "first"}},
		{Thinking: &ThinkingBlock{Text: "not visible"}},
		{Text: &TextBlock{Text: ""}},
		{ToolUse: &ToolUseBlock{Name: "ignored"}},
		{Text: &TextBlock{Text: "-last"}},
	}}

	if got := message.Text(); got != "first-last" {
		t.Fatalf("Text() = %q, want %q", got, "first-last")
	}
	var nilMessage *Message
	if got := nilMessage.Text(); got != "" {
		t.Fatalf("nil Text() = %q, want empty", got)
	}
}

func BenchmarkMessageTextMultipart(b *testing.B) {
	const blockCount = 1000
	text := strings.Repeat("x", 1024)
	message := &Message{Blocks: make([]Block, blockCount)}
	for i := range message.Blocks {
		message.Blocks[i].Text = &TextBlock{Text: text}
	}
	b.ReportAllocs()
	b.SetBytes(blockCount * int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := len(message.Text()); got != blockCount*len(text) {
			b.Fatalf("Text length = %d, want %d", got, blockCount*len(text))
		}
	}
}
