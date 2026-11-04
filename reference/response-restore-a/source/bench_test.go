package main

import (
	"strings"
	"testing"
)

// 构造不含自称句式的长文本，测量真实还原路径的开销。
func makeChunks(totalChars, chunkSize int) []string {
	unit := "这是一段普通正文内容，用于模拟真实的模型输出长度。"
	var sb strings.Builder
	for sb.Len() < totalChars {
		sb.WriteString(unit)
	}
	text := sb.String()[:totalChars]
	var chunks []string
	for i := 0; i < len(text); i += chunkSize {
		end := i + chunkSize
		if end > len(text) {
			end = len(text)
		}
		for end < len(text) && (text[end]&0xC0) == 0x80 {
			end++
		}
		chunks = append(chunks, text[i:end])
		i = end - chunkSize
	}
	return chunks
}

func benchStream(b *testing.B, chunks []string) {
	b.ResetTimer()
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		r := newResponseRestorer("Codex", "WorkBuddy")
		s := newResponseStreamRestorer(r)
		for _, c := range chunks {
			chunk := map[string]any{"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": c},
			}}}
			s.restoreChunkInPlace(chunk)
		}
	}
}

func Benchmark2K(b *testing.B)  { benchStream(b, makeChunks(2*1024, 20)) }
func Benchmark8K(b *testing.B)  { benchStream(b, makeChunks(8*1024, 20)) }
func Benchmark20K(b *testing.B) { benchStream(b, makeChunks(20*1024, 20)) }
