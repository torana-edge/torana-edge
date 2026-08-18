package engine

import (
	"bytes"
	"testing"
)

var benchmarkJSONObjectSink OptionalJSONObject

func benchmarkLargeRequestObject() []byte {
	var b bytes.Buffer
	b.Grow(1 << 20)
	b.WriteString(`{"messages":[{"role":"tool","content":"`)
	b.Write(bytes.Repeat([]byte{'p'}, 1<<20))
	b.WriteString(`"}],"model":"gpt-bench","stream":false,"tools":[]}`)
	return b.Bytes()
}

func BenchmarkParseOptionalJSONObject1MiB(b *testing.B) {
	raw := benchmarkLargeRequestObject()
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for range b.N {
		got, err := ParseOptionalJSONObject(raw)
		if err != nil {
			b.Fatal(err)
		}
		benchmarkJSONObjectSink = got
	}
}

func BenchmarkProjectOptionalJSONObject1MiB(b *testing.B) {
	raw := benchmarkLargeRequestObject()
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for range b.N {
		got, err := ParseOptionalJSONObject(raw)
		if err != nil {
			b.Fatal(err)
		}
		got, err = got.WithoutMembers("model", "messages", "tools", "stream")
		if err != nil {
			b.Fatal(err)
		}
		benchmarkJSONObjectSink = got
	}
}
