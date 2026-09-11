package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
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

func BenchmarkParseOptionalJSONObjectExcluding1MiB(b *testing.B) {
	raw := benchmarkLargeRequestObject()
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for range b.N {
		got, err := ParseOptionalJSONObjectExcluding(raw, "model", "messages", "tools", "stream")
		if err != nil {
			b.Fatal(err)
		}
		benchmarkJSONObjectSink = got
	}
}

// BenchmarkSetMember covers the mutation path a plugin takes when it rewrites
// one member, at the sizes coding-agent requests actually reach. The 1 MiB
// benchmarks above measure parsing and projection; nothing measured the splice
// helpers, which is where the discarded defensive copy lived.
func BenchmarkSetMember(b *testing.B) {
	for _, kb := range []int{16, 256, 1024} {
		raw := benchmarkObjectOfSize(kb)
		b.Run(fmt.Sprintf("%dKiB", kb), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out, err := setMember(raw, "added", json.RawMessage(`"v"`))
				if err != nil {
					b.Fatal(err)
				}
				benchmarkBytesSink = out
			}
		})
	}
}

func BenchmarkDeleteMember(b *testing.B) {
	raw := benchmarkObjectOfSize(256)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := deleteMember(raw, "k00000")
		if err != nil {
			b.Fatal(err)
		}
		benchmarkBytesSink = out
	}
}

var benchmarkBytesSink []byte

// benchmarkObjectOfSize builds an object of roughly sizeKB kilobytes with many
// members, so the splice has realistic work to do on both sides of the cut.
func benchmarkObjectOfSize(sizeKB int) []byte {
	filler := strings.Repeat("x", 512)
	var b bytes.Buffer
	b.Grow(sizeKB * 1024)
	b.WriteByte('{')
	for i := range sizeKB * 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"k%05d":%q`, i, filler)
	}
	b.WriteByte('}')
	return b.Bytes()
}
