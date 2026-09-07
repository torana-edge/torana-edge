//go:build windows

package mitm

import (
	"bytes"
	"encoding/pem"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// systemRootsPEM returns the platform's trusted roots in PEM form.
//
// Windows keeps them in a certificate STORE, not a file, so none of the Unix
// bundle paths exist and there is nothing to read. Scanning for those paths
// and refusing when none matched — correct on Unix — made mitm.New fail
// outright here, so the ingress could not start at all on a platform Torana
// ships. Enumerating the ROOT store yields the same set the platform's own
// verifier trusts.
func systemRootsPEM() ([]byte, error) {
	name, err := windows.UTF16PtrFromString("ROOT")
	if err != nil {
		return nil, err
	}
	store, err := windows.CertOpenSystemStore(0, name)
	if err != nil {
		return nil, fmt.Errorf("open the Windows ROOT certificate store: %w", err)
	}
	defer func() { _ = windows.CertCloseStore(store, 0) }()

	var out bytes.Buffer
	var count int
	var ctx *windows.CertContext
	for {
		// Each call frees the context handed in, so ctx must not be touched
		// after this line other than by the next iteration.
		ctx, err = windows.CertEnumCertificatesInStore(store, ctx)
		if ctx == nil {
			// A nil context ends the enumeration; CRYPT_E_NOT_FOUND is how it
			// says "that was the last one" and is not a failure. Anything else
			// stopped us early and must not pass for a complete root set.
			if err != nil && !errors.Is(err, windows.Errno(windows.CRYPT_E_NOT_FOUND)) {
				return nil, fmt.Errorf("enumerate the Windows ROOT certificate store: %w", err)
			}
			break
		}
		// Copy out of the store's memory before the next call frees it.
		der := bytes.Clone(unsafe.Slice(ctx.EncodedCert, ctx.Length))
		if err := pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
			return nil, err
		}
		count++
	}
	if count == 0 {
		return nil, errors.New("the Windows ROOT certificate store holds no certificates; " +
			"SSL_CERT_FILE would then reject every tunneled host")
	}
	return out.Bytes(), nil
}
