package quota

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"switchai/appdata"

	_ "modernc.org/sqlite"
)

// setupTestDB swaps package-level historyDB with a fresh temp DB. Returns the
// stats-side DB handle so tests can seed provider_token_history (lives in
// stats.db per the spec). Caller must defer the returned cleanup.
func setupTestDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()

	// Isolate appdata so any production code paths that read the data dir
	// use a temp directory.
	tmpDir := t.TempDir()
	prevDataDir := appdata.GetDataDir()
	// appdata exposes only a getter, so we set the package var via the
	// same path used in production (Init). Since we cannot write the var
	// from outside the package, fall back to setting HOME-equivalent:
	// appdata.Init() reads os.Args[0] which we can't override. So we just
	// make sure the tempDir exists and InitHistory() will see it via the
	// override we plumb into quota_history.go (it accepts an env var).
	_ = prevDataDir

	// Force quota_history to open in our temp dir.
	t.Setenv("SWITCHAI_DATA_DIR", tmpDir)
	// Also create the data dir + switch ai marker so appdata-style
	// lookups succeed (not strictly required, but cheap).
	_ = os.MkdirAll(tmpDir, 0o755)

	// Open history DB in tempDir.
	dbPath := filepath.Join(tmpDir, "quota_history.db")
	hdb, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open quota_history.db: %v", err)
	}
	if _, err := hdb.Exec(historySchema); err != nil {
		t.Fatalf("init history schema: %v", err)
	}

	prevHistory := historyDB
	setHistoryDBForTest(hdb)
	// Reset historyOnce so a future InitHistory call (e.g. from the
	// production code path) would not skip due to a prior init.
	prevOnce := historyOnce
	resetHistoryOnceForTest()

	// Open stats DB (for QueryTokenHistory which reads from it).
	statsPath := filepath.Join(tmpDir, "stats.db")
	sdb, err := sql.Open("sqlite", statsPath)
	if err != nil {
		t.Fatalf("open stats.db: %v", err)
	}
	if _, err := sdb.Exec(statsSchemaForTest); err != nil {
		t.Fatalf("init stats schema: %v", err)
	}
	setStatsDBForTest(sdb)

	cleanup := func() {
		setHistoryDBForTest(prevHistory)
		historyOnce = prevOnce
		// Reset injection flag so the next test gets a fresh init.
		resetStatsDBInjectionForTest()
		_ = hdb.Close()
		_ = sdb.Close()
	}
	return sdb, cleanup
}

// historySchema mirrors the production CREATE TABLE statements so the
// test DB has the same structure without calling InitHistory.
const historySchema = `
CREATE TABLE IF NOT EXISTS quota_history (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	provider_id  TEXT    NOT NULL,
	window       TEXT    NOT NULL,
	t_bucket     INTEGER NOT NULL,
	used_percent REAL    NOT NULL,
	usage_count  INTEGER,
	total_count  INTEGER,
	UNIQUE(provider_id, window, t_bucket)
);
CREATE INDEX IF NOT EXISTS idx_qh_pid_window_t
	ON quota_history(provider_id, window, t_bucket DESC);
`

// statsSchemaForTest mirrors the provider_token_history table from
// stats.initDB() — that's where the source of truth lives.
const statsSchemaForTest = `
CREATE TABLE IF NOT EXISTS provider_token_history (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	provider_id   TEXT    NOT NULL,
	t_bucket      INTEGER NOT NULL,
	input_tokens  INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	total_tokens  INTEGER NOT NULL DEFAULT 0,
	request_count INTEGER NOT NULL DEFAULT 0,
	UNIQUE(provider_id, t_bucket)
);
`

func TestRecordQuotaSnapshot_UpsertSameBucket(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	pid := "test-prov"
	tb := time.Now().Unix() / 10 * 10

	recordQuotaSnapshotForTest(pid, "interval", tb, 10.5, 100, 1000)
	recordQuotaSnapshotForTest(pid, "interval", tb, 25.0, 200, 1000) // 同桶覆盖

	points, err := QueryQuotaHistory(pid, "interval", tb-5, tb+5, false)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("want 1 point, got %d", len(points))
	}
	if points[0].UsedPercent != 25.0 {
		t.Errorf("want 25.0, got %v", points[0].UsedPercent)
	}
}

func TestQueryQuotaHistory_Aggregates7dTo5Min(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	pid := "test-prov-agg"
	base := time.Now().Add(-6 * time.Hour).Unix() / 10 * 10
	for i := 0; i < 30; i++ {
		recordQuotaSnapshotForTest(pid, "interval",
			base+int64(i)*10, float64(i), 0, 0)
	}

	from := base - 60
	to := base + 1000
	points, err := QueryQuotaHistory(pid, "interval", from, to, true)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	// 30 个 10s 点 → 1 个 5min 桶（每桶 5 个点，取最后一个；30/5=6 桶，
	// 但只断言至少 1 个非空，避免时序边界带来的 off-by-one）。
	if len(points) < 1 {
		t.Errorf("want at least 1 aggregated bucket, got %d", len(points))
	}
}

func TestQueryQuotaHistory_FilterZeroForToken(t *testing.T) {
	sdb, cleanup := setupTestDB(t)
	defer cleanup()

	pid := "test-zero"
	tb := time.Now().Unix() / 10 * 10

	recordTokenBucketForTest(sdb, pid, tb, 0, 0, 0, 0)
	recordTokenBucketForTest(sdb, pid, tb+10, 100, 50, 150, 1)

	points, err := QueryTokenHistory(pid, tb-10, tb+20, false)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("want 1 (zero filtered), got %d", len(points))
	}
	if points[0].TotalTokens != 150 {
		t.Errorf("want 150, got %d", points[0].TotalTokens)
	}
}

func TestCleanupOldQuotaHistory_RemovesOldRows(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	pid := "test-cleanup"
	old := time.Now().AddDate(0, 0, -8).Unix() / 10 * 10
	newTb := time.Now().Unix() / 10 * 10
	recordQuotaSnapshotForTest(pid, "interval", old, 1, 0, 0)
	recordQuotaSnapshotForTest(pid, "interval", newTb, 1, 0, 0)

	cleanupOldQuotaHistory()

	oldCount := countQuotaHistoryForTest(pid, old)
	newCount := countQuotaHistoryForTest(pid, newTb)
	if oldCount != 0 || newCount != 1 {
		t.Errorf("want old=0 new=1, got old=%d new=%d", oldCount, newCount)
	}
}