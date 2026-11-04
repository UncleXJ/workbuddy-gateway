package main

import (
	"strings"
	"testing"
)

func TestResponseIdentityDetectsVariousClients(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "codex instructions",
			body: `{"instructions":"You are Codex, a coding agent based on GPT-5. You and the user share one workspace."}`,
			want: "Codex",
		},
		{
			name: "claude code instructions",
			body: `{"instructions":"You are Claude Code, Anthropic's official CLI for Claude."}`,
			want: "Claude Code",
		},
		{
			name: "grok build instructions",
			body: `{"instructions":"You are Grok Build, a terminal coding agent."}`,
			want: "Grok Build",
		},
		{
			name: "anthropic system blocks",
			body: `{"system":[{"type":"text","text":"You are Cursor Agent, an AI code editor."}],"messages":[]}`,
			want: "Cursor Agent",
		},
		{
			name: "developer role",
			body: `{"messages":[{"role":"developer","content":"You are Aider, a pair programming tool."},{"role":"user","content":"hi"}]}`,
			want: "Aider",
		},
		{
			name: "chinese declaration",
			body: `{"instructions":"你是通义灵码，一个编码助手。"}`,
			want: "通义灵码",
		},
		{
			name: "user text ignored",
			body: `{"messages":[{"role":"user","content":"You are Codex, right?"}]}`,
			want: "",
		},
		{
			name: "no declaration",
			body: `{"instructions":"You are a helpful assistant."}`,
			want: "",
		},
		{
			name: "same as target",
			body: `{"instructions":"You are WorkBuddy, an AI agent."}`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := responseIdentityFromRequest([]byte(tc.body), "WorkBuddy")
			if got != tc.want {
				t.Fatalf("identity=%q want %q", got, tc.want)
			}
		})
	}
}

func TestResponseRestoreOnlySelfReference(t *testing.T) {
	r := newResponseRestorer("Codex", "WorkBuddy")
	if r == nil {
		t.Fatal("restorer should not be nil")
	}
	cases := map[string]string{
		"I'm WorkBuddy.":                          "I'm Codex.",
		"I am WorkBuddy, a coding agent.":         "I am Codex, a coding agent.",
		"我是 WorkBuddy，一个编码助手。":                   "我是 Codex，一个编码助手。",
		"我是 **WorkBuddy**，一个编码代理。":              "我是 **Codex**，一个编码代理。",
		"We are WorkBuddy and we help.":           "We are Codex and we help.",
		"WorkBuddy is a product by Tencent.":      "WorkBuddy is a product by Tencent.",
		"The WorkBuddy gateway proxies requests.": "The WorkBuddy gateway proxies requests.",
		"Call WorkBuddy.im for details.":          "Call WorkBuddy.im for details.",
	}
	for in, want := range cases {
		if got := r.rewrite(in); got != want {
			t.Errorf("rewrite(%q)=%q want %q", in, got, want)
		}
	}
}

func TestResponseRestoreNilWhenSameIdentity(t *testing.T) {
	if r := newResponseRestorer("WorkBuddy", "WorkBuddy"); r != nil {
		t.Fatal("restorer should be nil when client identity equals target")
	}
	if r := newResponseRestorer("", "WorkBuddy"); r != nil {
		t.Fatal("restorer should be nil without client identity")
	}
}

func TestResponseStreamRestoreAcrossChunks(t *testing.T) {
	r := newResponseRestorer("Codex", "WorkBuddy")
	s := newResponseStreamRestorer(r)

	// 模拟上游把 "I'm WorkBuddy." 拆成多个分片。
	var got strings.Builder
	for _, part := range []string{"I'm ", "Work", "Buddy", "."} {
		chunk := map[string]any{"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"content": part},
		}}}
		s.restoreChunkInPlace(chunk)
		delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if v, ok := delta["content"].(string); ok {
			got.WriteString(v)
		}
	}
	if rest := s.flushIntoChunk(); rest != nil {
		delta := rest["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if v, ok := delta["content"].(string); ok {
			got.WriteString(v)
		}
	}
	if out := got.String(); out != "I'm Codex." {
		t.Fatalf("stream output=%q want %q", out, "I'm Codex.")
	}
}

func TestResponseStreamRestoreKeepsChannelsSeparate(t *testing.T) {
	r := newResponseRestorer("Codex", "WorkBuddy")
	s := newResponseStreamRestorer(r)

	var content, reasoning strings.Builder
	chunks := []map[string]any{
		{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "I am "}}}},
		{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "你好。"}}}},
		{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "WorkBuddy"}}}},
	}
	for _, chunk := range chunks {
		s.restoreChunkInPlace(chunk)
		delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if v, ok := delta["content"].(string); ok {
			content.WriteString(v)
		}
		if v, ok := delta["reasoning_content"].(string); ok {
			reasoning.WriteString(v)
		}
	}
	if rest := s.flushIntoChunk(); rest != nil {
		delta := rest["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if v, ok := delta["content"].(string); ok {
			content.WriteString(v)
		}
		if v, ok := delta["reasoning_content"].(string); ok {
			reasoning.WriteString(v)
		}
	}
	if got := content.String(); got != "你好。" {
		t.Fatalf("content=%q want %q", got, "你好。")
	}
	if got := reasoning.String(); got != "I am Codex" {
		t.Fatalf("reasoning=%q want %q", got, "I am Codex")
	}
}

func TestResponseStreamRestoreNoUTF8Damage(t *testing.T) {
	r := newResponseRestorer("Codex", "WorkBuddy")
	full := "你好，我是 WorkBuddy，一个编码代理。"
	for cut := 1; cut < len(full); cut++ {
		s := newResponseStreamRestorer(r)
		var got strings.Builder
		for _, part := range []string{full[:cut], full[cut:]} {
			chunk := map[string]any{"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": part},
			}}}
			s.restoreChunkInPlace(chunk)
			delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
			if v, ok := delta["content"].(string); ok {
				got.WriteString(v)
			}
		}
		if rest := s.flushIntoChunk(); rest != nil {
			delta := rest["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
			if v, ok := delta["content"].(string); ok {
				got.WriteString(v)
			}
		}
		out := got.String()
		if strings.ContainsRune(out, '\uFFFD') {
			t.Fatalf("cut=%d produced replacement char in %q", cut, out)
		}
		if !strings.Contains(out, "Codex") {
			t.Fatalf("cut=%d did not restore identity: %q", cut, out)
		}
	}
}

// 长回复场景的正确性：内容不得丢失或重复。
//
// 还原器会保留本轮原文用于跨分片匹配，内存与回复长度成正比；这一点
// 在 identity_restore.go 的 outLen 注释里有说明，此处只验证输出正确。
func TestResponseStreamRestoreLongReply(t *testing.T) {
	r := newResponseRestorer("Codex", "WorkBuddy")
	s := newResponseStreamRestorer(r)

	chunkText := "这是普通正文，不包含任何身份名。"
	const rounds = 200
	var totalOut strings.Builder
	for i := 0; i < rounds; i++ {
		chunk := map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"content": chunkText},
		}}}
		s.restoreChunkInPlace(chunk)
		delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if v, ok := delta["content"].(string); ok {
			totalOut.WriteString(v)
		}
	}
	if got, want := totalOut.Len(), len(chunkText)*rounds; got != want {
		t.Fatalf("output length=%d want %d", got, want)
	}
}

func truncateForLog(s string) string {
	if len(s) > 40 {
		return s[:20] + "..." + s[len(s)-15:]
	}
	return s
}
