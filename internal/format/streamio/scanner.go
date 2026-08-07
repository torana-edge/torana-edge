// Package streamio owns the input bound shared by every line-framed provider
// stream. Provider adapters may use different wire envelopes, but they must not
// silently truncate at different sizes or allocate without a bound.
package streamio

import (
	"bufio"
	"io"
)

// MaxFrameBytes is the largest single provider stream line Torana accepts.
// Tool arguments can legitimately be much larger than bufio.Scanner's 64 KiB
// default, while a finite 2 MiB ceiling prevents a newline-less upstream from
// growing memory without bound.
const MaxFrameBytes = 2 * 1024 * 1024

// NewScanner returns the one scanner configuration used by every line-framed
// provider stream. Callers must inspect Err after Scan returns false and expose
// a terminal StreamError rather than treating it as clean EOF.
func NewScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxFrameBytes)
	return scanner
}
