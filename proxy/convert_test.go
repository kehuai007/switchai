package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	return m
}

func parseSSEPayloads(frames []string) []map[string]interface{} {
	out := make([]map[string]interface{}, 0)
	for _, frame := range frames {
		for _, line := range strings.Split(frame, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			var m map[string]interface{}
			if json.Unmarshal([]byte(payload), &m) == nil {
				out = append(out, m)
			}
		}
	}
	return out
}

func TestConvertClaudeToOpenAIRequest(t *testing.T) {
	req := mustJSON(t, `{
		"model": "claude-sonnet-4-5",
		"max_tokens": 1024,
		"temperature": 0.7,
		"top_k": 40,
		"stop_sequences": ["END"],
		"metadata": {"user_id": "u1"},
		"output_config": {"effort": "high"},
		"system": [
			{"type": "text", "text": "sys1"},
			{"type": "text", "text": "sys2", "cache_control": {"type": "ephemeral"}}
		],
		"tools": [{
			"name": "weather",
			"description": "get weather",
			"input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}
		}, {
			"type": "web_search_20250305",
			"name": "web_search"
		}],
		"tool_choice": {"type": "any", "disable_parallel_tool_use": true},
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "hi"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAA"}}
			]},
			{"role": "assistant", "content": [
				{"type": "text", "text": "calling"},
				{"type": "tool_use", "id": "t1", "name": "weather", "input": {"city": "SH"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t1", "content": "sunny"}
			]}
		]
	}`)

	out := convertClaudeToOpenAI(req)

	if _, exists := out["max_tokens"]; exists {
		t.Error("max_tokens must not be sent to OpenAI, use max_completion_tokens")
	}
	if got, _ := asInt(out["max_completion_tokens"]); got != 1024 {
		t.Errorf("max_completion_tokens = %v, want 1024", out["max_completion_tokens"])
	}
	if _, exists := out["top_k"]; exists {
		t.Error("top_k has no OpenAI equivalent and must be dropped")
	}
	if out["user"] != "u1" {
		t.Errorf("user = %v, want u1", out["user"])
	}
	if stops := asSlice(out["stop"]); len(stops) != 1 || stops[0] != "END" {
		t.Errorf("stop = %v, want [END]", out["stop"])
	}
	if out["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v, want high", out["reasoning_effort"])
	}
	if got := len(asSlice(out["tools"])); got != 1 {
		t.Errorf("tools len = %d, want 1 (server tool must be skipped)", got)
	}

	msgs := asSlice(out["messages"])
	if len(msgs) != 4 {
		t.Fatalf("messages len = %d, want 4", len(msgs))
	}
	sys := asMap(msgs[0])
	if sys["role"] != "system" || sys["content"] != "sys1\nsys2" {
		t.Errorf("system message = %#v", sys)
	}
	user := asMap(msgs[1])
	if user["role"] != "user" {
		t.Errorf("user role = %v", user["role"])
	}
	userParts := asSlice(user["content"])
	if len(userParts) != 2 {
		t.Fatalf("user content parts = %d, want 2", len(userParts))
	}
	if asMap(userParts[1])["type"] != "image_url" {
		t.Errorf("image part = %#v", userParts[1])
	}
	if url := asString(asMap(asMap(userParts[1])["image_url"])["url"]); url != "data:image/png;base64,AAA" {
		t.Errorf("image url = %q", url)
	}

	assistant := asMap(msgs[2])
	if assistant["content"] != "calling" {
		t.Errorf("assistant content = %v", assistant["content"])
	}
	calls := asSlice(assistant["tool_calls"])
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(calls))
	}
	fn := asMap(asMap(calls[0])["function"])
	if fn["name"] != "weather" {
		t.Errorf("tool name = %v", fn["name"])
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(asString(fn["arguments"])), &args); err != nil || args["city"] != "SH" {
		t.Errorf("tool arguments = %v (err=%v)", fn["arguments"], err)
	}

	toolMsg := asMap(msgs[3])
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "t1" || toolMsg["content"] != "sunny" {
		t.Errorf("tool message = %#v", toolMsg)
	}

	if out["tool_choice"] != "required" {
		t.Errorf("tool_choice = %v, want required", out["tool_choice"])
	}
	if out["parallel_tool_calls"] != false {
		t.Errorf("parallel_tool_calls = %v, want false", out["parallel_tool_calls"])
	}
	oSchema := asMap(asMap(asSlice(out["tools"])[0])["function"])["parameters"]
	if asMap(oSchema)["type"] != "object" {
		t.Errorf("function parameters = %#v", oSchema)
	}
}

func TestConvertOpenAIToClaudeRequest(t *testing.T) {
	req := mustJSON(t, `{
		"model": "gpt-6",
		"max_completion_tokens": 2048,
		"temperature": 1.6,
		"stop": "DONE",
		"user": "u2",
		"reasoning_effort": "high",
		"parallel_tool_calls": false,
		"tools": [{
			"type": "function",
			"function": {"name": "calc", "description": "calculator", "parameters": {"type": "object"}}
		}],
		"tool_choice": {"type": "function", "function": {"name": "calc"}},
		"messages": [
			{"role": "system", "content": "sys A"},
			{"role": "developer", "content": [{"type": "text", "text": "sys B"}]},
			{"role": "user", "content": [
				{"type": "text", "text": "hi"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,BBB"}}
			]},
			{"role": "assistant", "content": "using", "tool_calls": [
				{"id": "c1", "type": "function", "function": {"name": "calc", "arguments": "{\"x\":1}"}}
			]},
			{"role": "tool", "tool_call_id": "c1", "content": "2"}
		]
	}`)

	out := convertOpenAIToClaudeRequest(req)

	if got, _ := asInt(out["max_tokens"]); got != 2048 {
		t.Errorf("max_tokens = %v, want 2048", out["max_tokens"])
	}
	if _, exists := out["max_completion_tokens"]; exists {
		t.Error("max_completion_tokens must not be forwarded to Anthropic")
	}
	if got, _ := asFloat(out["temperature"]); got != 1 {
		t.Errorf("temperature = %v, want clamped to 1", got)
	}
	if out["system"] != "sys A\n\nsys B" {
		t.Errorf("system = %q", out["system"])
	}
	if md := asMap(out["metadata"]); md["user_id"] != "u2" {
		t.Errorf("metadata = %#v", out["metadata"])
	}
	if stops := asSlice(out["stop_sequences"]); len(stops) != 1 || stops[0] != "DONE" {
		t.Errorf("stop_sequences = %v", out["stop_sequences"])
	}
	if oc := asMap(out["output_config"]); oc["effort"] != "high" {
		t.Errorf("output_config = %#v", out["output_config"])
	}

	tool := asMap(asSlice(out["tools"])[0])
	if tool["name"] != "calc" || asMap(tool["input_schema"])["type"] != "object" {
		t.Errorf("tool = %#v", tool)
	}
	tc := asMap(out["tool_choice"])
	if tc["type"] != "tool" || tc["name"] != "calc" || tc["disable_parallel_tool_use"] != true {
		t.Errorf("tool_choice = %#v", tc)
	}

	msgs := asSlice(out["messages"])
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d, want 3 (user, assistant, tool_result)", len(msgs))
	}
	userBlocks := asSlice(asMap(msgs[0])["content"])
	if len(userBlocks) != 2 || asString(asMap(userBlocks[0])["type"]) != "text" {
		t.Errorf("user blocks = %#v", userBlocks)
	}
	img := asMap(userBlocks[1])
	if asString(img["type"]) != "image" {
		t.Errorf("image block = %#v", img)
	}
	if src := asMap(img["source"]); src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != "BBB" {
		t.Errorf("image source = %#v", src)
	}

	assistantBlocks := asSlice(asMap(msgs[1])["content"])
	if len(assistantBlocks) != 2 {
		t.Fatalf("assistant blocks = %d, want text + tool_use", len(assistantBlocks))
	}
	toolUse := asMap(assistantBlocks[1])
	if toolUse["type"] != "tool_use" || toolUse["id"] != "c1" || toolUse["name"] != "calc" {
		t.Errorf("tool_use = %#v", toolUse)
	}
	if in := asMap(toolUse["input"]); in["x"] != float64(1) {
		t.Errorf("tool_use input = %#v", in)
	}

	toolResultMsg := asMap(msgs[2])
	if toolResultMsg["role"] != "user" {
		t.Errorf("tool result role = %v", toolResultMsg["role"])
	}
	tr := asMap(asSlice(toolResultMsg["content"])[0])
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "c1" || tr["content"] != "2" {
		t.Errorf("tool_result = %#v", tr)
	}
}

func TestConvertOpenAIToClaudeRequestDefaultMaxTokens(t *testing.T) {
	out := convertOpenAIToClaudeRequest(mustJSON(t, `{"model":"gpt","messages":[]}`))
	if got, _ := asInt(out["max_tokens"]); got != defaultClaudeMaxTokens {
		t.Errorf("max_tokens = %v, want default %d", out["max_tokens"], defaultClaudeMaxTokens)
	}
}

func TestResponseConversions(t *testing.T) {
	claudeResp := mustJSON(t, `{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude",
		"content": [
			{"type": "text", "text": "hello"},
			{"type": "tool_use", "id": "t1", "name": "calc", "input": {"x": 2}}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 10, "output_tokens": 5, "cache_read_input_tokens": 3}
	}`)
	o := convertClaudeToOpenAIResponse(claudeResp)
	choice := asMap(asSlice(o["choices"])[0])
	msg := asMap(choice["message"])
	if msg["content"] != "hello" {
		t.Errorf("content = %v", msg["content"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v", choice["finish_reason"])
	}
	calls := asSlice(msg["tool_calls"])
	if len(calls) != 1 || asMap(asMap(calls[0])["function"])["name"] != "calc" {
		t.Errorf("tool_calls = %#v", calls)
	}
	usage := asMap(o["usage"])
	if got, _ := asInt(usage["prompt_tokens"]); got != 10 {
		t.Errorf("prompt_tokens = %v", usage["prompt_tokens"])
	}
	if got, _ := asInt(asMap(usage["prompt_tokens_details"])["cached_tokens"]); got != 3 {
		t.Errorf("cached_tokens = %v", usage["prompt_tokens_details"])
	}

	openaiResp := mustJSON(t, `{
		"id": "chatcmpl_1", "object": "chat.completion", "model": "gpt",
		"choices": [{
			"index": 0,
			"message": {
				"role": "assistant",
				"content": "hi",
				"tool_calls": [{"id": "c1", "type": "function", "function": {"name": "calc", "arguments": "{\"x\":3}"}}]
			},
			"finish_reason": "length"
		}],
		"usage": {"prompt_tokens": 7, "completion_tokens": 4, "prompt_tokens_details": {"cached_tokens": 2}}
	}`)
	c := convertOpenAIToClaude(openaiResp)
	if c["type"] != "message" || c["role"] != "assistant" || c["stop_reason"] != "max_tokens" {
		t.Errorf("claude response head = %#v", c)
	}
	blocks := asSlice(c["content"])
	if len(blocks) != 2 || asString(asMap(blocks[0])["type"]) != "text" || asString(asMap(blocks[1])["type"]) != "tool_use" {
		t.Errorf("claude content = %#v", blocks)
	}
	if in := asMap(asMap(blocks[1])["input"]); in["x"] != float64(3) {
		t.Errorf("tool input = %#v", in)
	}
	if got, _ := asInt(asMap(c["usage"])["input_tokens"]); got != 7 {
		t.Errorf("input_tokens = %v", c["usage"])
	}
	if got, _ := asInt(asMap(c["usage"])["cache_read_input_tokens"]); got != 2 {
		t.Errorf("cache_read_input_tokens = %v", c["usage"])
	}
}

func TestStreamOpenAIToClaude(t *testing.T) {
	s := newOpenAIToClaudeStream()
	var frames []string
	for _, chunk := range []string{
		`{"id":"msg_1","model":"gpt-6","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`{"id":"msg_1","model":"gpt-6","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`,
		`{"id":"msg_1","model":"gpt-6","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
		`{"id":"msg_1","model":"gpt-6","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"calc","arguments":""}}]},"finish_reason":null}]}`,
		`{"id":"msg_1","model":"gpt-6","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"x\":1}"}}]},"finish_reason":null}]}`,
		`{"id":"msg_1","model":"gpt-6","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"msg_1","model":"gpt-6","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
	} {
		frames = append(frames, s.Feed(chunk)...)
	}
	frames = append(frames, s.Finish()...)

	all := strings.Join(frames, "")
	for _, want := range []string{
		"event: message_start",
		`"type":"text_delta"`,
		`"text":"Hel"`,
		`"type":"tool_use"`,
		`"name":"calc"`,
		`"type":"input_json_delta"`,
		`"partial_json":"{\"x\":1}"`,
		"event: message_delta",
		`"stop_reason":"tool_use"`,
		"event: message_stop",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing frame fragment %q in stream output", want)
		}
	}
	if strings.Count(all, "content_block_stop") < 2 {
		t.Errorf("expected text and tool blocks to be closed, got:\n%s", all)
	}
	in, out := s.Usage()
	if in != 10 || out != 5 {
		t.Errorf("usage = (%d,%d), want (10,5)", in, out)
	}
	if s.ModelName() != "gpt-6" {
		t.Errorf("model = %q", s.ModelName())
	}

	// 文本块必须在工具块开始前关闭。
	textStop := strings.Index(all, `"index":0`)
	toolStart := strings.Index(all, `"type":"tool_use"`)
	if textStop < 0 || toolStart < 0 || textStop > toolStart {
		t.Errorf("unexpected block ordering:\n%s", all)
	}
}

func TestStreamClaudeToOpenAI(t *testing.T) {
	s := newClaudeToOpenAIStream()
	var frames []string
	for _, event := range []string{
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude","usage":{"input_tokens":10,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"calc","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`,
		`{"type":"message_stop"}`,
	} {
		frames = append(frames, s.Feed(event)...)
	}

	all := strings.Join(frames, "")
	for _, want := range []string{
		`"role":"assistant"`,
		`"content":"Hi"`,
		`"tool_calls"`,
		`"id":"toolu_1"`,
		`"name":"calc"`,
		`"arguments":"{\"x\":1}"`,
		`"finish_reason":"tool_calls"`,
		`"prompt_tokens":10`,
		`"completion_tokens":4`,
		`"total_tokens":14`,
		"data: [DONE]",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing frame fragment %q in stream output", want)
		}
	}
	in, out := s.Usage()
	if in != 10 || out != 4 {
		t.Errorf("usage = (%d,%d), want (10,4)", in, out)
	}
	if s.ModelName() != "claude" {
		t.Errorf("model = %q", s.ModelName())
	}
}
