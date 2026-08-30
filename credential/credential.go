// Package credential defines Torana's pluggable credential-provider boundary.
// Providers resolve an opaque key into secret bytes. Torana configuration maps
// stable credential IDs to a provider and key; callers and plugins never see
// provider configuration or enumerate other credentials.
package credential

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

type Provider interface {
	Resolve(context.Context, string) ([]byte, error)
}

type Factory func(json.RawMessage) (Provider, error)

var factories = struct {
	sync.RWMutex
	m map[string]Factory
}{m: map[string]Factory{"env": newEnvProvider}}

// RegisterProvider adds a credential provider type for custom Torana builds.
// Registration is process-wide and must happen before the server is built.
func RegisterProvider(name string, factory Factory) error {
	if name == "" || factory == nil {
		return fmt.Errorf("credential provider name and factory are required")
	}
	factories.Lock()
	defer factories.Unlock()
	if _, exists := factories.m[name]; exists {
		return fmt.Errorf("credential provider %q is already registered", name)
	}
	factories.m[name] = factory
	return nil
}

func NewProvider(name string, config json.RawMessage) (Provider, error) {
	factories.RLock()
	factory := factories.m[name]
	factories.RUnlock()
	if factory == nil {
		return nil, fmt.Errorf("unknown credential provider type %q", name)
	}
	return factory(append(json.RawMessage(nil), config...))
}

type envProvider struct{}

func newEnvProvider(config json.RawMessage) (Provider, error) {
	if len(config) > 0 && string(config) != "{}" && string(config) != "null" {
		return nil, fmt.Errorf("env credential provider takes no configuration")
	}
	return envProvider{}, nil
}

func (envProvider) Resolve(_ context.Context, key string) ([]byte, error) {
	if key == "" {
		return nil, fmt.Errorf("environment variable name is required")
	}
	value, ok := os.LookupEnv(key)
	if !ok || value == "" {
		return nil, fmt.Errorf("environment credential %q is not set", key)
	}
	return []byte(value), nil
}

// Registry is an immutable source map constructed with one server generation.
type Registry struct{ providers map[string]Provider }

func NewRegistry(sources map[string]Source) (*Registry, error) {
	return NewRegistryWithProviders(sources, nil)
}

// NewRegistryWithProviders allows the host to supply built-ins whose
// construction depends on host-owned state (for example the encrypted local
// credential store). Explicit providers override factories of the same type.
func NewRegistryWithProviders(sources map[string]Source, explicit map[string]Provider) (*Registry, error) {
	providers := make(map[string]Provider, len(sources))
	for name, source := range sources {
		if name == "" || source.Type == "" {
			return nil, fmt.Errorf("credential source name and type are required")
		}
		provider := explicit[source.Type]
		var err error
		if provider == nil {
			provider, err = NewProvider(source.Type, source.Config)
		}
		if err != nil {
			return nil, fmt.Errorf("credential source %q: %w", name, err)
		}
		providers[name] = provider
	}
	return &Registry{providers: providers}, nil
}

type Source struct {
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config,omitempty"`
}

func (r *Registry) Resolve(ctx context.Context, source, key string) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("credential registry is not configured")
	}
	provider := r.providers[source]
	if provider == nil {
		return nil, fmt.Errorf("credential source %q is not configured", source)
	}
	value, err := provider.Resolve(ctx, key)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), value...), nil
}
