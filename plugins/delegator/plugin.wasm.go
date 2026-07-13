package main
import "encoding/json"
var heap = make([]byte, 0, 65536)
//go:wasmexport alloc
func alloc(size uint32) uint32{idx:=uint32(len(heap));heap=append(heap,make([]byte,size)...);return idx}
//go:wasmexport dealloc
func dealloc(ptr,size uint32){}
//go:wasmexport on_chat_request
func on_chat_request(ptr,size uint32) uint64{
	input:=string(heap[ptr:ptr+size])
	result:=map[string]any{"echo":input,"status":"ok"}
	out,_:=json.Marshal(result)
	p:=alloc(uint32(len(out)));copy(heap[p:],out)
	return uint64(p)<<32|uint64(len(out))
}
func main(){}
