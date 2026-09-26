// Package mcpauth manages the instance MCP token in host-private durable state.
// Plugin KV access cannot claim the reserved host namespace.
package mcpauth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
)

const namespace = "@torana/mcp-auth"
const tokenKey = "instance-token"

type State interface {
	GetVersioned(namespace, key string) (value, version string, found bool, err error)
	CompareAndSet(namespace, key, value string, expected *string) (applied bool, version string, err error)
}

type Sealer interface {
	Encrypt(string) (string, error)
	Decrypt(string) (string, error)
}

type Manager struct {
	state  State
	sealer Sealer
}

func New(state State, sealer Sealer) *Manager { return &Manager{state: state, sealer: sealer} }

func (m *Manager) configured() bool { return m != nil && m.state != nil && m.sealer != nil }

// Current never creates a token as a side effect of an unauthenticated request.
// An empty token means MCP authentication is not configured and must fail closed.
func (m *Manager) Current() (string, error) {
	if !m.configured() {
		return "", errors.New("MCP token storage is unavailable")
	}
	sealed, _, found, err := m.state.GetVersioned(namespace, tokenKey)
	if err != nil || !found {
		return "", err
	}
	return m.sealer.Decrypt(sealed)
}

// Ensure is an explicit operator setup action. Concurrent setup calls converge
// on the same stored token, rather than returning orphaned credentials.
func (m *Manager) Ensure() (string, error) { return m.write(false) }

// Rotate replaces the token atomically. Authentication reads Current on each
// request, so the old token becomes invalid as soon as this write commits.
func (m *Manager) Rotate() (string, error) { return m.write(true) }

func (m *Manager) write(rotate bool) (string, error) {
	if !m.configured() {
		return "", errors.New("MCP token storage is unavailable")
	}
	for range 8 {
		previous, version, found, err := m.state.GetVersioned(namespace, tokenKey)
		if err != nil {
			return "", err
		}
		if found && !rotate {
			return m.sealer.Decrypt(previous)
		}
		var random [32]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", err
		}
		token := base64.RawURLEncoding.EncodeToString(random[:])
		sealed, err := m.sealer.Encrypt(token)
		if err != nil {
			return "", err
		}
		var expected *string
		if found {
			expected = &version
		}
		applied, _, err := m.state.CompareAndSet(namespace, tokenKey, sealed, expected)
		if err != nil {
			return "", err
		}
		if applied {
			return token, nil
		}
	}
	return "", errors.New("MCP token changed concurrently; retry setup")
}
