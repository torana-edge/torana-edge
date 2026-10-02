package wasm

type releaseInputKey struct{}

// SetBundleDigest binds user-approved exceptions to the complete bundle, not
// just its WASM code. Changing metadata or permissions also invalidates them.
func (p *Plugin) SetBundleDigest(digest string) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	p.bundleDigest = digest
}
