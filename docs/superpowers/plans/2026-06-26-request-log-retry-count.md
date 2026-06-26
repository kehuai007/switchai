# 请求日志展示重试次数 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 `history.RequestRecord` 上新增 `retry_count`（额外重试次数）字段，由 `proxy.doRequestWithRetry` 透传写入 SQLite，并在 `web/static/log.html` 表格中以「数字 + 重试高亮」形式展示。

**Architecture:** 改 `doRequestWithRetry` 用命名返回值带回最后一次 attempt；调用方计算 `finalAttempt-1` 作为额外重试次数，沿分发函数 → `handleStreamResponse` / `handleNonStreamResponse` 透传到 `RequestRecord{}`。DB 层加 `retry_count INTEGER NOT NULL DEFAULT 0` 列与 `ALTER TABLE` 迁移。`RecordSummary` 加字段同步给列表/WS/详情接口。前端表格新增一列，`.badge-retry` 样式标识重试过。

**Tech Stack:** Go 1.25 + Gin + SQLite (modernc.org/sqlite) + 原生 HTML/JS

**Spec:** `docs/superpowers/specs/2026-06-26-request-log-retry-count-design.md`

---

## 文件结构

修改：
- `history/history.go` — `RequestRecord`/`RecordSummary` 加字段；schema 加列 + `ALTER TABLE` 迁移；`AddRecord` INSERT 加列；`loadHomeCache`/`GetRecords`/`GetRecord`/`GetRecordsSummary` SELECT 加列；`handleBroadcast` msg 加 `retry_count`
- `proxy/proxy.go` — `doRequestWithRetry` 用命名返回值带回 `finalAttempt`；分发函数接收并透传；`handleStreamResponse`/`handleNonStreamResponse` 加 `retryCount` 参数；两处 `RequestRecord{}` 字面量加 `RetryCount`
- `web/static/log.html` — 表头/行渲染/colspan 同步从 12 → 13 列；新增 `.badge-retry` 样式；详情弹窗加「重试次数」一行

新建：
- `history/history_test.go` — `RecordSummary` 字段序列化 + DB round-trip 测试
- `proxy/proxy_retry_test.go` — `doRequestWithRetry` 各种 attempt 场景的单测

---

## 任务列表

- [ ] Task 1: history 包加 `RetryCount` 序列化与 DB round-trip 测试
- [ ] Task 2: `RequestRecord`/`RecordSummary` 加字段 + schema 列 + 迁移 + 所有 SELECT/INSERT 接入
- [ ] Task 3: `doRequestWithRetry` `finalAttempt` 语义单测（mock 上游）
- [ ] Task 4: `doRequestWithRetry` 改为命名返回值带回 `finalAttempt`
- [ ] Task 5: 调用链接入 `retryCount`（分发函数 + handle 函数 + `RequestRecord{}`）
- [ ] Task 6: `handleBroadcast` WS 推送消息加 `retry_count` 字段
- [ ] Task 7: 前端表格新增「重试」列 + `.badge-retry` 样式
- [ ] Task 8: 前端详情弹窗加「重试次数」字段
- [ ] Task 9: 端到端验证（编译/测试/启动/触发请求）

---

## Task 1: history 包加 `RetryCount` 序列化与 DB round-trip 测试

**Files:**
- Create: `history/history_test.go`

- [ ] **Step 1: 写失败测试**

```go
package history

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// resetForTest 把包级 db/broadcast/clients/homeCache 等状态重置，并在临时目录里重新初始化，
// 避免测试间污染。history 包用全局单例，每个测试都需要独立的数据目录。
// appdata 不读环境变量，参考 proxy_test.go 用 chdir 隔离。
func resetForTest(t *testing.T) {
	t.Helper()
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origCwd) })

	db = nil
	if broadcast != nil {
		close(broadcast)
	}
	broadcast = nil
	clients = nil
	homeCache = nil
	homeCacheTotal = 0
	history = nil
}

// TestRequestRecord_RetryCountJSONField 守护序列化：客户端依赖 `retry_count` JSON 字段。
func TestRequestRecord_RetryCountJSONField(t *testing.T) {
	r := RequestRecord{RetryCount: 2}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	v, ok := got["retry_count"]
	if !ok {
		t.Fatalf("retry_count field missing; json = %s", b)
	}
	if v.(float64) != 2 {
		t.Errorf("retry_count = %v, want 2", v)
	}
}

// TestRecordSummary_RetryCountJSONField 守护列表 API 序列化。
func TestRecordSummary_RetryCountJSONField(t *testing.T) {
	s := RecordSummary{RetryCount: 3}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !contains(b, `"retry_count":3`) {
		t.Errorf("retry_count not in summary JSON: %s", b)
	}
}

// TestAddRecord_PersistsRetryCount 端到端：写入 → 读出，retry_count 一致。
func TestAddRecord_PersistsRetryCount(t *testing.T) {
	resetForTest(t)
	if err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(Shutdown)

	id := "test-retry-" + filepath.Base(t.TempDir())
	r := RequestRecord{
		ID:           id,
		Timestamp:    nowUnixNano(),
		Method:       "POST",
		Path:         "/v1/messages",
		StatusCode:   200,
		Duration:     100,
		RetryCount:   2,
	}
	AddRecord(r)
	// Allow goroutine broadcast & home cache update
	time.Sleep(50 * time.Millisecond)

	got := GetRecord(id)
	if got == nil {
		t.Fatal("GetRecord returned nil after AddRecord")
	}
	if got.RetryCount != 2 {
		t.Errorf("GetRecord.RetryCount = %d, want 2", got.RetryCount)
	}

	summaries, _ := GetRecordsSummary(1, 20)
	found := false
	for _, s := range summaries {
		if s.ID == id {
			found = true
			if s.RetryCount != 2 {
				t.Errorf("RecordSummary.RetryCount = %d, want 2", s.RetryCount)
			}
		}
	}
	if !found {
		t.Errorf("record %s not in summary", id)
	}
}

func contains(haystack []byte, needle string) bool {
	return bytes.Contains(haystack, []byte(needle))
}

func nowUnixNano() time.Time { return time.Unix(0, time.Now().UnixNano()) }
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./history/... -run TestRequestRecord_RetryCountJSONField -v`
Expected: FAIL — `retry_count` 字段不存在

Run: `go test ./history/... -run TestRecordSummary_RetryCountJSONField -v`
Expected: FAIL — `retry_count` 字段不存在

Run: `go test ./history/... -run TestAddRecord_PersistsRetryCount -v`
Expected: FAIL — `RetryCount` 字段不存在 / DB 列不存在

- [ ] **Step 3: commit**

```bash
git add history/history_test.go
git commit -m "test(history): add RetryCount serialization & round-trip tests"
```

---

## Task 2: `RequestRecord`/`RecordSummary` 加字段 + schema 列 + 迁移 + 所有 SELECT/INSERT 接入

**Files:**
- Modify: `history/history.go:19-41` — RequestRecord struct
- Modify: `history/history.go:165-201` — schema + migration
- Modify: `history/history.go:319-328` — AddRecord INSERT
- Modify: `history/history.go:101-105, 123-125, 144-148` — loadHomeCache SELECT/scan
- Modify: `history/history.go:417-421, 439-441, 455-464` — GetRecords SELECT/scan
- Modify: `history/history.go:588-594, 599-616` — GetRecord SELECT/scan
- Modify: `history/history.go:380-398` — RecordSummary struct
- Modify: `history/history.go:495-513` — GetRecordsSummary summary 构造
- Modify: `history/history.go:527-531, 547-549, 555-570` — GetRecordsSummary SELECT/scan

- [ ] **Step 1: 给 `RequestRecord` 加 `RetryCount` 字段**

在 `history/history.go:39-40` 之间插入（`TotalTokens` 与 `Cost` 之间，或 `Cost` 之后均可——选 `Cost` 之后，便于排序）：

```go
	Cost            float64   `json:"cost"`
	RetryCount      int       `json:"retry_count"`
```

- [ ] **Step 2: 给 `RecordSummary` 加同名字段**

在 `history/history.go:397-398` 之间插入（`Cost` 之后）：

```go
	Cost        float64   `json:"cost"`
	RetryCount  int       `json:"retry_count"`
```

- [ ] **Step 3: schema 加列**

`history/history.go:188` 处 `cost REAL` 之后追加：

```go
		retry_count INTEGER NOT NULL DEFAULT 0
```

完整 schema 段（line 167-190）末尾应为：

```sql
CREATE TABLE IF NOT EXISTS history (
    id TEXT PRIMARY KEY,
    timestamp INTEGER NOT NULL,
    method TEXT,
    path TEXT,
    client_ip TEXT,
    key_id TEXT,
    provider TEXT,
    model TEXT,
    user_model TEXT DEFAULT '',
    status_code INTEGER,
    duration_ms INTEGER,
    request_body TEXT,
    response_body TEXT,
    request_headers TEXT,
    response_headers TEXT,
    request_size INTEGER,
    response_size INTEGER,
    input_tokens INTEGER,
    output_tokens INTEGER,
    total_tokens INTEGER,
    cost REAL,
    retry_count INTEGER NOT NULL DEFAULT 0
);
```

注：上面是给新建表用的。已有库走下面 Step 4 的迁移。

- [ ] **Step 4: `ALTER TABLE` 迁移**

`history/history.go:197-198`（既有 `user_model` 迁移之后）追加：

```go
// Migration: add user_model column if it doesn't exist
db.Exec("ALTER TABLE history ADD COLUMN user_model TEXT DEFAULT ''")

// Migration: add retry_count column if it doesn't exist
db.Exec("ALTER TABLE history ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0")
```

- [ ] **Step 5: `AddRecord` INSERT 加列**

`history/history.go:319-328` 改：

```go
_, err := db.Exec(`
    INSERT INTO history (id, timestamp, method, path, client_ip, key_id, provider, model, user_model,
        status_code, duration_ms, request_body, response_body, request_headers, response_headers,
        request_size, response_size, input_tokens, output_tokens, total_tokens, cost, retry_count)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
    record.ID, record.Timestamp.UnixNano(), record.Method, record.Path, record.ClientIP,
    record.KeyID, record.Provider, record.Model, record.UserModel, record.StatusCode, record.Duration,
    record.RequestBody, record.ResponseBody, reqHeaders, respHeaders,
    record.RequestSize, record.ResponseSize, record.InputTokens, record.OutputTokens,
    record.TotalTokens, record.Cost, record.RetryCount)
```

- [ ] **Step 6: `loadHomeCache` SELECT 加列**

`history/history.go:101-105` 改：

```go
rows, err := db.Query(`
    SELECT id, timestamp, method, path, client_ip, key_id, provider, model, user_model,
        status_code, duration_ms, request_body, response_body, request_headers, response_headers,
        request_size, response_size, input_tokens, output_tokens, total_tokens, cost, retry_count
    FROM history ORDER BY timestamp DESC LIMIT ?`, homeCacheSize)
```

`history/history.go:123-125` 改：

```go
err := rows.Scan(&r.ID, &timestamp, &method, &path, &clientIP, &keyID, &provider, &model, &userModel,
    &statusCode, &durationMs, &requestBody, &responseBody, &reqHeaders, &respHeaders,
    &requestSize, &responseSize, &inputTokens, &outputTokens, &totalTokens, &cost, &r.RetryCount)
```

`history/history.go:148` 之后无需额外代码（`r.RetryCount` 由 Scan 填好）。

- [ ] **Step 7: `GetRecords` SELECT 加列**

`history/history.go:417-421` 改：

```go
rows, err := db.Query(`
    SELECT id, timestamp, method, path, client_ip, key_id, provider, model, user_model,
        status_code, duration_ms, request_body, response_body, request_headers, response_headers,
        request_size, response_size, input_tokens, output_tokens, total_tokens, cost, retry_count
    FROM history ORDER BY timestamp DESC LIMIT ? OFFSET ?`, pageSize, offset)
```

`history/history.go:439-441` 改：

```go
err := rows.Scan(&r.ID, &timestamp, &method, &path, &clientIP, &keyID, &provider, &model, &userModel,
    &statusCode, &durationMs, &requestBody, &responseBody, &reqHeaders, &respHeaders,
    &requestSize, &responseSize, &inputTokens, &outputTokens, &totalTokens, &cost, &r.RetryCount)
```

- [ ] **Step 8: `GetRecord` SELECT 加列**

`history/history.go:588-594` 改：

```go
err := db.QueryRow(`
    SELECT id, timestamp, method, path, client_ip, key_id, provider, model, user_model,
        status_code, duration_ms, request_body, response_body, request_headers, response_headers,
        request_size, response_size, input_tokens, output_tokens, total_tokens, cost, retry_count
    FROM history WHERE id = ?`, id).Scan(&r.ID, &timestamp, &method, &path, &clientIP, &keyID, &provider, &model, &userModel,
    &statusCode, &durationMs, &requestBody, &responseBody, &reqHeaders, &respHeaders,
    &requestSize, &responseSize, &inputTokens, &outputTokens, &totalTokens, &cost, &r.RetryCount)
```

- [ ] **Step 9: `GetRecordsSummary` summary 构造加字段**

`history/history.go:495-513` 改：

```go
summaries = append(summaries, RecordSummary{
    ID:           r.ID,
    Timestamp:    r.Timestamp,
    Method:       r.Method,
    Path:         r.Path,
    ClientIP:     r.ClientIP,
    KeyID:        r.KeyID,
    Provider:     r.Provider,
    Model:        r.Model,
    UserModel:    r.UserModel,
    StatusCode:   r.StatusCode,
    Duration:     r.Duration,
    RequestSize:  r.RequestSize,
    ResponseSize: r.ResponseSize,
    InputTokens:  r.InputTokens,
    OutputTokens: r.OutputTokens,
    TotalTokens:  r.TotalTokens,
    Cost:         r.Cost,
    RetryCount:   r.RetryCount,
})
```

`history/history.go:527-531` 改：

```go
rows, err := db.Query(`
    SELECT id, timestamp, method, path, client_ip, key_id, provider, model, user_model,
        status_code, duration_ms, request_size, response_size,
        input_tokens, output_tokens, total_tokens, cost, retry_count
    FROM history ORDER BY timestamp DESC LIMIT ? OFFSET ?`, pageSize, offset)
```

`history/history.go:547-549` 改（同时改局部变量声明，加 `retryCount sql.NullInt64`）：

原始声明段（line 540-545）：

```go
var r RecordSummary
var timestamp int64
var method, path, clientIP, keyID, provider, model, userModel sql.NullString
var statusCode, durationMs, inputTokens, outputTokens, totalTokens sql.NullInt64
var requestSize, responseSize sql.NullInt64
var cost sql.NullFloat64
```

改为：

```go
var r RecordSummary
var timestamp int64
var method, path, clientIP, keyID, provider, model, userModel sql.NullString
var statusCode, durationMs, inputTokens, outputTokens, totalTokens, retryCount sql.NullInt64
var requestSize, responseSize sql.NullInt64
var cost sql.NullFloat64
```

Scan 调用（line 547-549）改为：

```go
err := rows.Scan(&r.ID, &timestamp, &method, &path, &clientIP, &keyID, &provider, &model, &userModel,
    &statusCode, &durationMs, &requestSize, &responseSize,
    &inputTokens, &outputTokens, &totalTokens, &cost, &retryCount)
```

字段赋值在 `r.Cost = cost.Float64`（line 570）之后追加：

```go
r.RetryCount = int(retryCount.Int64)
```

- [ ] **Step 10: 跑 Task 1 写的测试**

Run: `go test ./history/... -v`
Expected: PASS — `RetryCount` 字段、schema 列、迁移、SELECT/INSERT 全链路通

- [ ] **Step 11: commit**

```bash
git add history/history.go history/history_test.go
git commit -m "feat(history): persist retry_count column on RequestRecord"
```

---

## Task 3: `doRequestWithRetry` `finalAttempt` 语义单测

**Files:**
- Create: `proxy/proxy_retry_test.go`

- [ ] **Step 1: 写失败测试**

```go
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
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./proxy/... -run TestDoRequestWithRetry -v`
Expected: FAIL — `doRequestWithRetry` 当前签名只返回 `(resp, err)`，第三个返回值不存在

- [ ] **Step 3: commit**

```bash
git add proxy/proxy_retry_test.go
git commit -m "test(proxy): add doRequestWithRetry finalAttempt semantics"
```

注：测试会因为旧实现 sleep 等待而慢（最多约 6 秒）。这是为了不在生产代码里加测试 hook 引入复杂度。

---

## Task 4: `doRequestWithRetry` 改为命名返回值带回 `finalAttempt`

**Files:**
- Modify: `proxy/proxy.go:104-181`

- [ ] **Step 1: 改函数签名 + 加命名返回值 + 所有 return 处赋值**

`proxy/proxy.go:104-105` 改为：

```go
// doRequestWithRetry 执行请求并在遇到 "try again" 错误时自动重试
// finalAttempt 是最后一次执行的 attempt 编号（从 1 开始）。调用方用 finalAttempt-1 得到额外重试次数。
func doRequestWithRetry(req *http.Request, bodyBytes []byte, provider *config.Provider, maxRetries int) (resp *http.Response, finalAttempt int, err error) {
	var lastResp *http.Response
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		finalAttempt = attempt
```

- [ ] **Step 2: 改所有 `return` 语句带 finalAttempt**

把函数里所有 `return nil, err` 改为 `return nil, attempt, err`。

把 `return nil, err`（line 121, 134）改成 `return nil, attempt, err`。
把 `return resp, nil`（line 177）改成 `return resp, attempt, nil`。
把 `return lastResp, lastErr`（line 180）改成 `return lastResp, maxRetries, lastErr`。

完整函数体：

```go
func doRequestWithRetry(req *http.Request, bodyBytes []byte, provider *config.Provider, maxRetries int) (resp *http.Response, finalAttempt int, err error) {
	var lastResp *http.Response
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		finalAttempt = attempt
		// 为每次重试创建新的请求体 reader
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			if attempt < maxRetries {
				logger.Info("⚠️ 请求失败 (尝试 %d/%d): %v，准备重试...", attempt, maxRetries, err)
				time.Sleep(time.Duration(attempt) * time.Second) // 递增延迟
				continue
			}
			return nil, attempt, err
		}

		// 读取响应体以检查是否包含 "try again" 错误
		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			if attempt < maxRetries {
				logger.Info("⚠️ 读取响应失败 (尝试 %d/%d): %v，准备重试...", attempt, maxRetries, err)
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}
			return nil, attempt, err
		}

		// 检查响应是否包含需要重试的错误
		shouldRetry := false
		if resp.StatusCode >= 500 || resp.StatusCode == 429 || resp.StatusCode == 529 {
			var errorResp map[string]interface{}
			if json.Valid(respBody) && json.Unmarshal(respBody, &errorResp) == nil {
				if errorObj, ok := errorResp["error"].(map[string]interface{}); ok {
					// 检查 error type
					if errorType, ok := errorObj["type"].(string); ok {
						if errorType == "overloaded_error" || errorType == "rate_limit_error" {
							shouldRetry = true
						}
					}
					// 检查错误消息
					if message, ok := errorObj["message"].(string); ok {
						lowerMsg := strings.ToLower(message)
						if strings.Contains(lowerMsg, "try again") ||
							strings.Contains(lowerMsg, "high traffic") ||
							strings.Contains(lowerMsg, "overloaded") ||
							strings.Contains(lowerMsg, "负载较高") {
							shouldRetry = true
						}
					}
				}
			}
		}

		// 如果需要重试且还有重试次数
		if shouldRetry && attempt < maxRetries {
			logger.Info("⚠️ 检测到需重试错误 (尝试 %d/%d)：status=%d, type=overloaded_error，准备重试...", attempt, maxRetries, resp.StatusCode)
			time.Sleep(time.Duration(attempt) * time.Second) // 递增延迟
			continue
		}

		// 成功或不需要重试，重新包装响应体并返回
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
		if shouldRetry && attempt == maxRetries {
			logger.Info("❌ 重试 %d 次后仍然失败，终止重试", maxRetries)
		} else if attempt > 1 {
			logger.Info("✅ 重试成功 (尝试 %d/%d)", attempt, maxRetries)
		}
		return resp, attempt, nil
	}

	return lastResp, maxRetries, lastErr
}
```

- [ ] **Step 3: 跑测试**

Run: `go test ./proxy/... -run TestDoRequestWithRetry -v`
Expected: PASS（耗时长 ~6s，因为 backoff 是真实 sleep）

- [ ] **Step 4: commit**

```bash
git add proxy/proxy.go
git commit -m "feat(proxy): doRequestWithRetry returns finalAttempt via named return"
```

---

## Task 5: 调用链接入 `retryCount`

**Files:**
- Modify: `proxy/proxy.go:362` — 分发函数调用处
- Modify: `proxy/proxy.go:387` — 分发函数调用 handleStreamResponse
- Modify: `proxy/proxy.go:396` — handleStreamResponse 签名
- Modify: `proxy/proxy.go:623` — handleStreamResponse 内 RequestRecord 字面量
- Modify: `proxy/proxy.go:678` — handleNonStreamResponse 签名
- Modify: `proxy/proxy.go:752` — handleNonStreamResponse 内 RequestRecord 字面量

- [ ] **Step 1: 分发函数接 finalAttempt 并算 retryCount**

`proxy/proxy.go:362` 改为：

```go
// 发送请求，带重试机制
resp, finalAttempt, err := doRequestWithRetry(req, bodyBytes, provider, 3)
if err != nil {
    logger.Error("❌ Proxy error after retries: %v", err)
    c.JSON(http.StatusBadGateway, gin.H{"error": "Failed to proxy request after retries"})
    return
}
defer resp.Body.Close()

retryCount := finalAttempt - 1
```

- [ ] **Step 2: 分发函数传 retryCount 给 handleStreamResponse**

`proxy/proxy.go:387` 改为：

```go
if isStream {
    handleStreamResponse(c, resp, provider, requestID, startTime, c.Request.Method, c.Request.URL.Path, modifiedRequestBody, c.Request.Header, requestedModel, actualModel, userModel, keyID, clientIP, isIncomingOpenAIFormat, retryCount)
    return
}
```

- [ ] **Step 3: 找到分发函数调用 handleNonStreamResponse 的地方**

在 `proxy/proxy.go:388-389` 之后。让我先 grep 确认：
- 调用形式：`handleNonStreamResponse(c, resp, provider, requestID, startTime, c.Request.Method, c.Request.URL.Path, modifiedRequestBody, c.Request.Header, requestedModel, actualModel, userModel, keyID, clientIP, isIncomingOpenAIFormat)`

加 `, retryCount`：

```go
handleNonStreamResponse(c, resp, provider, requestID, startTime, c.Request.Method, c.Request.URL.Path, modifiedRequestBody, c.Request.Header, requestedModel, actualModel, userModel, keyID, clientIP, isIncomingOpenAIFormat, retryCount)
```

- [ ] **Step 4: handleStreamResponse 签名加 retryCount**

`proxy/proxy.go:396` 改为：

```go
func handleStreamResponse(c *gin.Context, resp *http.Response, provider *config.Provider, requestID string, startTime time.Time, method, path, requestBody string, requestHeaders http.Header, requestedModel, actualModel, userModel string, keyID, clientIP string, isIncomingOpenAIFormat bool, retryCount int) {
```

- [ ] **Step 5: handleStreamResponse 内 RequestRecord 字面量加 RetryCount**

`proxy/proxy.go:623-645` 的字面量加 `RetryCount: retryCount`：

```go
history.AddRecord(history.RequestRecord{
    ID:              requestID,
    Timestamp:       startTime,
    Method:          method,
    Path:            path,
    ClientIP:        clientIP,
    KeyID:           keyID,
    Provider:        provider.Name,
    Model:           model,
    UserModel:       userModel,
    StatusCode:      resp.StatusCode,
    Duration:        duration,
    RequestBody:     requestBody,
    ResponseBody:    responseBody.String(),
    RequestHeaders:  requestHeaders,
    ResponseHeaders: resp.Header,
    RequestSize:     int64(len(requestBody)),
    ResponseSize:    int64(responseBody.Len()),
    InputTokens:     inputTokens,
    OutputTokens:    outputTokens,
    TotalTokens:     inputTokens + outputTokens,
    Cost:            cost,
    RetryCount:      retryCount,
})
```

- [ ] **Step 6: handleNonStreamResponse 签名加 retryCount**

`proxy/proxy.go:678` 改为：

```go
func handleNonStreamResponse(c *gin.Context, resp *http.Response, provider *config.Provider, requestID string, startTime time.Time, method, path, requestBody string, requestHeaders http.Header, requestedModel, actualModel, userModel string, keyID, clientIP string, isIncomingOpenAIFormat bool, retryCount int) {
```

- [ ] **Step 7: handleNonStreamResponse 内 RequestRecord 字面量加 RetryCount**

`proxy/proxy.go:752-774` 字面量加 `RetryCount: retryCount`：

```go
history.AddRecord(history.RequestRecord{
    ID:              requestID,
    Timestamp:       startTime,
    Method:          method,
    Path:            path,
    ClientIP:        clientIP,
    KeyID:           keyID,
    Provider:        provider.Name,
    Model:           model,
    UserModel:       userModel,
    StatusCode:      resp.StatusCode,
    Duration:        duration,
    RequestBody:     requestBody,
    ResponseBody:    responseBodyForHistory,
    RequestHeaders:  requestHeaders,
    ResponseHeaders: resp.Header,
    RequestSize:     int64(len(requestBody)),
    ResponseSize:    int64(len(respBody)),
    InputTokens:     inputTokens,
    OutputTokens:    outputTokens,
    TotalTokens:     inputTokens + outputTokens,
    Cost:            cost,
    RetryCount:      retryCount,
})
```

- [ ] **Step 8: 编译 + 跑测试**

Run: `go build ./...`
Expected: SUCCESS

Run: `go test ./history/... ./proxy/... -v`
Expected: PASS

- [ ] **Step 9: commit**

```bash
git add proxy/proxy.go
git commit -m "feat(proxy): thread retryCount through handle*Response into history record"
```

---

## Task 6: `handleBroadcast` WS 推送消息加 `retry_count` 字段

**Files:**
- Modify: `history/history.go:253-284`

- [ ] **Step 1: WS 推送消息体加 retry_count**

`history/history.go:257-273` 的 `gin.H{}` 字典在 `"cost"` 之后加：

```go
"cost":         record.Cost,
"retry_count":  record.RetryCount,
```

完整 msg 段（line 257-273）：

```go
msg := gin.H{
    "id":           record.ID,
    "total":        total,
    "timestamp":    record.Timestamp,
    "method":       record.Method,
    "path":         record.Path,
    "client_ip":    record.ClientIP,
    "key_id":       record.KeyID,
    "provider":     record.Provider,
    "model":        record.Model,
    "status_code":  record.StatusCode,
    "duration_ms":  record.Duration,
    "input_tokens": record.InputTokens,
    "output_tokens": record.OutputTokens,
    "total_tokens": record.TotalTokens,
    "cost":         record.Cost,
    "retry_count":  record.RetryCount,
}
```

- [ ] **Step 2: 编译验证**

Run: `go build ./...`
Expected: SUCCESS

- [ ] **Step 3: commit**

```bash
git add history/history.go
git commit -m "feat(history): include retry_count in WS broadcast payload"
```

---

## Task 7: 前端表格新增「重试」列 + `.badge-retry` 样式

**Files:**
- Modify: `web/static/log.html:35-41` — CSS
- Modify: `web/static/log.html:128-139` — 表头
- Modify: `web/static/log.html:370` — 列表行渲染
- Modify: `web/static/log.html:380` — colspan
- Modify: `web/static/log.html:639` — 首页 colspan
- Modify: `web/static/log.html:722` — 首页行渲染

- [ ] **Step 1: 加 `.badge-retry` 样式**

`web/static/log.html:40` 之后追加：

```css
        .badge-retry { background: #ed8936; color: white; }
```

注：橙色 (#ed8936) 与现有 badge-stream 的绿、badge-non-stream 的蓝区分明显。

- [ ] **Step 2: 表头加 `<th>重试</th>`**

`web/static/log.html:134` 之后（在 `<th>类型</th>` 与 `<th>输入Token</th>` 之间）插入：

```html
                        <th>类型</th>
                        <th>重试</th>
                        <th>输入Token</th>
```

- [ ] **Step 3: 列表行渲染加 retry_count 列**

`web/static/log.html:370` 之后追加：

```html
                        <td><span class="badge ${record.path && record.path.includes('/v1/chat') ? 'badge-stream' : 'badge-non-stream'}">${record.path && record.path.includes('/v1/chat') ? 'OpenAI' : 'Anthropic'}</span></td>
                        <td>${record.retry_count > 0 ? `<span class="badge badge-retry">${record.retry_count}</span>` : '0'}</td>
                        <td>${formatToken(record.input_tokens || 0)}</td>
```

- [ ] **Step 4: 改两处 colspan 12 → 13**

`web/static/log.html:380` 改：

```html
                recordsTable.innerHTML = '<tr><td colspan="13" style="text-align: center; color: #999; padding: 20px;">暂无记录</td></tr>';
```

`web/static/log.html:639` 同样改 colspan=12 → colspan=13。

- [ ] **Step 5: 首页表格同步加列**

`web/static/log.html:717-728` 完整行块，结构同 Step 3 的列表版本——加同样的 `<td>` retry_count 列和表头列。表头位置在首页表格中确认是同一组 `<th>` 还是在另一个表格——若在首页表格里，同步插入。

实际首页表格的渲染段是 line 717-728，行结构与 line 365-376 相同。在 line 722 之后插入 retry_count 列：

```html
                        <td><span class="badge ${record.path && record.path.includes('/v1/chat') ? 'badge-stream' : 'badge-non-stream'}">${record.path && record.path.includes('/v1/chat') ? 'OpenAI' : 'Anthropic'}</span></td>
                        <td>${record.retry_count > 0 ? `<span class="badge badge-retry">${record.retry_count}</span>` : '0'}</td>
                        <td>${formatToken(record.input_tokens || 0)}</td>
```

- [ ] **Step 6: 浏览器加载验证**

启动应用，访问 `/static/log.html`，应看到：
- 表头新增「重试」列
- 列表里每行 retry_count > 0 的记录显示橙色 badge，= 0 显示普通 "0"
- 行为 13 列

Run: `grep -c '<th>' web/static/log.html | head -1` 应返回 13（或 26 如果有首页 + 列表两个表）

注：行不通的话，浏览器开发者工具查看是否布局错乱。

- [ ] **Step 7: commit**

```bash
git add web/static/log.html
git commit -m "feat(web): display retry count column with badge highlight"
```

---

## Task 8: 前端详情弹窗加「重试次数」字段

**Files:**
- Modify: `web/static/log.html:443-450` — 详情弹窗基本信息区

- [ ] **Step 1: 在「状态码」前/后插入「重试次数」**

`web/static/log.html:443-450` 区段（在「状态码」之前或「耗时」之后均可，选「耗时」之后更醒目）追加：

```html
                            <div class="detail-item">
                                <div class="detail-label">耗时</div>
                                <div class="detail-value">${formatDuration(record.duration_ms)}</div>
                            </div>
                            <div class="detail-item">
                                <div class="detail-label">重试次数</div>
                                <div class="detail-value">${record.retry_count > 0 ? `<span class="badge badge-retry">${record.retry_count}</span>` : '0'}</div>
                            </div>
```

- [ ] **Step 2: 浏览器加载验证**

启动应用，点击任意一条记录打开详情弹窗，应在「基本信息」区看到「重试次数」一行。重试过的记录显示橙色 badge，未重试的显示 "0"。

- [ ] **Step 3: commit**

```bash
git add web/static/log.html
git commit -m "feat(web): show retry count in request detail modal"
```

---

## Task 9: 端到端验证

**Files:** 无（验证步骤）

- [ ] **Step 1: 编译所有包**

Run: `go build ./...`
Expected: SUCCESS

- [ ] **Step 2: 跑所有测试**

Run: `go test ./... -short`
Expected: PASS

注：proxy/proxy_retry_test.go 会因为 backoff sleep 耗时 ~6s。如果觉得太长，可临时在测试里调 maxRetries=1 把 sleep 缩到 1s，但生产代码不动。

- [ ] **Step 3: 启动应用**

Run: `./switchai.exe`（或 `go run .`）
Expected: 正常启动，无 DB schema 错误（迁移生效）

- [ ] **Step 4: 触发一次成功请求**

通过 `/v1/messages` 或 `/v1/chat/completions` 发起一个正常请求。
- 在「最近请求记录」表格里，新行的「重试」列应显示 "0"
- 详情弹窗里「重试次数」应为 0

- [ ] **Step 5: 触发一次重试请求（人工或代码注入）**

方案 A：临时把目标 provider BaseURL 指向一个会返回 529 的 mock server，发请求：
- 应看到 4 次上游请求（1 + 3 retry）
- 表格里该记录的「重试」列显示橙色 badge 数字 "3"
- 详情弹窗里显示 "3"

方案 B（更简单）：通过 mock 上游 server 头两次返回 529、第三次返回 200：
- 表格里显示橙色 badge "2"
- 详情弹窗里显示 "2"

- [ ] **Step 6: 验证 DB schema 迁移对老数据无影响**

- Step 3 之前应已有老数据。启动后老记录的 retry_count 应默认为 0，不报错。
- 在表格里，老记录的「重试」列都显示 "0"

- [ ] **Step 7: commit（如有改动）**

```bash
git status
# 如果有遗漏的小改：
git add -u
git commit -m "chore: end-to-end verification tweaks"
```

---

## 备注

- **TDD 红绿循环**：每个 Task 第 1 步先写测试，第 2 步确认失败，然后才写实现。
- **回退点**：如果 Task 4 改 doRequestWithRetry 签名后下游编译失败，说明有其他调用点没收到。`grep -rn 'doRequestWithRetry' --include='*.go' .` 应只命中 `proxy/proxy.go:362` 一处。
- **time.Sleep 测试慢**：Task 3 测试因为真实 backoff（1+2+3=6s）耗时较长。如必须加快，可临时在测试文件顶部加 `-short` 跳过，但不修改生产代码。
- **多浏览器 WS 同步**：Task 6 加的 `retry_count` 字段，前端 WS 消费方（如有）会自动忽略未知字段；新前端（log.html）需自行消费该字段。