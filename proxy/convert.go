package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// defaultClaudeMaxTokens 用于 OpenAI 请求没有设置输出上限时兜底。
// Anthropic Messages API 规定 max_tokens 必填，缺失会被上游拒绝。
const defaultClaudeMaxTokens = 4096

// ---------------------------------------------------------------------------
// JSON 取值辅助
// ---------------------------------------------------------------------------

func asMap(v interface{}) map[string]interface{} {
	m, _ := v.(map[string]interface{})
	return m
}

func asSlice(v interface{}) []interface{} {
	s, _ := v.([]interface{})
	return s
}

func asString(v interface{}) string {
	s, _ := v.(string)
	return s
}

func asInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return int(i), true
		}
	}
	return 0, false
}

func asFloat(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		if err == nil {
			return f, true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// Anthropic request -> OpenAI request
// ---------------------------------------------------------------------------

// convertClaudeToOpenAI 将 Anthropic Messages 请求转换为 OpenAI Chat Completions 请求。
// 字段按两家协议标准做确定性映射，不做“遇到错误再猜”的兼容：
//   - max_tokens -> max_completion_tokens（OpenAI 新模型的标准字段，max_tokens 已弃用）
//   - stop_sequences -> stop
//   - system（字符串或 text 块数组）-> 首条 role=system 消息
//   - 顶层 metadata.user_id -> user
//   - tools[].input_schema -> tools[].function.parameters
//   - content 块：text / image / tool_use / tool_result / document(text) 逐块转换
//   - 无 OpenAI 对应字段的 Anthropic 专有项（top_k、thinking、cache_control 等）丢弃
func convertClaudeToOpenAI(claudeReq map[string]interface{}) map[string]interface{} {
	openaiReq := make(map[string]interface{})

	for _, key := range []string{"model", "stream", "temperature", "top_p"} {
		if v, ok := claudeReq[key]; ok {
			openaiReq[key] = v
		}
	}

	// OpenAI 新标准：用 max_completion_tokens 控制输出长度。
	if v, ok := claudeReq["max_tokens"]; ok {
		openaiReq["max_completion_tokens"] = v
	}
	if v, ok := claudeReq["max_completion_tokens"]; ok {
		openaiReq["max_completion_tokens"] = v
	}

	// stop_sequences 是数组，OpenAI stop 同时接受字符串和数组（最多 4 条）。
	if stops := asSlice(claudeReq["stop_sequences"]); len(stops) > 0 {
		if len(stops) > 4 {
			stops = stops[:4]
		}
		openaiReq["stop"] = stops
	}

	// metadata.user_id 与 OpenAI user 语义一致。
	if md := asMap(claudeReq["metadata"]); md != nil {
		if uid := asString(md["user_id"]); uid != "" {
			openaiReq["user"] = uid
		}
	}

	// tools：Anthropic input_schema -> OpenAI function.parameters。
	// 只转换客户端自定义工具；带 type 的服务端内置工具（如 web_search）没有 OpenAI 对应实现。
	if tools := asSlice(claudeReq["tools"]); len(tools) > 0 {
		out := make([]interface{}, 0, len(tools))
		for _, t := range tools {
			tm := asMap(t)
			if toolType := asString(tm["type"]); toolType != "" && toolType != "custom" {
				continue
			}
			name := asString(tm["name"])
			if name == "" {
				continue
			}
			fn := map[string]interface{}{"name": name}
			if desc := asString(tm["description"]); desc != "" {
				fn["description"] = desc
			}
			if schema, ok := tm["input_schema"]; ok {
				fn["parameters"] = schema
			} else {
				fn["parameters"] = map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				}
			}
			out = append(out, map[string]interface{}{
				"type":     "function",
				"function": fn,
			})
		}
		if len(out) > 0 {
			openaiReq["tools"] = out
		}
	}

	if tc := asMap(claudeReq["tool_choice"]); tc != nil {
		if choice, ok := claudeToolChoiceToOpenAI(tc); ok {
			openaiReq["tool_choice"] = choice
		}
		if disable, ok := tc["disable_parallel_tool_use"].(bool); ok && disable {
			openaiReq["parallel_tool_calls"] = false
		}
	}

	// output_config.effort 与 OpenAI reasoning_effort 语义对应。
	if oc := asMap(claudeReq["output_config"]); oc != nil {
		if effort := asString(oc["effort"]); effort != "" {
			openaiReq["reasoning_effort"] = effort
		}
	}

	// 流式请求要求上游返回 usage，否则转换后的 Anthropic message_delta 拿不到 token 数。
	if stream, ok := claudeReq["stream"].(bool); ok && stream {
		openaiReq["stream_options"] = map[string]interface{}{"include_usage": true}
	}

	openaiReq["messages"] = claudeMessagesToOpenAI(claudeReq["messages"], claudeReq["system"])
	return openaiReq
}

func claudeToolChoiceToOpenAI(tc map[string]interface{}) (interface{}, bool) {
	switch asString(tc["type"]) {
	case "auto":
		return "auto", true
	case "any":
		return "required", true
	case "none":
		return "none", true
	case "tool":
		name := asString(tc["name"])
		if name == "" {
			return nil, false
		}
		return map[string]interface{}{
			"type":     "function",
			"function": map[string]interface{}{"name": name},
		}, true
	}
	return nil, false
}

func claudeMessagesToOpenAI(rawMessages interface{}, system interface{}) []interface{} {
	out := make([]interface{}, 0)
	if sys := claudeSystemToText(system); sys != "" {
		out = append(out, map[string]interface{}{"role": "system", "content": sys})
	}
	for _, raw := range asSlice(rawMessages) {
		m := asMap(raw)
		switch asString(m["role"]) {
		case "user", "":
			out = append(out, claudeUserMessageToOpenAI(m)...)
		case "assistant":
			if msg, ok := claudeAssistantMessageToOpenAI(m); ok {
				out = append(out, msg)
			}
		}
	}
	return out
}

// claudeSystemToText 把 system 字符串或 text 内容块数组拼成一段系统提示词。
func claudeSystemToText(v interface{}) string {
	switch s := v.(type) {
	case string:
		return s
	case []interface{}:
		texts := make([]string, 0, len(s))
		for _, raw := range s {
			bm := asMap(raw)
			if asString(bm["type"]) == "text" {
				if t := asString(bm["text"]); t != "" {
					texts = append(texts, t)
				}
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

func claudeUserMessageToOpenAI(m map[string]interface{}) []interface{} {
	content := m["content"]
	if s, ok := content.(string); ok {
		if s == "" {
			return nil
		}
		return []interface{}{map[string]interface{}{"role": "user", "content": s}}
	}

	blocks := asSlice(content)
	if len(blocks) == 0 {
		return nil
	}

	toolMessages := make([]interface{}, 0)
	parts := make([]interface{}, 0)
	texts := make([]string, 0)

	for _, raw := range blocks {
		bm := asMap(raw)
		switch asString(bm["type"]) {
		case "tool_result":
			toolMessages = append(toolMessages, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": asString(bm["tool_use_id"]),
				"content":      claudeToolResultContentToOpenAI(bm["content"]),
			})
		case "text":
			if t := asString(bm["text"]); t != "" {
				texts = append(texts, t)
				parts = append(parts, map[string]interface{}{"type": "text", "text": t})
			}
		case "image":
			if part, ok := claudeImageBlockToOpenAIPart(bm); ok {
				parts = append(parts, part)
			}
		case "document":
			if t := claudeDocumentToText(bm); t != "" {
				texts = append(texts, t)
				parts = append(parts, map[string]interface{}{"type": "text", "text": t})
			}
		}
	}

	out := toolMessages
	if len(parts) > 0 {
		var payload interface{}
		// 纯文本时用字符串，兼容性最好；含图片时用 content parts 数组。
		if len(texts) == len(parts) {
			payload = strings.Join(texts, "\n")
		} else {
			payload = parts
		}
		out = append(out, map[string]interface{}{"role": "user", "content": payload})
	}
	return out
}

func claudeAssistantMessageToOpenAI(m map[string]interface{}) (interface{}, bool) {
	content := m["content"]
	if s, ok := content.(string); ok {
		if s == "" {
			return nil, false
		}
		return map[string]interface{}{"role": "assistant", "content": s}, true
	}

	texts := make([]string, 0)
	toolCalls := make([]interface{}, 0)
	for _, raw := range asSlice(content) {
		bm := asMap(raw)
		switch asString(bm["type"]) {
		case "text":
			if t := asString(bm["text"]); t != "" {
				texts = append(texts, t)
			}
		case "tool_use":
			input := bm["input"]
			if input == nil {
				input = map[string]interface{}{}
			}
			args, _ := json.Marshal(input)
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   asString(bm["id"]),
				"type": "function",
				"function": map[string]interface{}{
					"name":      asString(bm["name"]),
					"arguments": string(args),
				},
			})
		}
	}

	if len(texts) == 0 && len(toolCalls) == 0 {
		return nil, false
	}
	msg := map[string]interface{}{"role": "assistant"}
	if len(texts) > 0 {
		msg["content"] = strings.Join(texts, "\n")
	} else {
		msg["content"] = nil
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	return msg, true
}

func claudeToolResultContentToOpenAI(v interface{}) interface{} {
	switch c := v.(type) {
	case string:
		return c
	case []interface{}:
		texts := make([]string, 0)
		parts := make([]interface{}, 0)
		hasNonText := false
		for _, raw := range c {
			bm := asMap(raw)
			switch asString(bm["type"]) {
			case "text":
				t := asString(bm["text"])
				texts = append(texts, t)
				parts = append(parts, map[string]interface{}{"type": "text", "text": t})
			case "image":
				if part, ok := claudeImageBlockToOpenAIPart(bm); ok {
					parts = append(parts, part)
					hasNonText = true
				}
			}
		}
		if hasNonText {
			return parts
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

func claudeImageBlockToOpenAIPart(bm map[string]interface{}) (interface{}, bool) {
	src := asMap(bm["source"])
	switch asString(src["type"]) {
	case "base64":
		data := asString(src["data"])
		mediaType := asString(src["media_type"])
		if data == "" || mediaType == "" {
			return nil, false
		}
		return map[string]interface{}{
			"type": "image_url",
			"image_url": map[string]interface{}{
				"url": "data:" + mediaType + ";base64," + data,
			},
		}, true
	case "url":
		u := asString(src["url"])
		if u == "" {
			return nil, false
		}
		return map[string]interface{}{
			"type":      "image_url",
			"image_url": map[string]interface{}{"url": u},
		}, true
	}
	return nil, false
}

// claudeDocumentToText 只转换纯文本文档；PDF/base64 文档在 Chat Completions
// 中没有对应字段，直接丢弃，避免把二进制塞进提示词。
func claudeDocumentToText(bm map[string]interface{}) string {
	src := asMap(bm["source"])
	switch asString(src["type"]) {
	case "text":
		return asString(src["data"])
	case "content":
		texts := make([]string, 0)
		for _, raw := range asSlice(src["content"]) {
			child := asMap(raw)
			if asString(child["type"]) == "text" {
				if t := asString(child["text"]); t != "" {
					texts = append(texts, t)
				}
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

// ---------------------------------------------------------------------------
// OpenAI request -> Anthropic request
// ---------------------------------------------------------------------------

// convertOpenAIToClaudeRequest 将 OpenAI Chat Completions 请求转换为 Anthropic Messages 请求。
// 关键映射：
//   - max_completion_tokens（优先）或 max_tokens -> max_tokens（Anthropic 必填，缺失时兜底 4096）
//   - stop -> stop_sequences
//   - system/developer 消息 -> 顶层 system
//   - user/assistant 内容 parts -> text/image 内容块
//   - assistant.tool_calls -> tool_use 内容块
//   - role=tool 消息 -> user 消息里的 tool_result 内容块（连续多条合并为一条 user 消息）
//   - tools[].function.parameters -> tools[].input_schema
//   - user -> metadata.user_id
//   - temperature 从 OpenAI 的 [0,2] 夹到 Anthropic 的 [0,1]
func convertOpenAIToClaudeRequest(openaiReq map[string]interface{}) map[string]interface{} {
	claudeReq := make(map[string]interface{})

	for _, key := range []string{"model", "stream"} {
		if v, ok := openaiReq[key]; ok {
			claudeReq[key] = v
		}
	}

	if f, ok := asFloat(openaiReq["temperature"]); ok {
		if f < 0 {
			f = 0
		}
		if f > 1 {
			f = 1
		}
		claudeReq["temperature"] = f
	}
	if f, ok := asFloat(openaiReq["top_p"]); ok {
		if f < 0 {
			f = 0
		}
		if f > 1 {
			f = 1
		}
		claudeReq["top_p"] = f
	}

	switch {
	case openaiReq["max_completion_tokens"] != nil:
		claudeReq["max_tokens"] = openaiReq["max_completion_tokens"]
	case openaiReq["max_tokens"] != nil:
		claudeReq["max_tokens"] = openaiReq["max_tokens"]
	default:
		claudeReq["max_tokens"] = defaultClaudeMaxTokens
	}

	switch stop := openaiReq["stop"].(type) {
	case string:
		if stop != "" {
			claudeReq["stop_sequences"] = []interface{}{stop}
		}
	case []interface{}:
		if len(stop) > 0 {
			claudeReq["stop_sequences"] = stop
		}
	}

	if uid := asString(openaiReq["user"]); uid != "" {
		claudeReq["metadata"] = map[string]interface{}{"user_id": uid}
	}

	// OpenAI reasoning_effort 与 Anthropic output_config.effort 对应。
	if effort := asString(openaiReq["reasoning_effort"]); effort != "" {
		claudeReq["output_config"] = map[string]interface{}{"effort": effort}
	}

	// tool_choice=none 在 Anthropic 没有对应值，直接不传 tools，确保不会触发工具调用。
	dropTools := asString(openaiReq["tool_choice"]) == "none"
	if !dropTools {
		if tools := asSlice(openaiReq["tools"]); len(tools) > 0 {
			out := make([]interface{}, 0, len(tools))
			for _, raw := range tools {
				tm := asMap(raw)
				if asString(tm["type"]) != "function" {
					continue
				}
				fn := asMap(tm["function"])
				name := asString(fn["name"])
				if name == "" {
					continue
				}
				tool := map[string]interface{}{"name": name}
				if desc := asString(fn["description"]); desc != "" {
					tool["description"] = desc
				}
				if params, ok := fn["parameters"]; ok {
					tool["input_schema"] = params
				} else {
					tool["input_schema"] = map[string]interface{}{
						"type":       "object",
						"properties": map[string]interface{}{},
					}
				}
				out = append(out, tool)
			}
			if len(out) > 0 {
				claudeReq["tools"] = out
			}
		}

		if choice, ok := openAIToolChoiceToClaude(openaiReq["tool_choice"]); ok {
			if choiceMap, isMap := choice.(map[string]interface{}); isMap {
				if parallel, ok := openaiReq["parallel_tool_calls"].(bool); ok && !parallel {
					choiceMap["disable_parallel_tool_use"] = true
				}
			}
			claudeReq["tool_choice"] = choice
		}
	}

	system, messages := openAIMessagesToClaude(openaiReq["messages"])
	if system != "" {
		claudeReq["system"] = system
	}
	claudeReq["messages"] = messages

	return claudeReq
}

func openAIToolChoiceToClaude(v interface{}) (interface{}, bool) {
	switch tc := v.(type) {
	case string:
		switch tc {
		case "auto":
			return map[string]interface{}{"type": "auto"}, true
		case "required":
			return map[string]interface{}{"type": "any"}, true
		}
	case map[string]interface{}:
		if asString(tc["type"]) != "function" {
			return nil, false
		}
		name := asString(asMap(tc["function"])["name"])
		if name == "" {
			return nil, false
		}
		return map[string]interface{}{"type": "tool", "name": name}, true
	}
	return nil, false
}

func openAIMessagesToClaude(raw interface{}) (string, []interface{}) {
	systemTexts := make([]string, 0)
	out := make([]interface{}, 0)
	pendingToolResults := make([]interface{}, 0)

	flushToolResults := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		out = append(out, map[string]interface{}{
			"role":    "user",
			"content": pendingToolResults,
		})
		pendingToolResults = make([]interface{}, 0)
	}

	for _, rawMsg := range asSlice(raw) {
		m := asMap(rawMsg)
		switch asString(m["role"]) {
		case "system", "developer":
			flushToolResults()
			if t := openAIContentToText(m["content"]); t != "" {
				systemTexts = append(systemTexts, t)
			}
		case "user":
			flushToolResults()
			if blocks, ok := openAIContentToClaudeBlocks(m["content"]); ok {
				out = append(out, map[string]interface{}{"role": "user", "content": blocks})
			}
		case "assistant":
			flushToolResults()
			blocks := make([]interface{}, 0)
			if contentBlocks, ok := openAIContentToClaudeBlocks(m["content"]); ok {
				blocks = append(blocks, contentBlocks...)
			}
			for i, rawCall := range asSlice(m["tool_calls"]) {
				cm := asMap(rawCall)
				fn := asMap(cm["function"])
				id := asString(cm["id"])
				if id == "" {
					id = fmt.Sprintf("toolu_%d", i)
				}
				input := interface{}(map[string]interface{}{})
				switch args := fn["arguments"].(type) {
				case string:
					if strings.TrimSpace(args) != "" {
						var parsed interface{}
						if err := json.Unmarshal([]byte(args), &parsed); err == nil {
							input = parsed
						}
					}
				case map[string]interface{}:
					input = args
				}
				blocks = append(blocks, map[string]interface{}{
					"type":  "tool_use",
					"id":    id,
					"name":  asString(fn["name"]),
					"input": input,
				})
			}
			if len(blocks) > 0 {
				out = append(out, map[string]interface{}{"role": "assistant", "content": blocks})
			}
		case "tool":
			pendingToolResults = append(pendingToolResults, map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": asString(m["tool_call_id"]),
				"content":     openAIToolContentToClaude(m["content"]),
			})
		case "function":
			// 旧版 OpenAI functions 返回值没有 tool_call_id，降级为普通用户文本。
			flushToolResults()
			if t := openAIContentToText(m["content"]); t != "" {
				out = append(out, map[string]interface{}{
					"role":    "user",
					"content": []interface{}{map[string]interface{}{"type": "text", "text": t}},
				})
			}
		}
	}
	flushToolResults()

	return strings.Join(systemTexts, "\n\n"), out
}

func openAIContentToText(v interface{}) string {
	switch c := v.(type) {
	case string:
		return c
	case []interface{}:
		texts := make([]string, 0)
		for _, raw := range c {
			pm := asMap(raw)
			if asString(pm["type"]) == "text" {
				if t := asString(pm["text"]); t != "" {
					texts = append(texts, t)
				}
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

func openAIContentToClaudeBlocks(v interface{}) ([]interface{}, bool) {
	switch c := v.(type) {
	case string:
		if c == "" {
			return nil, false
		}
		return []interface{}{map[string]interface{}{"type": "text", "text": c}}, true
	case []interface{}:
		blocks := make([]interface{}, 0, len(c))
		for _, raw := range c {
			pm := asMap(raw)
			switch asString(pm["type"]) {
			case "text":
				if t := asString(pm["text"]); t != "" {
					blocks = append(blocks, map[string]interface{}{"type": "text", "text": t})
				}
			case "image_url":
				if block, ok := openAIImagePartToClaude(pm); ok {
					blocks = append(blocks, block)
				}
			}
		}
		if len(blocks) == 0 {
			return nil, false
		}
		return blocks, true
	}
	return nil, false
}

func openAIToolContentToClaude(v interface{}) interface{} {
	switch c := v.(type) {
	case string:
		return c
	case []interface{}:
		texts := make([]string, 0)
		blocks := make([]interface{}, 0)
		for _, raw := range c {
			pm := asMap(raw)
			switch asString(pm["type"]) {
			case "text":
				t := asString(pm["text"])
				texts = append(texts, t)
				blocks = append(blocks, map[string]interface{}{"type": "text", "text": t})
			case "image_url":
				if block, ok := openAIImagePartToClaude(pm); ok {
					blocks = append(blocks, block)
				}
			}
		}
		if len(blocks) == 0 {
			return ""
		}
		if len(texts) == len(blocks) {
			return strings.Join(texts, "\n")
		}
		return blocks
	}
	return ""
}

func openAIImagePartToClaude(pm map[string]interface{}) (interface{}, bool) {
	url := asString(asMap(pm["image_url"])["url"])
	if url == "" {
		return nil, false
	}
	if strings.HasPrefix(url, "data:") {
		rest := strings.TrimPrefix(url, "data:")
		comma := strings.Index(rest, ",")
		if comma < 0 {
			return nil, false
		}
		meta := rest[:comma]
		data := rest[comma+1:]
		mediaType := meta
		if semi := strings.Index(meta, ";"); semi >= 0 {
			mediaType = meta[:semi]
		}
		if mediaType == "" {
			mediaType = "image/png"
		}
		return map[string]interface{}{
			"type": "image",
			"source": map[string]interface{}{
				"type":       "base64",
				"media_type": mediaType,
				"data":       data,
			},
		}, true
	}
	return map[string]interface{}{
		"type":   "image",
		"source": map[string]interface{}{"type": "url", "url": url},
	}, true
}

// ---------------------------------------------------------------------------
// 非流式响应
// ---------------------------------------------------------------------------

func claudeStopReasonToOpenAI(v interface{}) interface{} {
	switch s := asString(v); s {
	case "":
		return nil
	case "end_turn", "stop_sequence", "pause_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return s
	}
}

func openAIFinishReasonToClaude(v interface{}) interface{} {
	switch s := asString(v); s {
	case "":
		return nil
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "end_turn"
	default:
		return s
	}
}

// convertClaudeToOpenAIResponse 将 Anthropic Messages 响应转换为 OpenAI Chat Completion 响应。
func convertClaudeToOpenAIResponse(claudeResp map[string]interface{}) map[string]interface{} {
	openaiResp := map[string]interface{}{
		"id":      claudeResp["id"],
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   claudeResp["model"],
	}

	message := map[string]interface{}{"role": "assistant"}
	texts := make([]string, 0)
	toolCalls := make([]interface{}, 0)
	for _, raw := range asSlice(claudeResp["content"]) {
		block := asMap(raw)
		switch asString(block["type"]) {
		case "text":
			if t := asString(block["text"]); t != "" {
				texts = append(texts, t)
			}
		case "tool_use":
			input := block["input"]
			if input == nil {
				input = map[string]interface{}{}
			}
			args, _ := json.Marshal(input)
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   asString(block["id"]),
				"type": "function",
				"function": map[string]interface{}{
					"name":      asString(block["name"]),
					"arguments": string(args),
				},
			})
		}
	}

	content := strings.Join(texts, "")
	if content != "" || len(toolCalls) == 0 {
		message["content"] = content
	} else {
		message["content"] = nil
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	openaiResp["choices"] = []interface{}{
		map[string]interface{}{
			"index":         0,
			"message":       message,
			"finish_reason": claudeStopReasonToOpenAI(claudeResp["stop_reason"]),
		},
	}
	openaiResp["usage"] = claudeUsageToOpenAI(claudeResp["usage"])
	return openaiResp
}

// convertOpenAIToClaude 将 OpenAI Chat Completion 响应转换为 Anthropic Messages 响应。
func convertOpenAIToClaude(openaiResp map[string]interface{}) map[string]interface{} {
	claudeResp := map[string]interface{}{
		"id":            openaiResp["id"],
		"type":          "message",
		"role":          "assistant",
		"model":         openaiResp["model"],
		"stop_sequence": nil,
	}

	content := make([]interface{}, 0)
	choices := asSlice(openaiResp["choices"])
	if len(choices) > 0 {
		choice := asMap(choices[0])
		message := asMap(choice["message"])

		switch c := message["content"].(type) {
		case string:
			if c != "" {
				content = append(content, map[string]interface{}{"type": "text", "text": c})
			}
		case []interface{}:
			for _, raw := range c {
				part := asMap(raw)
				if asString(part["type"]) == "text" {
					if t := asString(part["text"]); t != "" {
						content = append(content, map[string]interface{}{"type": "text", "text": t})
					}
				}
			}
		}

		if refusal := asString(message["refusal"]); refusal != "" {
			content = append(content, map[string]interface{}{"type": "text", "text": refusal})
		}

		for i, raw := range asSlice(message["tool_calls"]) {
			cm := asMap(raw)
			fn := asMap(cm["function"])
			id := asString(cm["id"])
			if id == "" {
				id = fmt.Sprintf("toolu_%d", i)
			}
			input := interface{}(map[string]interface{}{})
			switch args := fn["arguments"].(type) {
			case string:
				if strings.TrimSpace(args) != "" {
					var parsed interface{}
					if err := json.Unmarshal([]byte(args), &parsed); err == nil {
						input = parsed
					}
				}
			case map[string]interface{}:
				input = args
			}
			content = append(content, map[string]interface{}{
				"type":  "tool_use",
				"id":    id,
				"name":  asString(fn["name"]),
				"input": input,
			})
		}

		claudeResp["stop_reason"] = openAIFinishReasonToClaude(choice["finish_reason"])
	}

	claudeResp["content"] = content
	claudeResp["usage"] = openAIUsageToClaude(openaiResp["usage"])
	return claudeResp
}

func claudeUsageToOpenAI(v interface{}) map[string]interface{} {
	usage := asMap(v)
	inputTokens, _ := asInt(usage["input_tokens"])
	outputTokens, _ := asInt(usage["output_tokens"])
	result := map[string]interface{}{
		"prompt_tokens":     inputTokens,
		"completion_tokens": outputTokens,
		"total_tokens":      inputTokens + outputTokens,
	}
	if cached, ok := asInt(usage["cache_read_input_tokens"]); ok && cached > 0 {
		result["prompt_tokens_details"] = map[string]interface{}{"cached_tokens": cached}
	}
	return result
}

func openAIUsageToClaude(v interface{}) map[string]interface{} {
	usage := asMap(v)
	inputTokens, _ := asInt(usage["prompt_tokens"])
	outputTokens, _ := asInt(usage["completion_tokens"])
	result := map[string]interface{}{
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
	}
	if details := asMap(usage["prompt_tokens_details"]); details != nil {
		if cached, ok := asInt(details["cached_tokens"]); ok && cached > 0 {
			result["cache_read_input_tokens"] = cached
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// 流式转换
// ---------------------------------------------------------------------------

// sseDataPayload 从一行 SSE 中取出 data 负载；非 data 行返回 false。
func sseDataPayload(line string) (string, bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "data:")), true
}

func anthropicSSEFrame(event string, payload interface{}) string {
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return "event: " + event + "\ndata: " + string(data) + "\n\n"
}

func openAIStreamFrame(payload interface{}) string {
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return "data: " + string(data) + "\n\n"
}

const openAIDoneFrame = "data: [DONE]\n\n"

// openAIToClaudeStream 把 OpenAI Chat Completions 的流式 chunk 转成 Anthropic SSE 事件。
type openAIToClaudeStream struct {
	started          bool
	finished         bool
	id               string
	model            string
	nextBlock        int
	openBlock        int
	textBlockOpen    bool
	toolBlockByCall  map[int]int
	stopReason       string
	inputTokens      int
	outputTokens     int
	messageDeltaSent bool
}

func newOpenAIToClaudeStream() *openAIToClaudeStream {
	return &openAIToClaudeStream{
		openBlock:       -1,
		toolBlockByCall: map[int]int{},
	}
}

func (s *openAIToClaudeStream) ensureStarted() []string {
	if s.started {
		return nil
	}
	s.started = true
	if s.id == "" {
		s.id = "msg_" + uuid.NewString()
	}
	if s.model == "" {
		s.model = "unknown"
	}
	return []string{anthropicSSEFrame("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            s.id,
			"type":          "message",
			"role":          "assistant",
			"model":         s.model,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]interface{}{"input_tokens": s.inputTokens, "output_tokens": 0},
		},
	})}
}

func (s *openAIToClaudeStream) closeOpenBlock() []string {
	if s.openBlock < 0 {
		return nil
	}
	index := s.openBlock
	s.openBlock = -1
	s.textBlockOpen = false
	return []string{anthropicSSEFrame("content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": index,
	})}
}

func (s *openAIToClaudeStream) textDeltaFrames(text string) []string {
	if text == "" {
		return nil
	}
	var frames []string
	if !s.textBlockOpen {
		frames = append(frames, s.closeOpenBlock()...)
		s.openBlock = s.nextBlock
		s.nextBlock++
		s.textBlockOpen = true
		frames = append(frames, anthropicSSEFrame("content_block_start", map[string]interface{}{
			"type":          "content_block_start",
			"index":         s.openBlock,
			"content_block": map[string]interface{}{"type": "text", "text": ""},
		}))
	}
	frames = append(frames, anthropicSSEFrame("content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": s.openBlock,
		"delta": map[string]interface{}{"type": "text_delta", "text": text},
	}))
	return frames
}

// Feed 处理一条 OpenAI SSE data payload，返回要写给客户端的 Anthropic SSE 帧。
func (s *openAIToClaudeStream) Feed(data string) []string {
	if s.finished {
		return nil
	}
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil
	}

	if id := asString(chunk["id"]); id != "" {
		s.id = id
	}
	if model := asString(chunk["model"]); model != "" {
		s.model = model
	}
	if usage := asMap(chunk["usage"]); usage != nil {
		if v, ok := asInt(usage["prompt_tokens"]); ok {
			s.inputTokens = v
		}
		if v, ok := asInt(usage["completion_tokens"]); ok {
			s.outputTokens = v
		}
	}

	if errObj := asMap(chunk["error"]); errObj != nil {
		frames := s.ensureStarted()
		frames = append(frames, anthropicSSEFrame("error", map[string]interface{}{
			"type": "error",
			"error": map[string]interface{}{
				"type":    "api_error",
				"message": asString(errObj["message"]),
			},
		}))
		s.finished = true
		return frames
	}

	choices := asSlice(chunk["choices"])
	if len(choices) == 0 {
		return nil
	}

	frames := s.ensureStarted()
	choice := asMap(choices[0])
	delta := asMap(choice["delta"])

	if content := asString(delta["content"]); content != "" {
		frames = append(frames, s.textDeltaFrames(content)...)
	}
	if refusal := asString(delta["refusal"]); refusal != "" {
		frames = append(frames, s.textDeltaFrames(refusal)...)
	}

	for _, raw := range asSlice(delta["tool_calls"]) {
		call := asMap(raw)
		callIndex := 0
		if v, ok := asInt(call["index"]); ok {
			callIndex = v
		}
		fn := asMap(call["function"])
		block, exists := s.toolBlockByCall[callIndex]
		if !exists {
			frames = append(frames, s.closeOpenBlock()...)
			block = s.nextBlock
			s.nextBlock++
			s.toolBlockByCall[callIndex] = block
			s.openBlock = block
			s.textBlockOpen = false
			id := asString(call["id"])
			if id == "" {
				id = "toolu_" + uuid.NewString()
			}
			frames = append(frames, anthropicSSEFrame("content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": block,
				"content_block": map[string]interface{}{
					"type":  "tool_use",
					"id":    id,
					"name":  asString(fn["name"]),
					"input": map[string]interface{}{},
				},
			}))
		}
		if args := asString(fn["arguments"]); args != "" {
			frames = append(frames, anthropicSSEFrame("content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": block,
				"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": args},
			}))
		}
	}

	if finishReason := choice["finish_reason"]; finishReason != nil {
		if s.stopReason == "" {
			s.stopReason = asString(finishReason)
		}
		frames = append(frames, s.closeOpenBlock()...)
	}
	return frames
}

// Finish 在流结束时补齐 message_delta 和 message_stop。
func (s *openAIToClaudeStream) Finish() []string {
	if s.finished {
		return nil
	}
	frames := s.ensureStarted()
	frames = append(frames, s.closeOpenBlock()...)
	if !s.messageDeltaSent {
		stopReason := s.stopReason
		if stopReason == "" {
			stopReason = "end_turn"
		}
		frames = append(frames, anthropicSSEFrame("message_delta", map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason":   claudeStopReasonForDelta(stopReason),
				"stop_sequence": nil,
			},
			"usage": map[string]interface{}{
				"input_tokens":  s.inputTokens,
				"output_tokens": s.outputTokens,
			},
		}))
		s.messageDeltaSent = true
	}
	frames = append(frames, anthropicSSEFrame("message_stop", map[string]interface{}{"type": "message_stop"}))
	s.finished = true
	return frames
}

func (s *openAIToClaudeStream) Usage() (int, int) {
	return s.inputTokens, s.outputTokens
}

func (s *openAIToClaudeStream) ModelName() string {
	return s.model
}

// claudeStopReasonForDelta 把 OpenAI finish_reason 映射为 Anthropic stop_reason。
func claudeStopReasonForDelta(finishReason string) interface{} {
	switch finishReason {
	case "", "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "end_turn"
	default:
		return finishReason
	}
}

// claudeToOpenAIStream 把 Anthropic SSE 事件转成 OpenAI Chat Completions 流式 chunk。
type claudeToOpenAIStream struct {
	started      bool
	finished     bool
	id           string
	model        string
	created      int64
	inputTokens  int
	outputTokens int
	blockToTool  map[int]int
	nextTool     int
	stopReason   string
	finishSent   bool
}

func newClaudeToOpenAIStream() *claudeToOpenAIStream {
	return &claudeToOpenAIStream{
		created:     time.Now().Unix(),
		blockToTool: map[int]int{},
	}
}

func (s *claudeToOpenAIStream) ensureStarted() []string {
	if s.started {
		return nil
	}
	s.started = true
	if s.id == "" {
		s.id = "chatcmpl-" + uuid.NewString()
	}
	if s.model == "" {
		s.model = "unknown"
	}
	return []string{openAIStreamFrame(s.chunk(map[string]interface{}{"role": "assistant"}, nil))}
}

func (s *claudeToOpenAIStream) chunk(delta map[string]interface{}, finishReason interface{}) map[string]interface{} {
	if delta == nil {
		delta = map[string]interface{}{}
	}
	return map[string]interface{}{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"delta":         delta,
				"finish_reason": finishReason,
			},
		},
	}
}

// Feed 处理一条 Anthropic SSE data payload，返回要写给客户端的 OpenAI SSE 帧。
func (s *claudeToOpenAIStream) Feed(data string) []string {
	if s.finished {
		return nil
	}
	var event map[string]interface{}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return nil
	}

	switch asString(event["type"]) {
	case "message_start":
		message := asMap(event["message"])
		if id := asString(message["id"]); id != "" {
			s.id = id
		}
		if model := asString(message["model"]); model != "" {
			s.model = model
		}
		if usage := asMap(message["usage"]); usage != nil {
			if v, ok := asInt(usage["input_tokens"]); ok {
				s.inputTokens = v
			}
			if v, ok := asInt(usage["output_tokens"]); ok {
				s.outputTokens = v
			}
		}
		return s.ensureStarted()

	case "content_block_start":
		frames := s.ensureStarted()
		block := asMap(event["content_block"])
		if asString(block["type"]) == "tool_use" {
			blockIndex, _ := asInt(event["index"])
			toolIndex := s.nextTool
			s.nextTool++
			s.blockToTool[blockIndex] = toolIndex
			frames = append(frames, openAIStreamFrame(s.chunk(map[string]interface{}{
				"tool_calls": []interface{}{
					map[string]interface{}{
						"index":    toolIndex,
						"id":       asString(block["id"]),
						"type":     "function",
						"function": map[string]interface{}{"name": asString(block["name"]), "arguments": ""},
					},
				},
			}, nil)))
		}
		return frames

	case "content_block_delta":
		frames := s.ensureStarted()
		delta := asMap(event["delta"])
		switch asString(delta["type"]) {
		case "text_delta":
			if text := asString(delta["text"]); text != "" {
				frames = append(frames, openAIStreamFrame(s.chunk(map[string]interface{}{"content": text}, nil)))
			}
		case "input_json_delta":
			blockIndex, _ := asInt(event["index"])
			if toolIndex, ok := s.blockToTool[blockIndex]; ok {
				if partial := asString(delta["partial_json"]); partial != "" {
					frames = append(frames, openAIStreamFrame(s.chunk(map[string]interface{}{
						"tool_calls": []interface{}{
							map[string]interface{}{
								"index":    toolIndex,
								"function": map[string]interface{}{"arguments": partial},
							},
						},
					}, nil)))
				}
			}
		}
		return frames

	case "message_delta":
		frames := s.ensureStarted()
		if stopReason := asString(asMap(event["delta"])["stop_reason"]); stopReason != "" {
			s.stopReason = stopReason
		}
		if usage := asMap(event["usage"]); usage != nil {
			if v, ok := asInt(usage["output_tokens"]); ok {
				s.outputTokens = v
			}
			if v, ok := asInt(usage["input_tokens"]); ok {
				s.inputTokens = v
			}
		}
		if !s.finishSent {
			frames = append(frames, openAIStreamFrame(s.chunk(nil, claudeStopReasonToOpenAI(s.stopReason))))
			s.finishSent = true
		}
		return frames

	case "message_stop":
		return s.Finish()

	case "error":
		errObj := asMap(event["error"])
		s.finished = true
		return []string{
			openAIStreamFrame(map[string]interface{}{
				"error": map[string]interface{}{
					"type":    "api_error",
					"message": asString(errObj["message"]),
				},
			}),
			openAIDoneFrame,
		}
	}

	// ping / content_block_stop 等事件对 OpenAI 客户端没有对应输出。
	return nil
}

// Finish 在流结束时补齐 finish_reason、usage 和 [DONE]。
func (s *claudeToOpenAIStream) Finish() []string {
	if s.finished {
		return nil
	}
	frames := s.ensureStarted()
	if !s.finishSent {
		frames = append(frames, openAIStreamFrame(s.chunk(nil, claudeStopReasonToOpenAI(s.stopReason))))
		s.finishSent = true
	}
	frames = append(frames, openAIStreamFrame(map[string]interface{}{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []interface{}{},
		"usage": map[string]interface{}{
			"prompt_tokens":     s.inputTokens,
			"completion_tokens": s.outputTokens,
			"total_tokens":      s.inputTokens + s.outputTokens,
		},
	}))
	frames = append(frames, openAIDoneFrame)
	s.finished = true
	return frames
}

func (s *claudeToOpenAIStream) Usage() (int, int) {
	return s.inputTokens, s.outputTokens
}

func (s *claudeToOpenAIStream) ModelName() string {
	return s.model
}
