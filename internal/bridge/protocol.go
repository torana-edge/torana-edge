// Package bridge translates explicitly configured inference contracts through
// Torana's canonical representation. Native proxy routes remain byte preserving.
package bridge

import (
	"fmt"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// Protocol identifies an API contract, including variants sharing a provider.
type Protocol string

const (
	OpenAIChat       Protocol = "openai-chat"
	OpenAIResponses  Protocol = "openai-responses"
	Anthropic        Protocol = "anthropic"
	Gemini           Protocol = "gemini"
	GeminiCodeAssist Protocol = "gemini-codeassist"
)

func (p Protocol) Valid() bool { return p.Format() != "" }

func (p Protocol) Format() string {
	switch p {
	case OpenAIChat, OpenAIResponses:
		return "openai"
	case Anthropic:
		return "anthropic"
	case Gemini:
		return "gemini"
	case GeminiCodeAssist:
		return "gemini-codeassist"
	default:
		return ""
	}
}

// ApplyTopology selects the destination's wire shape on an owned request copy.
func (p Protocol) ApplyTopology(chat *engine.ChatRequest) {
	chat.CodeAssist = p == GeminiCodeAssist
	chat.OpenAIVariant = engine.OpenAIChat
	if p == OpenAIResponses {
		chat.OpenAIVariant = engine.OpenAIResponses
	}
}

// MatchesPath classifies inference endpoints without interpreting auxiliary APIs.
func (p Protocol) MatchesPath(method, path string) bool {
	if method != "POST" {
		return false
	}
	path = strings.TrimSuffix(path, "/")
	switch p {
	case OpenAIChat:
		return strings.HasSuffix(path, "/chat/completions")
	case OpenAIResponses:
		return strings.HasSuffix(path, "/responses")
	case Anthropic:
		return strings.HasSuffix(path, "/messages")
	case Gemini, GeminiCodeAssist:
		return strings.HasSuffix(path, ":generateContent") || strings.HasSuffix(path, ":streamGenerateContent")
	default:
		return false
	}
}

// UnsupportedError names a host-owned feature category, never request values.
// Callers can return it as a client error without exposing prompts or credentials.
type UnsupportedError struct{ Feature string }

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("protocol translation does not support %s", e.Feature)
}

func unsupported(feature string) error { return &UnsupportedError{Feature: feature} }
