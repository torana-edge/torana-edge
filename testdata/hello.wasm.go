package main


var heap [65536]byte
var bump uint32

//export alloc
func alloc(size uint32) uint32 {
	if bump + size > uint32(len(heap)) {
		return 0
	}
	ptr := bump
	bump += size
	return ptr
}

//export dealloc
func dealloc(ptr, size uint32) {}

//export on_chat_request
func on_chat_request(ptr, size uint32) uint64 {
	data := heap[ptr:ptr+size]
	// Return a simple acknowledgement
	resp := []byte(`{"result":"ok","input_len":` + itoa(len(data)) + `}`)
	respPtr := alloc(uint32(len(resp)))
	copy(heap[respPtr:], resp)
	return uint64(respPtr)<<32 | uint64(len(resp))
}

func itoa(n int) string {
	if n == 0 { return "0" }
	var buf [10]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func main() {}
