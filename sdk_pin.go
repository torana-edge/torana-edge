// Package sdkpin reads the SDK pin from the go.mod used to build Torana.
package sdkpin

import (
	_ "embed"
	"errors"

	"golang.org/x/mod/modfile"
)

//go:embed go.mod
var moduleFile []byte

const sdkModulePath = "github.com/torana-edge/torana-plugin-sdk"

// SDKVersion is the declared, portable SDK release for this Torana source.
// The running binary's build info is preferred when it contains a selected
// module version; this fallback also works in isolated package-test binaries.
func SDKVersion() (string, error) {
	file, err := modfile.Parse("go.mod", moduleFile, nil)
	if err != nil {
		return "", err
	}
	for _, requirement := range file.Require {
		if requirement.Mod.Path == sdkModulePath {
			return requirement.Mod.Version, nil
		}
	}
	return "", errors.New("Torana's go.mod does not require the plugin SDK")
}
