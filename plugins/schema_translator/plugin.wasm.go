package main
import "encoding/json"
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
	var msg struct{Chat string`json:"chat"`;Tools []struct{Name string`json:"name"`;Parameters map[string]any`json:"parameters"`}`json:"tools"`}
	json.Unmarshal([]byte(input), &msg)
	for i:=range msg.Tools{t:=&msg.Tools[i]
		if t.Parameters==nil{continue}
		p,_:=t.Parameters["properties"].(map[string]any)
		if p==nil{continue}
		if _,ok:=p["i"];!ok{p["i"]=map[string]any{"type":"string","description":"what you intend to accomplish"}
			if r,ok:=t.Parameters["required"].([]any);ok{t.Parameters["required"]=append(r,"i")}}}
	out,_:=json.Marshal(msg)
	p2:=alloc(uint32(len(out)));copy(heap[p2:],out)
	return uint64(p2)<<32|uint64(len(out))
}
func main(){}
