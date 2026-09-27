package proxy

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

type transcriptTicketState struct {
	Kind    string `json:"kind"`
	Hash    string `json:"hash"`
	Nonce   string `json:"nonce"`
	Expires int64  `json:"expires"`
}

func canonicalInvokeHash(input namespaceInvokeInput) (string, error) {
	if len(input.Input) == 0 {
		input.Input = json.RawMessage(`{}`)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	return consentArgumentHash(raw), nil
}

func (s *Server) sealTranscriptTicket(ctx context.Context, input namespaceInvokeInput) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.secrets == nil {
		return "", errors.New("secret store is unavailable")
	}
	hash, err := canonicalInvokeHash(input)
	if err != nil {
		return "", err
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	state := transcriptTicketState{Kind: "mcp-transcript-ticket-v1", Hash: hash, Nonce: base64.RawURLEncoding.EncodeToString(nonce[:]), Expires: time.Now().Add(operationConsentTTL).Unix()}
	encoded, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	return s.secrets.Encrypt(string(encoded))
}

func (s *Server) consumeTranscriptTicket(ctx context.Context, sealed string, raw json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.secrets == nil || s.suggestions == nil || sealed == "" {
		return errors.New("transcript ticket is unavailable")
	}
	input, ok := decodeNamespaceInvoke(raw)
	if !ok {
		return errors.New("invalid transcript invocation")
	}
	hash, err := canonicalInvokeHash(input)
	if err != nil {
		return err
	}
	plaintext, err := s.secrets.Decrypt(sealed)
	if err != nil {
		return errors.New("invalid transcript ticket")
	}
	var state transcriptTicketState
	if json.Unmarshal([]byte(plaintext), &state) != nil || state.Kind != "mcp-transcript-ticket-v1" || state.Hash != hash || state.Nonce == "" || time.Now().Unix() >= state.Expires {
		return errors.New("invalid transcript ticket")
	}
	return s.suggestions.ClaimTranscriptTicket(state.Nonce, time.Unix(state.Expires, 0))
}
