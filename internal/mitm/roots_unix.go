//go:build !windows

package mitm

import (
	"fmt"
	"os"
)

// systemRootsPEM returns the platform's trusted roots in PEM form.
//
// Unix distributions ship them as a file, so the answer is whichever of the
// well-known bundles exists.
func systemRootsPEM() ([]byte, error) {
	for _, p := range []string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/ssl/cert.pem",
	} {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			return b, nil
		}
	}
	return nil, fmt.Errorf("no system CA bundle found at any known path; " +
		"SSL_CERT_FILE would then reject every tunneled host. Install the " +
		"platform's ca-certificates package, or point mitm at a directory " +
		"holding a bundle you trust")
}
