package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"switchai/appdata"
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

	// appdata doesn't read env vars — use chdir + Init pattern (see proxy_test.go)
	if err := appdata.Init(); err != nil {
		t.Fatalf("appdata.Init: %v", err)
	}

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
	var got map[string]interface{}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	v, ok := got["retry_count"]
	if !ok {
		t.Fatalf("retry_count field missing; json = %s", b)
	}
	if v.(float64) != 3 {
		t.Errorf("retry_count = %v, want 3", v)
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
		ID:         id,
		Timestamp:  time.Now(),
		Method:     "POST",
		Path:       "/v1/messages",
		StatusCode: 200,
		Duration:   100,
		RetryCount: 2,
	}
	AddRecord(r)

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