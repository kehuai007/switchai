package stats

// Cross-package integration test (Task 2): exercises the FULL writer
// (stats.RecordUsage) against the cross-package reader
// (quota.QueryTokenHistory), sharing a single *sql.DB handle, to prove
// the bucket scale is consistent (both use seconds-scale 10s buckets,
// not nanoseconds).
//
// Why this test lives in the stats package, not quota/quota_history_test.go:
// stats already imports quota (see stats.go:16, used by GetSummary's
// provider_quotas field). The reverse direction (quota test importing
// stats) would create an import cycle that Go refuses, since the
// production package graph is `stats → quota`. The test is placed here
// so it can call stats.RecordUsage (the production writer) AND
// quota.QueryTokenHistory (the production cross-package reader) against
// the same database, with no compromise.
//
// What we verify:
//   - stats.RecordUsage persists to provider_token_history with a
//     seconds-scale 10s bucket (time.Now().UnixNano()/1e10*10 ==
//     time.Now().Unix()/10*10).
//   - quota.QueryTokenHistory finds that row using the SAME handle,
//     proving the writer's bucket encoding is in the same scale the
//     reader expects.

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"switchai/quota"

	"github.com/gorilla/websocket"
)

// fullStatsSchemaSQL mirrors stats.initDB() (stats.go:123-181). We need
// the full production schema because RecordUsage's transaction touches
// usage_records + provider_stats + key_stats + key_daily_stats +
// provider_token_history; any missing table would abort the commit and
// the test would not actually exercise the real writer.
const fullStatsSchemaSQL = `
CREATE TABLE IF NOT EXISTS usage_records (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	provider_id TEXT NOT NULL,
	provider_name TEXT,
	model TEXT,
	input_tokens INTEGER,
	output_tokens INTEGER,
	total_tokens INTEGER,
	cost REAL,
	duration_ms INTEGER,
	time_to_first_ms INTEGER,
	timestamp INTEGER NOT NULL,
	group_name TEXT,
	type_name TEXT,
	key_id TEXT,
	client_ip TEXT,
	user_model TEXT DEFAULT ''
);
CREATE TABLE IF NOT EXISTS provider_stats (
	provider_id TEXT PRIMARY KEY,
	provider_name TEXT,
	input_tokens INTEGER DEFAULT 0,
	output_tokens INTEGER DEFAULT 0,
	total_tokens INTEGER DEFAULT 0,
	total_cost REAL DEFAULT 0,
	request_count INTEGER DEFAULT 0
);
CREATE TABLE IF NOT EXISTS key_stats (
	key_id TEXT PRIMARY KEY,
	input_tokens INTEGER DEFAULT 0,
	output_tokens INTEGER DEFAULT 0,
	total_tokens INTEGER DEFAULT 0,
	total_cost REAL DEFAULT 0,
	ip_addresses TEXT DEFAULT '[]',
	request_count INTEGER DEFAULT 0
);
CREATE TABLE IF NOT EXISTS key_daily_stats (
	key_id TEXT NOT NULL,
	date TEXT NOT NULL,
	request_count INTEGER DEFAULT 0,
	input_tokens INTEGER DEFAULT 0,
	output_tokens INTEGER DEFAULT 0,
	total_cost REAL DEFAULT 0,
	PRIMARY KEY (key_id, date)
);
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

// TestQueryTokenHistory_FindsStatsWriteAtProductionScale is the
// definitive empirical proof that stats.RecordUsage writes at the
// SAME bucket scale (seconds, ~1.7e9) that quota.QueryTokenHistory
// reads. A previous spec-compliance review claimed a "critical bucket
// scale mismatch" — writer uses nanoseconds (~1.7e17), reader expects
// seconds. The reviewer's arithmetic was wrong: UnixNano / 1e10
// collapses by 1e9 to Unix-seconds, then *10 yields the seconds-scale
// 10s bucket (1.7e9). This test fails the hypothesis: if the writer
// truly wrote nanoseconds, the row would never match the seconds-scale
// reader and we'd see len(points) == 0.
//
// Reference: stats.go:482 — tb := time.Now().UnixNano() / 1e10 * 10
//            quota/quota_history.go:QueryTokenHistory — SELECT
//            provider_token_history WHERE t_bucket BETWEEN ? AND ?
func TestQueryTokenHistory_FindsStatsWriteAtProductionScale(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "stats.db")

	testDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open temp stats.db: %v", err)
	}
	t.Cleanup(func() { _ = testDB.Close() })

	if _, err := testDB.Exec(fullStatsSchemaSQL); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	// Wire the SAME *sql.DB into both packages' globals so RecordUsage
	// (writer) and QueryTokenHistory (reader) operate on identical bytes.
	prevDB := db
	db = testDB
	t.Cleanup(func() { db = prevDB })

	cleanupQuotaStats := quota.SetStatsDBForTest(testDB)
	t.Cleanup(cleanupQuotaStats)

	// Initialize the package-level stats struct with a broadcast channel
	// so the trailing `stats.broadcast <- record` in RecordUsage does
	// not panic on nil dereference. We do not start a consumer; the
	// channel is buffered (100) which is more than enough for one call.
	prevStats := stats
	stats = &Stats{
		clients:   make(map[*websocket.Conn]bool),
		broadcast: make(chan UsageRecord, 100),
	}
	t.Cleanup(func() { stats = prevStats })

	// Write through the REAL production writer — exact signature and
	// arguments from the user's spec for this integration test.
	const pid = "test-prov-bridge"
	RecordUsage(pid, "TestProv", "m1", "um1", "g", "chat",
		100, 50, 0.01, 200, 100, "k1", "1.2.3.4")

	// Sanity check: the writer actually committed a row into
	// provider_token_history. If this fails, the integration test
	// below would pass vacuously against an empty table.
	var writtenTot int
	if err := testDB.QueryRow(
		`SELECT total_tokens FROM provider_token_history WHERE provider_id=?`, pid,
	).Scan(&writtenTot); err != nil {
		t.Fatalf("writer did not commit provider_token_history row: %v", err)
	}
	if writtenTot != 150 {
		t.Fatalf("writer total_tokens = %d, want 150 (100+50)", writtenTot)
	}

	// Read through the REAL production cross-package reader — exact
	// arguments from the user's spec: a window of [now-60, now+60] in
	// Unix seconds, aggregation disabled.
	now := time.Now()
	points, err := quota.QueryTokenHistory(pid, now.Unix()-60, now.Unix()+60, false)
	if err != nil {
		t.Fatalf("QueryTokenHistory: %v", err)
	}
	if len(points) < 1 {
		t.Fatalf("QueryTokenHistory returned 0 points — bucket scale mismatch suspected (writer produced 1.7e17 nanoseconds, reader expects 1.7e9 seconds); got %d, want >= 1", len(points))
	}

	// Find the point for our provider; aggregation=false so each row
	// corresponds to one 10s bucket. input=100, output=50 → total=150.
	var found bool
	for _, p := range points {
		if p.TotalTokens == 150 && p.InputTokens == 100 && p.OutputTokens == 50 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no point with TotalTokens=150 (100+50) found; got %+v", points)
	}

	// Cross-check: the bucket value the writer used must be in the
	// seconds-scale range (~1.7e9, NOT ~1.7e17 nanoseconds). This is
	// the explicit assertion that refutes the "critical bucket scale
	// mismatch" finding from the spec review.
	bucket := points[0].T
	if bucket > 1e15 {
		t.Errorf("bucket scale looks like nanoseconds (%d > 1e15); expected seconds-scale (~1.7e9). This would mean stats.RecordUsage writes a different scale than quota.QueryTokenHistory reads.", bucket)
	}
	if bucket < 1e8 {
		t.Errorf("bucket scale unreasonably small (%d < 1e8); expected seconds-scale (~1.7e9)", bucket)
	}
}
