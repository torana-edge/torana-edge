package proxy

import "fmt"

type namespaceAccessPolicy struct {
	registry  *namespaceRegistry
	protected map[string]bool
	overrides map[string]string
}

func newNamespaceAccessPolicy(registry *namespaceRegistry, protected []string, overrides map[string]string) (*namespaceAccessPolicy, error) {
	p := &namespaceAccessPolicy{registry: registry, protected: map[string]bool{}, overrides: map[string]string{}}
	if protected == nil {
		protected = []string{"pii", "pii_guard", "auth"}
	}
	for _, name := range protected {
		p.protected[name] = true
	}
	for key, access := range overrides {
		if accessRank(access) < 0 {
			return nil, fmt.Errorf("invalid model access %q for %q", access, key)
		}
		p.overrides[key] = access
	}
	return p, nil
}

func accessRank(access string) int {
	switch access {
	case "read":
		return 0
	case "confirm":
		return 1
	case "never":
		return 2
	default:
		return -1
	}
}

func (p *namespaceAccessPolicy) lookup(namespace, operation string, aliases bool) (namespaceEntry, namespaceOperation, bool) {
	if p == nil || p.registry == nil {
		return namespaceEntry{}, namespaceOperation{}, false
	}
	entry, ok := p.registry.resolve(namespace, aliases)
	if !ok {
		return namespaceEntry{}, namespaceOperation{}, false
	}
	for _, op := range entry.Operations {
		if op.ID == operation {
			return entry, op, true
		}
	}
	return namespaceEntry{}, namespaceOperation{}, false
}

// floor cannot be loosened by declarations, aliases, or operator overrides.
func (p *namespaceAccessPolicy) floor(entry namespaceEntry, op namespaceOperation) bool {
	if op.ID == "_config.get" {
		return true
	}
	if p.protected[entry.Name] && op.Risk != "read" {
		return true
	}
	if op.Source == "core" {
		switch op.ID {
		case "system.status", "plugins.list", "stats.get", "feed.recent", "session.usage", "suggestions.list", "changes.list", "changes.undo":
		default:
			return true
		}
	}
	return false
}

// ModelReachable is the single access decision for MCP, ask, search and
// description. Unknown operations, disabled guests and floor entries fail
// closed. A separate dispatcher must still enforce binding and revision checks.
func (p *namespaceAccessPolicy) ModelReachable(namespace, operation string) string {
	entry, op, ok := p.lookup(namespace, operation, false)
	if !ok || !op.Callable || p.floor(entry, op) {
		return "never"
	}
	access := op.ModelAccess
	if accessRank(access) < 0 {
		return "never"
	}
	for _, key := range []string{entry.Name, entry.Name + "." + op.ID} {
		if override, ok := p.overrides[key]; ok && accessRank(override) > accessRank(access) {
			access = override
		}
	}
	return access
}

type directiveAccess struct {
	Allowed             bool
	Confirm             bool
	ConversationBinding string
}

// DirectiveAllowed is the equivalent choke point for user-authored directives.
// user_direct is limited to eligible plugin writes, never protected/floor ops.
func (p *namespaceAccessPolicy) DirectiveAllowed(namespace, operation string) directiveAccess {
	entry, op, ok := p.lookup(namespace, operation, true)
	if !ok || !op.Callable || p.floor(entry, op) {
		return directiveAccess{}
	}
	access := p.ModelReachable(entry.Name, op.ID)
	if access == "never" {
		return directiveAccess{}
	}
	confirm := access == "confirm" || op.Risk != "read"
	if op.Source == "plugin" && op.Guest != nil && op.Guest.Directive != nil && op.Guest.Directive.UserDirect && op.Risk == "write" && !p.protected[entry.Name] {
		// Tightening overrides still require confirmation even if the guest
		// opted into direct execution for its ordinary write default.
		if p.overrides[entry.Name] != "confirm" && p.overrides[entry.Name+"."+op.ID] != "confirm" {
			confirm = false
		}
	}
	return directiveAccess{Allowed: true, Confirm: confirm, ConversationBinding: op.ConversationBinding}
}
