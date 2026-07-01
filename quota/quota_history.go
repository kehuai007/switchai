// Package quota — history persistence: writes per-10s snapshots of upstream
// quota snapshots to a dedicated SQLite DB and queries token consumption
// back from the stats DB (which owns provider_token_history). Retention
// is 7 days; auto-cleaned on Init.
package quota

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"switchai/appdata"

	_ "modernc.org/sqlite"
)

const (
	historyRetention = 7 * 24 * time.Hour
	historyBucketSec = 10
	aggregateBucket  = 5 * 60 // 5min
)

// Package-level state. historyDB holds the quota_history table; statsDB
// is the cross-package handle to stats.db::provider_token_history for
// reads (writes happen in stats.RecordUsage). statsDBInjected is set by
// tests to suppress initStatsDB's auto-open (so the test owns the only
// stats handle and the temp file can be cleaned up).
var (
	historyDB       *sql.DB
	historyOnce     sync.Once
	historyMu       sync.Mutex
	statsDBInjected bool

	statsDB   *sql.DB
	statsOnce sync.Once
	statsMu   sync.RWMutex
)

// QuotaPoint is one row in the chart for usage-percent.
type QuotaPoint struct {
	T           int64   `json:"t"`
	UsedPercent float64 `json:"used_percent"`
	UsageCount  int64   `json:"usage_count,omitempty"`
	TotalCount  int64   `json:"total_count,omitempty"`
}

// TokenPoint is one row in the chart for token consumption.
type TokenPoint struct {
	T            int64 `json:"t"`
	InputTokens  int   `json:"input_tokens"`
	OutputTokens int   `json:"output_tokens"`
	TotalTokens  int   `json:"total_tokens"`
	RequestCount int   `json:"request_count"`
}

// resolveDataDir returns the data directory in this order:
//  1. $SWITCHAI_DATA_DIR (used by tests and operators to override)
//  2. appdata.GetDataDir() (production default)
//
// We prefer the env var because the quota package opens a separate DB
// and we want tests (and any future ops tooling) to be able to redirect
// without touching appdata's package state.
func resolveDataDir() string {
	if v := os.Getenv("SWITCHAI_DATA_DIR"); v != "" {
		return v
	}
	return appdata.GetDataDir()
}

// InitHistory opens the quota DB, runs schema + cleanup. Idempotent.
// Safe to call multiple times; first call wins (sync.Once).
func InitHistory() error {
	var openErr error
	historyOnce.Do(func() {
		dataDir := resolveDataDir()
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			openErr = fmt.Errorf("mkdir: %w", err)
			return
		}
		dbPath := filepath.Join(dataDir, "quota_history.db")
		historyDB, openErr = sql.Open("sqlite", dbPath)
		if openErr != nil {
			return
		}
		schema := `
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
		if _, err := historyDB.Exec(schema); err != nil {
			openErr = fmt.Errorf("schema: %w", err)
			return
		}
		cleanupOldQuotaHistory()
	})
	return openErr
}

// ShutdownHistory closes the quota DB. Safe to call before Init.
func ShutdownHistory() {
	historyMu.Lock()
	defer historyMu.Unlock()
	if historyDB != nil {
		_ = historyDB.Close()
		historyDB = nil
	}
	historyOnce = sync.Once{}

	statsMu.Lock()
	if statsDB != nil && !statsDBInjected {
		_ = statsDB.Close()
	}
	statsDB = nil
	statsDBInjected = false
	statsMu.Unlock()
	statsOnce = sync.Once{}
}

// initStatsDB lazily opens a read-only handle to stats.db so
// QueryTokenHistory can read provider_token_history without making the
// quota package depend on stats. stats.RecordUsage owns the write path.
// stats.Init() also opens stats.db; whichever runs first wins (we use
// sync.Once). Tests inject a pre-built handle via setStatsDBForTest
// and set statsDBInjected=true so we don't try to reopen the same file.
func initStatsDB() error {
	if statsDBInjected {
		return nil
	}
	var openErr error
	statsOnce.Do(func() {
		dataDir := resolveDataDir()
		dbPath := filepath.Join(dataDir, "stats.db")
		// Use a read-only mode=ro DSN when the file exists; fall back
		// to read-write otherwise so tests that pre-create the DB work.
		if _, err := os.Stat(dbPath); err == nil {
			statsDB, openErr = sql.Open("sqlite", dbPath+"?mode=ro")
			if openErr != nil {
				return
			}
		} else {
			statsDB, openErr = sql.Open("sqlite", dbPath)
		}
	})
	return openErr
}

// RecordQuotaSnapshot writes one (provider, window) snapshot to the 10s bucket.
// Upserts so multiple polls in the same bucket collapse to the latest value.
func RecordQuotaSnapshot(providerID, window string, usedPercent float64, usageCount, totalCount int64) error {
	if historyDB == nil {
		if err := InitHistory(); err != nil {
			return err
		}
	}
	tb := time.Now().Unix() / historyBucketSec * historyBucketSec
	historyMu.Lock()
	defer historyMu.Unlock()
	_, err := historyDB.Exec(`
		INSERT INTO quota_history (provider_id, window, t_bucket, used_percent, usage_count, total_count)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider_id, window, t_bucket) DO UPDATE SET
			used_percent = excluded.used_percent,
			usage_count  = excluded.usage_count,
			total_count  = excluded.total_count`,
		providerID, window, tb, usedPercent, usageCount, totalCount)
	return err
}

// QueryQuotaHistory returns points in [fromTs, toTs]. If aggregate is true,
// collapses 10s buckets to 5min buckets (last value per bucket).
func QueryQuotaHistory(providerID, window string, fromTs, toTs int64, aggregate bool) ([]QuotaPoint, error) {
	if historyDB == nil {
		if err := InitHistory(); err != nil {
			return nil, err
		}
	}
	historyMu.Lock()
	defer historyMu.Unlock()
	if aggregate {
		// 5min buckets: GROUP BY (t_bucket/300)*300, take last (max t_bucket) point
		rows, err := historyDB.Query(`
			SELECT (t_bucket/?)*? AS bucket, used_percent, usage_count, total_count
			FROM quota_history
			WHERE provider_id=? AND window=? AND t_bucket BETWEEN ? AND ?
				AND t_bucket IN (
					SELECT MAX(t_bucket) FROM quota_history
					WHERE provider_id=? AND window=? AND t_bucket BETWEEN ? AND ?
					GROUP BY (t_bucket/?)*?
				)
			ORDER BY bucket ASC`,
			aggregateBucket, aggregateBucket,
			providerID, window, fromTs, toTs,
			providerID, window, fromTs, toTs,
			aggregateBucket, aggregateBucket)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []QuotaPoint
		for rows.Next() {
			var p QuotaPoint
			var uc, tc sql.NullInt64
			if err := rows.Scan(&p.T, &p.UsedPercent, &uc, &tc); err != nil {
				return nil, err
			}
			p.UsageCount = uc.Int64
			p.TotalCount = tc.Int64
			out = append(out, p)
		}
		return out, rows.Err()
	}
	rows, err := historyDB.Query(`
		SELECT t_bucket, used_percent, usage_count, total_count
		FROM quota_history
		WHERE provider_id=? AND window=? AND t_bucket BETWEEN ? AND ?
		ORDER BY t_bucket ASC`,
		providerID, window, fromTs, toTs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuotaPoint
	for rows.Next() {
		var p QuotaPoint
		var uc, tc sql.NullInt64
		if err := rows.Scan(&p.T, &p.UsedPercent, &uc, &tc); err != nil {
			return nil, err
		}
		p.UsageCount = uc.Int64
		p.TotalCount = tc.Int64
		out = append(out, p)
	}
	return out, rows.Err()
}

// QueryTokenHistory reads provider_token_history from stats.db (which is
// populated by stats.RecordUsage in the same transaction as the usage
// records). aggregate=true → 5min SUM. Filters out buckets where
// total_tokens=0. stats package owns the schema and writes; this is a
// read-only convenience wrapper for the quota-package API handlers.
func QueryTokenHistory(providerID string, fromTs, toTs int64, aggregate bool) ([]TokenPoint, error) {
	if err := initStatsDB(); err != nil {
		return nil, err
	}
	statsMu.RLock()
	db := statsDB
	statsMu.RUnlock()
	if db == nil {
		return nil, nil
	}
	var (
		rows *sql.Rows
		err  error
	)
	if aggregate {
		rows, err = db.Query(`
			SELECT (t_bucket/?)*? AS bucket,
			       SUM(input_tokens), SUM(output_tokens), SUM(total_tokens), SUM(request_count)
			FROM provider_token_history
			WHERE provider_id=? AND t_bucket BETWEEN ? AND ?
			GROUP BY bucket
			ORDER BY bucket ASC`,
			aggregateBucket, aggregateBucket,
			providerID, fromTs, toTs)
	} else {
		rows, err = db.Query(`
			SELECT t_bucket, input_tokens, output_tokens, total_tokens, request_count
			FROM provider_token_history
			WHERE provider_id=? AND t_bucket BETWEEN ? AND ? AND total_tokens > 0
			ORDER BY t_bucket ASC`,
			providerID, fromTs, toTs)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenPoint
	for rows.Next() {
		var p TokenPoint
		if err := rows.Scan(&p.T, &p.InputTokens, &p.OutputTokens, &p.TotalTokens, &p.RequestCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func cleanupOldQuotaHistory() {
	if historyDB == nil {
		return
	}
	cutoff := time.Now().Add(-historyRetention).Unix()
	historyMu.Lock()
	defer historyMu.Unlock()
	if _, err := historyDB.Exec(`DELETE FROM quota_history WHERE t_bucket < ?`, cutoff); err != nil {
		fmt.Printf("quota: cleanup quota_history: %v\n", err)
	}
}

// --- test helpers ---

// setHistoryDBForTest swaps the package-level historyDB. Tests use this
// to redirect to a temp DB. Returns the previous handle so cleanup can
// restore it.
func setHistoryDBForTest(db *sql.DB) *sql.DB {
	historyMu.Lock()
	defer historyMu.Unlock()
	prev := historyDB
	historyDB = db
	return prev
}

// resetHistoryOnceForTest lets tests re-trigger InitHistory.
func resetHistoryOnceForTest() {
	historyMu.Lock()
	defer historyMu.Unlock()
	historyOnce = sync.Once{}
}

// setStatsDBForTest swaps the package-level statsDB. Tests use this to
// inject a temp stats DB containing provider_token_history rows. While
// injected, initStatsDB is suppressed.
func setStatsDBForTest(db *sql.DB) *sql.DB {
	statsMu.Lock()
	defer statsMu.Unlock()
	prev := statsDB
	statsDB = db
	statsDBInjected = true
	// Reset the once so initStatsDB doesn't try to re-open.
	statsOnce = sync.Once{}
	return prev
}

// resetStatsDBInjectionForTest clears the test-injection flag so the
// next test (or the next production caller) gets fresh state.
func resetStatsDBInjectionForTest() {
	statsMu.Lock()
	defer statsMu.Unlock()
	statsDBInjected = false
	statsOnce = sync.Once{}
}

func recordQuotaSnapshotForTest(providerID, window string, tBucket int64, usedPercent float64, usageCount, totalCount int64) {
	if historyDB == nil {
		if err := InitHistory(); err != nil {
			panic(err)
		}
	}
	historyMu.Lock()
	defer historyMu.Unlock()
	if _, err := historyDB.Exec(`
		INSERT INTO quota_history (provider_id, window, t_bucket, used_percent, usage_count, total_count)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider_id, window, t_bucket) DO UPDATE SET
			used_percent = excluded.used_percent,
			usage_count  = excluded.usage_count,
			total_count  = excluded.total_count`,
		providerID, window, tBucket, usedPercent, usageCount, totalCount); err != nil {
		panic(err)
	}
}

// recordTokenBucketForTest inserts directly into provider_token_history
// using the stats-side handle (passed in by the test). It exists to keep
// the test free of any stats-package dependency.
func recordTokenBucketForTest(db *sql.DB, providerID string, tBucket int64, in, out, tot, cnt int) {
	if _, err := db.Exec(`
		INSERT INTO provider_token_history (provider_id, t_bucket, input_tokens, output_tokens, total_tokens, request_count)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider_id, t_bucket) DO UPDATE SET
			input_tokens  = input_tokens  + excluded.input_tokens,
			output_tokens = output_tokens + excluded.output_tokens,
			total_tokens  = total_tokens  + excluded.total_tokens,
			request_count = request_count + 1`,
		providerID, tBucket, in, out, tot, cnt); err != nil {
		panic(err)
	}
}

// SetStatsDBForTest injects a pre-built stats.db handle into the quota
// package so QueryTokenHistory reads from a caller-supplied database
// instead of opening stats.db from the data directory. While injected,
// initStatsDB is suppressed (so the caller controls the only open handle
// for the temp file). Returns a cleanup function that restores the
// previous state. Intended for cross-package tests (e.g. the stats
// package's bridge integration test) that need stats.RecordUsage and
// quota.QueryTokenHistory to operate against the same *sql.DB.
func SetStatsDBForTest(db *sql.DB) (cleanup func()) {
	// Note: we swap the package globals (statsDB, statsDBInjected,
	// statsOnce) and return a closure that restores them. The stats
	// package's Stats type embeds sync.RWMutex which is a noCopy type,
	// but statsOnce is sync.Once which is also noCopy — so we cannot
	// store it in a captured local variable. Instead we use
	// sync.Once's zero-value swap and remember the prior injected
	// flag (a plain bool) to restore on cleanup.
	statsMu.Lock()
	prevDB := statsDB
	prevInjected := statsDBInjected
	statsDB = db
	statsDBInjected = true
	// Reset the once under the lock so a concurrent caller does not
	// observe a partially-swapped state.
	statsOnce = sync.Once{}
	statsMu.Unlock()
	return func() {
		statsMu.Lock()
		statsDB = prevDB
		statsDBInjected = prevInjected
		statsOnce = sync.Once{}
		statsMu.Unlock()
	}
}

func countQuotaHistoryForTest(providerID string, tBucket int64) int {
	historyMu.Lock()
	defer historyMu.Unlock()
	var n int
	_ = historyDB.QueryRow(`SELECT COUNT(*) FROM quota_history WHERE provider_id=? AND t_bucket=?`,
		providerID, tBucket).Scan(&n)
	return n
}