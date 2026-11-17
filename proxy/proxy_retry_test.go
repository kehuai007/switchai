package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestDoRequestWithRetry_FirstAttemptSucceeds 验证一次就成功时 finalAttempt=1, retries 计数=0
func TestDoRequestWithRetry_FirstAttemptSucceeds(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL, bytes.NewReader([]byte(`{}`)))
	resp, finalAttempt, err := doRequestWithRetry(req, []byte(`{}`), nil, 3)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	defer resp.Body.Close()
	if finalAttempt != 1 {
		t.Errorf("finalAttempt = %d, want 1 (first try succeeded)", finalAttempt)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1", got)
	}
}

// TestDoRequestWithRetry_RetriesOnOverloaded 验证 529 后重试，finalAttempt=2
func TestDoRequestWithRetry_RetriesOnOverloaded(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 2 {
			w.WriteHeader(529)
			io.WriteString(w, `{"error":{"type":"overloaded_error","message":"try again"}}`)
			return
		}
		w.WriteHeader(200)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL, bytes.NewReader([]byte(`{}`)))
	resp, finalAttempt, err := doRequestWithRetry(req, []byte(`{}`), nil, 3)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	defer resp.Body.Close()
	if finalAttempt != 2 {
		t.Errorf("finalAttempt = %d, want 2 (one retry then success)", finalAttempt)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("upstream hits = %d, want 2", got)
	}
}

// TestDoRequestWithRetry_ExhaustsRetries 验证重试耗尽后仍尝试 maxRetries 次，finalAttempt=maxRetries
func TestDoRequestWithRetry_ExhaustsRetries(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(529)
		io.WriteString(w, `{"error":{"type":"overloaded_error","message":"try again"}}`)
	}))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL, bytes.NewReader([]byte(`{}`)))
	resp, finalAttempt, err := doRequestWithRetry(req, []byte(`{}`), nil, 3)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	defer resp.Body.Close()
	if finalAttempt != 3 {
		t.Errorf("finalAttempt = %d, want 3 (exhausted)", finalAttempt)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("upstream hits = %d, want 3", got)
	}
}

// TestDoRequestWithRetry_ConnectionError 验证连不上时的 finalAttempt
func TestDoRequestWithRetry_ConnectionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // 立刻关闭让连接拒绝

	req, _ := http.NewRequest("POST", srv.URL, bytes.NewReader([]byte(`{}`)))
	_, finalAttempt, err := doRequestWithRetry(req, []byte(`{}`), nil, 2)
	if err == nil {
		t.Fatal("expected connection error")
	}
	if finalAttempt != 2 {
		t.Errorf("finalAttempt = %d, want 2 (retried once after first failure)", finalAttempt)
	}
}

// TestRewriteMaxTokensAsCompletion 验证 max_tokens 被改名且不残留；
// 已存在 max_completion_tokens 或没有 max_tokens 时不改写。
func TestRewriteMaxTokensAsCompletion(t *testing.T) {
	patched, ok := RewriteMaxTokensAsCompletion([]byte(`{"model":"gpt-6-sol","max_tokens":256}`))
	if !ok {
		t.Fatal("expected rewrite, got false")
	}
	var got map[string]interface{}
	if err := json.Unmarshal(patched, &got); err != nil {
		t.Fatalf("unmarshal patched: %v", err)
	}
	if _, exists := got["max_tokens"]; exists {
		t.Errorf("max_tokens should be removed, got %s", patched)
	}
	if got["max_completion_tokens"] != float64(256) {
		t.Errorf("max_completion_tokens = %v, want 256", got["max_completion_tokens"])
	}
	if got["model"] != "gpt-6-sol" {
		t.Errorf("model = %v, want gpt-6-sol", got["model"])
	}

	if _, ok := RewriteMaxTokensAsCompletion([]byte(`{"max_completion_tokens":16}`)); ok {
		t.Error("should not rewrite when max_tokens is absent")
	}
	if _, ok := RewriteMaxTokensAsCompletion([]byte(`{"max_tokens":16,"max_completion_tokens":8}`)); ok {
		t.Error("should not rewrite when max_completion_tokens already exists")
	}
	if _, ok := RewriteMaxTokensAsCompletion([]byte(`not-json`)); ok {
		t.Error("should not rewrite invalid JSON")
	}

	downgraded, ok := RewriteMaxCompletionTokensAsMaxTokens([]byte(`{"model":"old","max_completion_tokens":8}`))
	if !ok {
		t.Fatal("expected downgrade rewrite, got false")
	}
	var downgradedMap map[string]interface{}
	if err := json.Unmarshal(downgraded, &downgradedMap); err != nil {
		t.Fatalf("unmarshal downgraded: %v", err)
	}
	if _, exists := downgradedMap["max_completion_tokens"]; exists {
		t.Errorf("max_completion_tokens should be removed: %s", downgraded)
	}
	if got, _ := asInt(downgradedMap["max_tokens"]); got != 8 {
		t.Errorf("max_tokens = %v, want 8", downgradedMap["max_tokens"])
	}
}

// TestIsMaxTokensParamRejected 只对“明确点名 max_tokens 不支持”的 400 命中，
// 且不能把反向错误（点名 max_completion_tokens）误判为 max_tokens 被拒。
func TestIsMaxTokensParamRejected(t *testing.T) {
	forward := []byte(`{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.","type":"unsupported_parameter"}}`)
	reverse := []byte(`{"error":{"message":"Unsupported parameter: 'max_completion_tokens' is not supported. Use 'max_tokens' instead."}}`)

	if !IsMaxTokensParamRejected(http.StatusBadRequest, forward) {
		t.Error("expected forward max_tokens rejection to be detected")
	}
	if IsMaxTokensParamRejected(http.StatusBadRequest, reverse) {
		t.Error("reverse max_completion_tokens rejection must not be detected as max_tokens")
	}
	if IsMaxTokensParamRejected(http.StatusOK, forward) {
		t.Error("200 should never be detected")
	}
	if IsMaxTokensParamRejected(http.StatusBadRequest, []byte(`{"error":"model not found"}`)) {
		t.Error("unrelated 400 should not be detected")
	}
	if IsMaxTokensParamRejected(http.StatusBadRequest, []byte(`{"error":{"message":"Unknown parameter: 'max_tokens'."}}`)) != true {
		t.Error("unknown parameter form should be detected")
	}

	if !IsMaxCompletionTokensRejected(http.StatusBadRequest, reverse) {
		t.Error("expected reverse max_completion_tokens rejection to be detected")
	}
	if IsMaxCompletionTokensRejected(http.StatusBadRequest, forward) {
		t.Error("forward max_tokens rejection must not be detected as max_completion_tokens")
	}
}

// TestDoRequestWithRetry_MaxTokensFallback 验证上游第一次拒收 max_tokens 后，
// 第二次自动改用 max_completion_tokens 并成功。
func TestDoRequestWithRetry_MaxTokensFallback(t *testing.T) {
	var hits int32
	var secondBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		b, _ := io.ReadAll(r.Body)
		if n == 1 {
			if !strings.Contains(string(b), `"max_tokens"`) {
				t.Errorf("first attempt body = %s, want max_tokens", b)
			}
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.","type":"unsupported_parameter"}}`)
			return
		}
		secondBody = string(b)
		w.WriteHeader(200)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	body := []byte(`{"model":"gpt-6-sol","max_tokens":256,"messages":[]}`)
	req, _ := http.NewRequest("POST", srv.URL, bytes.NewReader(body))
	resp, finalAttempt, err := doRequestWithRetry(req, body, nil, 3)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if finalAttempt != 2 {
		t.Errorf("finalAttempt = %d, want 2 (one compat retry)", finalAttempt)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("upstream hits = %d, want 2", got)
	}
	if strings.Contains(secondBody, `"max_tokens"`) {
		t.Errorf("retry body still has max_tokens: %s", secondBody)
	}
	if !strings.Contains(secondBody, `"max_completion_tokens":256`) {
		t.Errorf("retry body missing max_completion_tokens: %s", secondBody)
	}
}

// TestDoRequestWithRetry_MaxCompletionTokensFallback 验证旧 OpenAI 兼容服务
// 拒收 max_completion_tokens 时，自动降级为 max_tokens 并成功。
func TestDoRequestWithRetry_MaxCompletionTokensFallback(t *testing.T) {
	var hits int32
	var secondBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		b, _ := io.ReadAll(r.Body)
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"Unsupported parameter: 'max_completion_tokens' is not supported. Use 'max_tokens' instead."}}`)
			return
		}
		secondBody = string(b)
		w.WriteHeader(200)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	body := []byte(`{"model":"old-model","max_completion_tokens":256,"messages":[]}`)
	req, _ := http.NewRequest("POST", srv.URL, bytes.NewReader(body))
	resp, finalAttempt, err := doRequestWithRetry(req, body, nil, 3)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if finalAttempt != 2 {
		t.Errorf("finalAttempt = %d, want 2 (one compat retry)", finalAttempt)
	}
	if strings.Contains(secondBody, `"max_completion_tokens"`) {
		t.Errorf("retry body still has max_completion_tokens: %s", secondBody)
	}
	if !strings.Contains(secondBody, `"max_tokens":256`) {
		t.Errorf("retry body missing max_tokens: %s", secondBody)
	}
}
