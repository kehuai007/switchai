package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
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
