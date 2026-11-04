package main

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// This is the original A parity corpus: expected output is A's own whole-text
// rewrite, not B's full replacement policy.
func TestARecheckStreamingParity(t *testing.T) {
	const seed, cases = 7, 3000
	r := newResponseRestorer("Codex", "WorkBuddy")
	fragments := []string{
		"这是普通正文。", "Sure. ", "\n", " ", "OK",
		"I am ", "I'm ", "We are ", "我是", "我叫", "my name is ",
		"WorkBuddy", "**WorkBuddy**", "workbuddy",
		"Codex", "Claude Code",
		"。", ".", "，", ",", "！",
	}
	rng := rand.New(rand.NewSource(seed))
	fail := 0
	firstFail := ""
	for i := 0; i < cases; i++ {
		var full strings.Builder
		for n := 1 + rng.Intn(12); n > 0; n-- {
			full.WriteString(fragments[rng.Intn(len(fragments))])
		}
		text := full.String()
		want := r.rewrite(text)
		stream := newResponseStreamRestorer(r)
		var got strings.Builder
		for pos := 0; pos < len(text); {
			end := pos + 1 + rng.Intn(4)
			if end > len(text) {
				end = len(text)
			}
			for end < len(text) && !utf8.RuneStart(text[end]) {
				end++
			}
			chunk := map[string]any{"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"content": text[pos:end]},
			}}}
			stream.restoreChunkInPlace(chunk)
			delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
			if value, ok := delta["content"].(string); ok {
				got.WriteString(value)
			}
			pos = end
		}
		if tail := stream.flushIntoChunk(); tail != nil {
			delta := tail["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
			if value, ok := delta["content"].(string); ok {
				got.WriteString(value)
			}
		}
		if got.String() != want {
			fail++
			if firstFail == "" {
				firstFail = fmt.Sprintf("case=%d input=%q got=%q want=%q", i, text, got.String(), want)
			}
		}
	}
	fmt.Printf("A_RECHECK seed=%d cases=%d fail=%d ratio=%.4f first=%s\n", seed, cases, fail, float64(fail)/float64(cases), firstFail)
	if fail != 0 {
		t.Errorf("A streaming parity failures: %d/%d", fail, cases)
	}
}
