package web

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"switchai/quota"
	"testing"

	"github.com/gin-gonic/gin"
	_ "modernc.org/sqlite"
)

func newTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// skip auth middleware for tests
	r.GET("/api/providers/:id/quota-history", getQuotaHistory)
	r.GET("/api/providers/:id/token-history", getTokenHistory)
	return r
}

func TestGetQuotaHistory_RejectsBadWindow(t *testing.T) {
	r := newTestRouter()
	req := httptest.NewRequest("GET", "/api/providers/p1/quota-history?window=bogus&range=5h", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestGetQuotaHistory_RejectsBadRange(t *testing.T) {
	r := newTestRouter()
	req := httptest.NewRequest("GET", "/api/providers/p1/quota-history?window=interval&range=99h", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestGetQuotaHistory_RejectsInjection(t *testing.T) {
	r := newTestRouter()
	req := httptest.NewRequest("GET", `/api/providers/p1/quota-history?window=interval%22%3B%20DROP&range=5h`, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestGetTokenHistory_RejectsBadRange(t *testing.T) {
	r := newTestRouter()
	req := httptest.NewRequest("GET", "/api/providers/p1/token-history?range=hax", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestGetQuotaHistory_EmptyReturnsEmptyPoints(t *testing.T) {
	// Isolate quota history DB into a temp dir so this test does not
	// touch the production DB. The web package cannot inject the DB
	// handle directly (that helper is unexported in package quota), so
	// we rely on the SWITCHAI_DATA_DIR env var that quota.InitHistory
	// honors on first call.
	tmpDir := t.TempDir()
	t.Setenv("SWITCHAI_DATA_DIR", tmpDir)
	// Ensure both expected DB files exist so initStatsDB opens the
	// read-only handle without falling back to read-write (matching
	// quota_history_test.go setupTestDB).
	if err := os.WriteFile(filepath.Join(tmpDir, "stats.db"), nil, 0o644); err != nil {
		t.Fatalf("seed stats.db: %v", err)
	}
	r := newTestRouter()
	req := httptest.NewRequest("GET", "/api/providers/nonexistent/quota-history?window=interval&range=5h", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d, body=%s", w.Code, w.Body.String())
	}
	// Close DBs so t.TempDir cleanup can remove the sqlite files on
	// Windows (file locks otherwise block RemoveAll).
	quota.ShutdownHistory()
	var resp struct {
		Window string                   `json:"window"`
		Range  string                   `json:"range"`
		Points []map[string]interface{} `json:"points"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Window != "interval" || resp.Range != "5h" {
		t.Errorf("bad echo: %+v", resp)
	}
	if resp.Points == nil {
		t.Errorf("points should be [], got null")
	}
}

// TestGetQuotaHistory_HappyPath exercises the 200 path with valid params
// and asserts the response shape (window/range echoed, points is [] not
// null, current is an object).
func TestGetQuotaHistory_HappyPath(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SWITCHAI_DATA_DIR", tmpDir)
	if err := os.WriteFile(filepath.Join(tmpDir, "stats.db"), nil, 0o644); err != nil {
		t.Fatalf("seed stats.db: %v", err)
	}
	r := newTestRouter()
	req := httptest.NewRequest("GET", "/api/providers/p-happy/quota-history?window=interval&range=5h", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d, body=%s", w.Code, w.Body.String())
	}
	quota.ShutdownHistory()
	var resp struct {
		Window  string                   `json:"window"`
		Range   string                   `json:"range"`
		Points  []map[string]interface{} `json:"points"`
		Current map[string]interface{}   `json:"current"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Window != "interval" || resp.Range != "5h" {
		t.Errorf("bad echo: window=%q range=%q", resp.Window, resp.Range)
	}
	if resp.Points == nil {
		t.Errorf("points should be [], got null")
	}
	if resp.Current == nil {
		t.Errorf("current should be an object, got null")
	}
}

// TestGetTokenHistory_HappyPath exercises the 200 path with valid params
// and asserts the response shape (range echoed, points is [] not null).
func TestGetTokenHistory_HappyPath(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SWITCHAI_DATA_DIR", tmpDir)
	// Pre-seed stats.db with the provider_token_history schema so the
	// read-only QueryTokenHistory query does not 500 on a missing table.
	statsPath := filepath.Join(tmpDir, "stats.db")
	sdb, err := sql.Open("sqlite", statsPath)
	if err != nil {
		t.Fatalf("open stats.db: %v", err)
	}
	if _, err := sdb.Exec(`CREATE TABLE IF NOT EXISTS provider_token_history (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		provider_id   TEXT    NOT NULL,
		t_bucket      INTEGER NOT NULL,
		input_tokens  INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens  INTEGER NOT NULL DEFAULT 0,
		request_count INTEGER NOT NULL DEFAULT 0,
		UNIQUE(provider_id, t_bucket)
	);`); err != nil {
		t.Fatalf("init stats schema: %v", err)
	}
	_ = sdb.Close()
	r := newTestRouter()
	req := httptest.NewRequest("GET", "/api/providers/p-happy/token-history?range=5h", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d, body=%s", w.Code, w.Body.String())
	}
	quota.ShutdownHistory()
	var resp struct {
		Range  string                   `json:"range"`
		Points []map[string]interface{} `json:"points"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Range != "5h" {
		t.Errorf("bad echo: range=%q", resp.Range)
	}
	if resp.Points == nil {
		t.Errorf("points should be [], got null")
	}
}

// TestGetTokenHistory_Accepts24h 守护：validRanges 白名单 + rangeToSeconds 必须
// 接受 "24h"（用量统计 modal 的默认档位），否则 400 拦截会让 modal 默认态空。
func TestGetTokenHistory_Accepts24h(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SWITCHAI_DATA_DIR", tmpDir)
	statsPath := filepath.Join(tmpDir, "stats.db")
	sdb, err := sql.Open("sqlite", statsPath)
	if err != nil {
		t.Fatalf("open stats.db: %v", err)
	}
	if _, err := sdb.Exec(`CREATE TABLE IF NOT EXISTS provider_token_history (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		provider_id   TEXT    NOT NULL,
		t_bucket      INTEGER NOT NULL,
		input_tokens  INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens  INTEGER NOT NULL DEFAULT 0,
		request_count INTEGER NOT NULL DEFAULT 0,
		UNIQUE(provider_id, t_bucket)
	);`); err != nil {
		t.Fatalf("init stats schema: %v", err)
	}
	_ = sdb.Close()
	r := newTestRouter()
	req := httptest.NewRequest("GET", "/api/providers/p-24h/token-history?range=24h", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 for range=24h, got %d, body=%s", w.Code, w.Body.String())
	}
	defer quota.ShutdownHistory()
	var resp struct {
		Range  string                   `json:"range"`
		Points []map[string]interface{} `json:"points"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Range != "24h" {
		t.Errorf("bad echo: range=%q", resp.Range)
	}
	if resp.Points == nil {
		t.Errorf("points should be [], got null")
	}
}
