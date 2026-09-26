//go:build !linux && !darwin

package harness

import "os"

func preserveGroup(_ *os.File, _ string) error { return nil }
