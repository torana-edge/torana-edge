package proxy

import "sync"

// bodyBufferPool reuses byte buffers for request body reads to reduce
// GC pressure under concurrent load.
var bodyBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, 64*1024) // 64KB initial capacity
		return &buf
	},
}

// getBodyBuffer returns a pooled byte buffer. Call releaseBodyBuffer to
// return it to the pool.
func getBodyBuffer() *[]byte {
	return bodyBufferPool.Get().(*[]byte)
}

// releaseBodyBuffer resets and returns the buffer to the pool.
func releaseBodyBuffer(buf *[]byte) {
	if cap(*buf) < 1<<20 { // 1MB max poolable size
		*buf = (*buf)[:0]
		bodyBufferPool.Put(buf)
	}
}
