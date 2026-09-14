package bridge

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/torana-edge/torana-edge/internal/engine"
)

// Images cross APIs only when the data and interpretation can be preserved.
// Torana never fetches URLs or manufactures a provider file identifier.
func projectMedia(block *engine.UnknownBlock, from, to Protocol) (*engine.UnknownBlock, error) {
	if block.Signature != "" || !block.PartMetadataJson.IsAbsent() {
		return nil, unsupported("signed media or provider part metadata")
	}
	var imageURL, mime, data string
	switch from {
	case OpenAIChat:
		if block.Kind != "image_url" {
			return nil, unsupported("provider content blocks")
		}
		var payload struct {
			ImageURL struct {
				URL    string `json:"url"`
				Detail string `json:"detail,omitempty"`
			} `json:"image_url"`
		}
		if decodeClosed(block.Payload.Bytes(), &payload) != nil {
			return nil, unsupported("image fields")
		}
		if payload.ImageURL.Detail != "" && payload.ImageURL.Detail != "auto" {
			return nil, unsupported("image detail constraints")
		}
		imageURL = payload.ImageURL.URL
	case OpenAIResponses:
		if block.Kind != "input_image" {
			return nil, unsupported("provider content blocks")
		}
		var payload struct {
			ImageURL string `json:"image_url"`
			Detail   string `json:"detail,omitempty"`
		}
		if decodeClosed(block.Payload.Bytes(), &payload) != nil {
			return nil, unsupported("image fields")
		}
		if payload.Detail != "" && payload.Detail != "auto" {
			return nil, unsupported("image detail constraints")
		}
		imageURL = payload.ImageURL
	case Anthropic:
		if block.Kind != "image" {
			return nil, unsupported("provider content blocks")
		}
		var payload struct {
			Source struct {
				Type      string `json:"type"`
				MediaType string `json:"media_type,omitempty"`
				Data      string `json:"data,omitempty"`
				URL       string `json:"url,omitempty"`
			} `json:"source"`
		}
		if decodeClosed(block.Payload.Bytes(), &payload) != nil {
			return nil, unsupported("image fields")
		}
		source := payload.Source
		switch source.Type {
		case "base64":
			if source.URL != "" {
				return nil, unsupported("ambiguous image source")
			}
			mime, data = source.MediaType, source.Data
		case "url":
			if source.Data != "" || source.MediaType != "" {
				return nil, unsupported("ambiguous image source")
			}
			imageURL = source.URL
		default:
			return nil, unsupported("image source")
		}
	case Gemini, GeminiCodeAssist:
		if block.Kind != "part" {
			return nil, unsupported("provider content blocks")
		}
		var payload struct {
			InlineData struct {
				MimeType string `json:"mimeType"`
				Data     string `json:"data"`
			} `json:"inlineData"`
		}
		if decodeClosed(block.Payload.Bytes(), &payload) != nil {
			return nil, unsupported("non-inline image parts")
		}
		mime, data = payload.InlineData.MimeType, payload.InlineData.Data
	default:
		return nil, unsupported("image source protocol")
	}
	if strings.HasPrefix(imageURL, "data:") {
		header, encoded, ok := strings.Cut(strings.TrimPrefix(imageURL, "data:"), ",")
		if !ok || !strings.HasSuffix(header, ";base64") {
			return nil, unsupported("image data URL")
		}
		mime, data = strings.TrimSuffix(header, ";base64"), encoded
		imageURL = ""
	}
	if imageURL != "" {
		u, err := url.Parse(imageURL)
		if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, unsupported("image URL")
		}
		if to == Gemini || to == GeminiCodeAssist {
			return nil, unsupported("remote image URLs on the upstream protocol; supply inline image data")
		}
	} else {
		switch mime {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
		default:
			return nil, unsupported("image media type")
		}
		if data == "" {
			return nil, unsupported("empty inline image")
		}
		if _, err := base64.StdEncoding.DecodeString(data); err != nil {
			return nil, unsupported("invalid image base64")
		}
	}
	var kind string
	var payload any
	switch to {
	case Anthropic:
		kind = "image"
		if imageURL != "" {
			payload = map[string]any{"source": map[string]any{"type": "url", "url": imageURL}}
		} else {
			payload = map[string]any{"source": map[string]any{"type": "base64", "media_type": mime, "data": data}}
		}
	case OpenAIChat, OpenAIResponses:
		if imageURL == "" {
			imageURL = "data:" + mime + ";base64," + data
		}
		if to == OpenAIChat {
			kind = "image_url"
			payload = map[string]any{"image_url": map[string]any{"url": imageURL}}
		} else {
			kind = "input_image"
			payload = map[string]any{"image_url": imageURL}
		}
	case Gemini, GeminiCodeAssist:
		kind = "part"
		payload = map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": data}}
	default:
		return nil, unsupported("image destination protocol")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	object, err := engine.ParseRequiredJSONObject(raw)
	if err != nil {
		return nil, err
	}
	return &engine.UnknownBlock{Kind: kind, Payload: object}, nil
}
