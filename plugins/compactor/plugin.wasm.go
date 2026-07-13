package main
import ("encoding/json";"strings")
var heap = make([]byte, 0, 65536)
//go:wasmexport alloc
func alloc(size uint32) uint32 {
	idx := uint32(len(heap)); heap = append(heap, make([]byte, size)...); return idx
}
//go:wasmexport dealloc
func dealloc(ptr, size uint32) {}
//go:wasmexport on_chat_request
func on_chat_request(ptr, size uint32) uint64 {
	input := string(heap[ptr:ptr+size])
	var msg struct{Chat string`json:"chat"`;Messages []struct{Role,Content,ToolCallID string`json:"role,content,tool_call_id"`}`json:"messages"`}
	json.Unmarshal([]byte(input), &msg)
	for i:=range msg.Messages{if msg.Messages[i].Role=="tool"&&len(msg.Messages[i].Content)>2000{
		msg.Messages[i].Content = compact(msg.Messages[i].Content)
	}}
	out,_:=json.Marshal(msg)
	p:=alloc(uint32(len(out)));copy(heap[p:],out)
	return uint64(p)<<32|uint64(len(out))
}
func compact(c string)string{
	for _,l:=range strings.Split(c,"\n"){_=l};return c[:min(500,len(c))]
}
func main(){}
