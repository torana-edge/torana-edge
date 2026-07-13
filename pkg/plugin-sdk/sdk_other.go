//go:build !wasip1

package plugin_sdk

func alloc(size uint32) uint32 { return 0 }
func dealloc(ptr uint32, size uint32) {}
func ReadBytes(ptr, size uint32) []byte { return nil }
func WriteResult(data []byte) uint64 { return 0 }

func OnChatRequest(handler func(req []byte) ([]byte, error)) {}
func OnChatResponse(handler func(resp []byte) ([]byte, error)) {}
func OnStreamChunk(handler func(chunk []byte) ([]byte, error)) {}

const (
	LogLevelDebug = 0
	LogLevelInfo  = 1
)

func Log(msg string, level int32) {}

func HostCall(cmd string, args string) (string, error) { return "", nil }
