package main

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// responseRestorer 在出站方向把上游注入的展示身份还原为客户端自己声明的身份。
//
// 入站时 identity.replacements 把客户端身份（Codex、Claude Code、Grok Build 等）
// 改写成 target（默认 WorkBuddy），以免触发上游渠道校验。副作用是模型在回复中
// 也会自称 target。本模块在把响应正文写回客户端之前，只把「自称句式」里的
// target 改回客户端原始身份，不改其他位置出现的 target。
//
// 它不认识任何具体客户端：还原目标来自本次请求里客户端自己写的身份句，因此
// 对所有客户端通用。
type responseRestorer struct {
	// name 是还原目标，即客户端在请求中声明的身份名。
	name string
	// from 是上游伪装使用的身份名，即 identity.target。
	from string
	// pattern 匹配完整的自称句式，捕获组 1 是身份名。
	pattern *regexp.Regexp
	// fromPattern 匹配伪装身份名本身，仅用于判断末尾是否可能还在增长。
	fromPattern *regexp.Regexp
}

// selfMarkers 是模型自我称呼时可能出现的引导词。
// 顺序不影响正确性，最长匹配优先由上层的 LastIndex 选择逻辑处理。
var responseSelfMarkers = []string{
	"I am",
	"I'm",
	"We are",
	"We're",
	"my name is",
	"I go by",
	"我是",
	"我叫",
	"我的名字是",
	"我们是",
}

// responseRestoreMu 保护 currentResponseRestore，支持配置热加载。
var (
	responseRestoreMu     sync.RWMutex
	responseRestoreEnable bool
)

// setResponseRestoreConfig 由配置加载处调用，支持热加载。
func setResponseRestoreConfig(enabled bool) {
	responseRestoreMu.Lock()
	responseRestoreEnable = enabled
	responseRestoreMu.Unlock()
}

// responseRestoreSettings 读取当前开关状态。
func responseRestoreSettings() bool {
	responseRestoreMu.RLock()
	defer responseRestoreMu.RUnlock()
	return responseRestoreEnable
}

// newResponseRestorer 为一次请求构造还原器。
// identityName 为空或与伪装名相同、或开关关闭时返回 nil，调用方据此跳过还原。
func newResponseRestorer(identityName, from string) *responseRestorer {
	if identityName == "" || from == "" || strings.EqualFold(identityName, from) {
		return nil
	}
	quoted := regexp.QuoteMeta(from)
	pattern := regexp.MustCompile(
		`(?i)(?:\bI am\b|\bI'?m\b|\bWe are\b|\bWe'?re\b|\bmy name is\b|\bI go by\b|我是|我叫|我的名字是|我们是)` +
			`(?:\s*(?:\*\*|__|\*|_)?\s*(?:an?\s+|the\s+)?)(` + quoted + `)(?:\*\*|__|\*|_)?\b`,
	)
	return &responseRestorer{
		name:        identityName,
		from:        from,
		pattern:     pattern,
		fromPattern: regexp.MustCompile(`(?i)` + quoted),
	}
}

// rewrite 在完整文本中把自称句式里的 from 还原为客户端身份名。
// 只改自称句式，其它位置的 from（产品名、路径、用户引用）保持原样。
func (r *responseRestorer) rewrite(text string) string {
	if r == nil || text == "" {
		return text
	}
	return r.pattern.ReplaceAllStringFunc(text, func(match string) string {
		idx := r.pattern.FindStringSubmatchIndex(match)
		if len(idx) < 4 || idx[2] < 0 {
			return match
		}
		// 只替换捕获组（身份名），引导词、冠词与 Markdown 包裹保持原样。
		return match[:idx[2]] + r.name + match[idx[3]:]
	})
}

// holdback 返回文本末尾必须暂缓的字节数。
//
// 只暂缓「仍可能继续增长」的自称：结尾是引导词、引导词前缀、身份名前缀，
// 或完整身份名但后面还没有跟任何终止字符。一旦身份名后出现正文，说明自称
// 已经确定，可以立即还原发出。
//
// 暂缓范围只覆盖这一个自称，前面的正文仍按原始分片节奏立即输出。
func (r *responseRestorer) holdback(text string) int {
	if r == nil || text == "" {
		return 0
	}
	trimmed := strings.TrimRight(text, " \t\r\n")
	if trimmed == "" {
		return len(text)
	}
	lower := strings.ToLower(trimmed)
	fromLower := strings.ToLower(r.from)

	// 1. 找出最后一个完整的引导词位置，判断它后面的片段是否仍未确定。
	lastMarkerIdx := -1
	lastMarkerLen := 0
	for _, marker := range responseSelfMarkers {
		markerLower := strings.ToLower(marker)
		idx := strings.LastIndex(lower, markerLower)
		if idx < 0 {
			continue
		}
		if idx > lastMarkerIdx || (idx == lastMarkerIdx && len(marker) > lastMarkerLen) {
			lastMarkerIdx = idx
			lastMarkerLen = len(marker)
		}
	}

	if lastMarkerIdx >= 0 {
		after := trimmed[lastMarkerIdx+lastMarkerLen:]
		rest := strings.TrimLeft(after, " \t")
		restLower := strings.ToLower(rest)
		restLower = strings.TrimPrefix(restLower, "an ")
		restLower = strings.TrimPrefix(restLower, "a ")
		restLower = strings.TrimPrefix(restLower, "the ")
		articleLen := len(rest) - len(restLower)
		spaceLen := len(after) - len(rest)

		switch {
		case restLower == "":
			// 只有引导词或引导词 + 冠词，名字还没到。
			return len(text) - lastMarkerIdx
		case strings.HasPrefix(fromLower, restLower):
			// 名字还没写完：连同前面的空格/冠词整体暂缓。
			if articleLen > 0 {
				return articleLen + len(restLower)
			}
			return spaceLen + len(restLower)
		case strings.HasPrefix(restLower, fromLower):
			// 名字已完整。只有后面出现正文才可释放。
			nameLen := len(restLower)
			if n := len(fromLower); n < nameLen {
				nameLen = n
			}
			afterName := rest[articleLen+nameLen:]
			if afterName == "" || strings.TrimSpace(afterName) == "" || strings.TrimSpace(after[len(after)-len(rest):]) == "" {
				return len(after)
			}
			return 0
		}
	}

	// 2. 结尾是 ASCII 引导词/身份名前缀（不含完整词），必须暂缓。
	// 只检查 RuneStart 对齐的 ASCII 后缀，避免把多字节字符后半字节误当前缀。
	suffixLimit := len(fromLower) - 1
	for _, markerLower := range responseSelfMarkersLower() {
		if l := len(markerLower) - 1; l > suffixLimit {
			suffixLimit = l
		}
	}
	if suffixLimit > len(lower) {
		suffixLimit = len(lower)
	}
	for k := suffixLimit; k >= 1; k-- {
		if !utf8.RuneStart(lower[len(lower)-k]) {
			continue
		}
		suffix := lower[len(lower)-k:]
		if suffix[0] >= utf8.RuneSelf {
			continue
		}
		if strings.HasPrefix(fromLower, suffix) {
			return k + (len(text) - len(trimmed))
		}
		for _, markerLower := range responseSelfMarkersLower() {
			if strings.HasPrefix(markerLower, suffix) {
				return k + (len(text) - len(trimmed))
			}
		}
	}

	return len(text) - len(trimmed)
}

// responseSelfMarkersLower 返回小写的自称引导词列表。
func responseSelfMarkersLower() []string {
	out := make([]string, 0, len(responseSelfMarkers))
	for _, marker := range responseSelfMarkers {
		out = append(out, strings.ToLower(marker))
	}
	return out
}

// split 把缓冲文本切成「可立即发出」和「需继续暂缓」两段。
// context 是该通道已发出的最近原文，用于把跨分片的「我是」+「Work」+「Buddy」
// 当成一个整体暂缓；context 本身不会被再次输出。
func (r *responseRestorer) split(text, context string) (ready, rest string) {
	if text == "" {
		return "", ""
	}
	combined := context + text
	hbCombined := r.holdback(combined)
	var hbInText int
	switch {
	case hbCombined == 0:
		hbInText = 0
	case hbCombined > len(context):
		// 暂缓区间延伸到本次 text 内：整个未完成自称都在 pending 里，整体暂缓。
		hbInText = len(text)
	default:
		hbInText = r.holdback(text)
	}
	cut := len(text) - hbInText
	if cut <= 0 {
		return "", text
	}
	if cut > len(text) {
		cut = len(text)
	}
	for cut > 0 && cut < len(text) && !utf8.RuneStart(text[cut]) {
		cut--
	}
	if cut <= 0 {
		return "", text
	}
	return text[:cut], text[cut:]
}

// responseIdentityFromRequest 从入站请求里识别客户端自己声明的身份名。
//
// 只读取客户端指令位置（instructions / system / developer），不读 user、
// tool 消息，避免把用户提到客户端名字的情况误判为身份声明。
// 返回空串表示未识别到，调用方应跳过还原。
func responseIdentityFromRequest(body []byte, from string) string {
	if len(body) == 0 {
		return ""
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return ""
	}
	var texts []string
	if instructions, ok := doc["instructions"].(string); ok {
		texts = append(texts, instructions)
	}
	if system, exists := doc["system"]; exists {
		if text := responseCollectText(system); text != "" {
			texts = append(texts, text)
		}
	}
	for _, field := range []string{"messages", "input"} {
		items, ok := doc[field].([]any)
		if !ok {
			continue
		}
		for _, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch strings.ToLower(strOf(item["role"])) {
			case "system", "developer":
				texts = append(texts, responseCollectText(item["content"]))
			}
		}
	}
	joined := strings.Join(texts, "\n")
	if joined == "" {
		return ""
	}
	return responseIdentityFromText(joined, from)
}

// responseIdentityFromText 从指令文本里找出「You are X」形式的身份声明。
// 只接受与 target 不同的名字，避免把 target 当成客户端身份。
func responseIdentityFromText(text, from string) string {
	lower := strings.ToLower(text)
	for _, marker := range []string{"you are ", "you're ", "你是", "你作为"} {
		idx := strings.Index(lower, strings.ToLower(marker))
		if idx < 0 {
			continue
		}
		rest := text[idx+len(marker):]
		name := responseLeadingIdentityName(rest)
		if name == "" {
			continue
		}
		if strings.EqualFold(name, from) {
			continue
		}
		return name
	}
	return ""
}

// responseLeadingIdentityName 取身份声明后紧跟的名字。
//
// 支持 "Codex, a coding agent"、"Claude Code, Anthropic's ..." 这类形式；
// 遇到逗号、句点、换行或超过 4 个词时停止。
func responseLeadingIdentityName(text string) string {
	text = strings.TrimLeft(text, " \t")
	text = strings.TrimLeft(text, "*_`")
	if text == "" {
		return ""
	}
	// 最多取 4 个词，避免把整句描述吞掉。
	fields := strings.FieldsFunc(text, func(r rune) bool {
		switch r {
		case ',', '.', ';', ':', '(', '\n', '\r',
			'，', '。', '；', '：', '（', '、', '！', '？':
			return true
		}
		return false
	})
	if len(fields) == 0 {
		return ""
	}
	words := strings.Fields(fields[0])
	if len(words) == 0 {
		return ""
	}
	if len(words) > 4 {
		words = words[:4]
	}
	name := strings.TrimRight(strings.Join(words, " "), "*_` \t")
	if name == "" {
		return ""
	}
	if responseIsGenericRole(name) {
		return ""
	}
	return name
}

// responseGenericRoles 是常见泛称，不构成客户端身份声明。
// 这些值出现在 "You are a helpful assistant" 这类通用指令里，
// 若当作身份名会让还原逻辑误改正文。
var responseGenericRoles = []string{
	"a helpful assistant",
	"an ai assistant",
	"a helpful ai assistant",
	"an assistant",
	"a large language model",
	"an ai",
	"assistant",
	"ai assistant",
}

// responseIsGenericRole 判断取到的名字是否只是泛称。
func responseIsGenericRole(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	for _, generic := range responseGenericRoles {
		if lower == generic {
			return true
		}
	}
	return false
}

// responseCollectText 从 content 字段收集纯文本，忽略图片与工具块。
func responseCollectText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var b strings.Builder
		for _, raw := range value {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch strOf(block["type"]) {
			case "text", "input_text", "output_text":
				if text, ok := block["text"].(string); ok {
					b.WriteString(text)
					b.WriteString("\n")
				}
			}
		}
		return b.String()
	}
	return ""
}

// responseStreamRestorer 维护一次流式响应中各文本通道的暂缓状态。
//
// 三条协议（Chat / Responses / Messages）的上游都是 Chat 格式分片，
// 因此只要在这一层还原，三个下游协议的输出自然都是还原后的文本。
type responseStreamRestorer struct {
	r *responseRestorer
	// channel 目前只用 "content" 与 "reasoning" 两个键，
	// 正文与思维链各自独立暂缓，互不干扰。
	pending map[string]string
	emitted map[string]string
	// outLen 记录每个通道已发出的还原后字节数。
	//
	// 还原会改变文本长度（WorkBuddy → Codex 变短），因此不能按原文偏移
	// 切分已还原结果；只记录已经写到下游的字节数，下次从这里接着切，
	// 才既能避免重复输出，也不会切在多字节字符中间。
	outLen map[string]int
}

// newResponseStreamRestorer 构造流式还原状态；r 为 nil 时返回 nil。
func newResponseStreamRestorer(r *responseRestorer) *responseStreamRestorer {
	if r == nil {
		return nil
	}
	return &responseStreamRestorer{
		r:       r,
		pending: make(map[string]string, 2),
		emitted: make(map[string]string, 2),
		outLen:  make(map[string]int, 2),
	}
}

// push 送入一个分片，返回本次可立即发出的已还原文本。
func (s *responseStreamRestorer) push(channel, text string) string {
	if s == nil || s.r == nil || text == "" {
		return text
	}
	s.pending[channel] += text
	ready, rest := s.r.split(s.pending[channel], s.emitted[channel])
	s.pending[channel] = rest
	if ready == "" {
		return ""
	}
	combined := s.emitted[channel] + ready
	rewritten := s.r.rewrite(combined)
	tail := ""
	if len(rewritten) > s.outLen[channel] {
		tail = rewritten[s.outLen[channel]:]
	}
	s.emitted[channel] = combined
	s.outLen[channel] = len(rewritten)
	return tail
}

// flush 在流结束时把剩余暂缓文本还原发出。各通道按固定顺序处理。
func (s *responseStreamRestorer) flush() map[string]string {
	if s == nil || s.r == nil {
		return nil
	}
	out := make(map[string]string, len(s.pending))
	for _, channel := range []string{"reasoning", "content"} {
		rest := s.pending[channel]
		if rest == "" {
			continue
		}
		combined := s.emitted[channel] + rest
		rewritten := s.r.rewrite(combined)
		if len(rewritten) > s.outLen[channel] {
			out[channel] = rewritten[s.outLen[channel]:]
			s.outLen[channel] = len(rewritten)
		}
		delete(s.pending, channel)
	}
	return out
}

// restoreChatChunkDelta 就地把一个上游 Chat 分片里的正文与思维链还原。
//
// 三条下游协议（Chat / Responses / Messages）的上游都是 Chat 格式，因此在这
// 一层还原只需一处实现。返回 true 表示该分片被改动。
func (s *responseStreamRestorer) restoreChatChunkDelta(chunk map[string]any) bool {
	if s == nil || s.r == nil {
		return false
	}
	choices, _ := chunk["choices"].([]any)
	changed := false
	for _, rawChoice := range choices {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			continue
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			continue
		}
		if v, ok := delta["content"].(string); ok && v != "" {
			if out := s.push("content", v); out != v {
				delta["content"] = out
				changed = true
			}
		}
		if v, ok := delta["reasoning_content"].(string); ok && v != "" {
			if out := s.push("reasoning", v); out != v {
				delta["reasoning_content"] = out
				changed = true
			}
		}
	}
	return changed
}

// restoreAggregateText 还原非流式聚合后的正文与思维链。
func (s *responseStreamRestorer) restoreAggregateText(content, reasoning string) (string, string) {
	if s == nil || s.r == nil {
		return content, reasoning
	}
	return s.r.rewrite(content), s.r.rewrite(reasoning)
}

// restoreChunkInPlace 是给上游流式循环用的入口：就地把 chunk 的 delta 文本
// 还原为客户端身份，并在本次分片使此前暂缓的内容可以释放时一并吐出。
//
// 由于暂缓窗口只覆盖结尾可能仍在增长的身份名（通常几个字符），即使上游
// 在此刻断流，丢失量也极小；正常收尾时 flushRemaining 会补齐。
func (s *responseStreamRestorer) restoreChunkInPlace(chunk map[string]any) bool {
	return s.restoreChatChunkDelta(chunk)
}

// flushIntoChunk 在收尾时把剩余暂缓文本并入一个合成 chunk，
// 由调用方在发 completed 事件之前先发出它。
func (s *responseStreamRestorer) flushIntoChunk() map[string]any {
	rest := s.flush()
	if len(rest) == 0 {
		return nil
	}
	delta := map[string]any{}
	for _, channel := range []string{"reasoning", "content"} {
		if text, ok := rest[channel]; ok && text != "" {
			if channel == "reasoning" {
				delta["reasoning_content"] = text
			} else {
				delta["content"] = text
			}
		}
	}
	if len(delta) == 0 {
		return nil
	}
	return map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta}}}
}

// responseRestoreConfig 是 identity 段里的可选开关。
//
// 用 *bool 区分「未配置」与显式 false，未配置时保持关闭，
// 与既有行为完全一致。
type responseRestoreConfig struct {
	Enabled *bool `json:"responseRestore,omitempty"`
}

// applyResponseRestoreConfig 在配置加载时调用，支持热加载。
func applyResponseRestoreConfig(raw *bool) {
	enabled := raw != nil && *raw
	setResponseRestoreConfig(enabled)
}

// responseRestorerFor 为一次请求构造还原器；关闭或无法识别身份时返回 nil。
//
// target 取 identity.target，body 是客户端原始请求体。
func responseRestorerFor(body []byte, target string) *responseRestorer {
	if !responseRestoreSettings() {
		return nil
	}
	name := responseIdentityFromRequest(body, target)
	if name == "" {
		return nil
	}
	return newResponseRestorer(name, target)
}

// identityTarget 返回当前配置的伪装身份名，供出站还原判断使用。
func identityTarget() string {
	settings := identitySnapshot()
	if settings.target != "" {
		return settings.target
	}
	return "WorkBuddy"
}

// reset 清空流式状态，用于上游重放场景：重放会重新消费一遍上游流，
// 已发出计数必须归零，否则第二次的文本会被当成已发送而丢弃。
func (s *responseStreamRestorer) reset() {
	if s == nil {
		return
	}
	s.pending = make(map[string]string, 2)
	s.emitted = make(map[string]string, 2)
	s.outLen = make(map[string]int, 2)
}

// responseRestoreContextKey 用于把本次请求的还原器挂到 context 上。
//
// 三个协议 handler 都持有客户端原始请求体，但流式函数只拿到 r 和 resp；
// 用 context 传递可以避免给六个函数签名加参数，也不需要改协议结构。
type responseRestoreContextKey struct{}

// withResponseRestorer 把还原器挂到请求 context。
func withResponseRestorer(r *http.Request, restorer *responseStreamRestorer) *http.Request {
	if restorer == nil {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), responseRestoreContextKey{}, restorer))
}

// responseRestorerFromRequest 取出挂载的还原器；没有则返回 nil。
func responseRestorerFromRequest(r *http.Request) *responseStreamRestorer {
	if r == nil {
		return nil
	}
	value, _ := r.Context().Value(responseRestoreContextKey{}).(*responseStreamRestorer)
	return value
}

// decodeJSONObject 解析一个 JSON 对象，供非流式聚合路径复用。
func decodeJSONObject(data []byte) (map[string]any, error) {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// restoreAggregateObject 就地还原非流式响应里的正文与思维链。
//
// 覆盖 Chat Completions 的 choices[].message，以及 Responses 的 output[]。
// 返回 true 表示发生了变化。
func (s *responseStreamRestorer) restoreAggregateObject(doc map[string]any) bool {
	if s == nil || s.r == nil || doc == nil {
		return false
	}
	changed := false
	if choices, ok := doc["choices"].([]any); ok {
		for _, rawChoice := range choices {
			choice, ok := rawChoice.(map[string]any)
			if !ok {
				continue
			}
			message, ok := choice["message"].(map[string]any)
			if !ok {
				continue
			}
			if v, ok := message["content"].(string); ok && v != "" {
				if out := s.r.rewrite(v); out != v {
					message["content"] = out
					changed = true
				}
			}
			if v, ok := message["reasoning_content"].(string); ok && v != "" {
				if out := s.r.rewrite(v); out != v {
					message["reasoning_content"] = out
					changed = true
				}
			}
		}
	}
	if output, ok := doc["output"].([]any); ok {
		for _, rawItem := range output {
			item, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			content, ok := item["content"].([]any)
			if !ok {
				continue
			}
			for _, rawBlock := range content {
				block, ok := rawBlock.(map[string]any)
				if !ok {
					continue
				}
				if v, ok := block["text"].(string); ok && v != "" {
					if out := s.r.rewrite(v); out != v {
						block["text"] = out
						changed = true
					}
				}
			}
		}
	}
	if content, ok := doc["content"].([]any); ok {
		for _, rawBlock := range content {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				continue
			}
			for _, field := range []string{"text", "thinking"} {
				if v, ok := block[field].(string); ok && v != "" {
					if out := s.r.rewrite(v); out != v {
						block[field] = out
						changed = true
					}
				}
			}
		}
	}
	return changed
}
