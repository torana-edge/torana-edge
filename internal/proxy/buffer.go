package proxy

import (
	"bytes"
	"io"
	"sync"
)

var bodyBufferPool = sync.Pool{
	New: func() any {
		buf := new(bytes.Buffer)
		buf.Grow(64 * 1024) // 64KB initial capacity
		return buf
	},
}

func getBodyBuffer() *bytes.Buffer {
	return bodyBufferPool.Get().(*bytes.Buffer)
}

func releaseBodyBuffer(buf *bytes.Buffer) {
	if buf.Cap() < 10*1024*1024 { // Drop buffers larger than 10MB to avoid memory leaks
		buf.Reset()
		bodyBufferPool.Put(buf)
	}
}

// readBodyPool uses the buffer pool to read the body, preventing large allocations.
func readBodyPool(r io.Reader) ([]byte, error) {
	buf := getBodyBuffer()
	defer releaseBodyBuffer(buf)

	_, err := buf.ReadFrom(r)
	if err != nil {
		return nil, err
	}

	// We must copy the bytes because the buffer will be reused when released.
	// However, since we return a copy, we still allocate!
	// To truly avoid allocation, we need a way to return the buffer itself and have the caller release it.
	// But `ChatRequest` requires a byte slice for Unmarshal.
	// For now, doing a single exact-size allocation (via copy) is better than io.ReadAll's internal growth allocations (which doubles capacity and leaves garbage).
	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	return out, nil
}
