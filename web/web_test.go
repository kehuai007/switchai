package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"switchai/quota"
	"testing"

	"github.com/gin-gonic/gin"
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