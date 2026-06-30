# Quota History Popup Modal Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a click-through Modal popup on the "API Provider" quota bars that shows a dual-axis ECharts line chart with usage percentage and input/output/total token trends, backed by two new SQLite history tables.

**Architecture:**
- Backend: Two new SQLite tables (`quota_history`, `provider_token_history`) updated on the existing 10s poll / per-request transaction paths, queried via two new REST endpoints.
- Frontend: New `#quotaChartModal` reusing the existing `.modal` styles and the `echarts` instance pattern from `key-chart-panel`.

**Tech Stack:** Go (modernc.org/sqlite, gin), Vanilla JS + ECharts (already vendored at `web/static/vendor/echarts.min.js`).

**Spec:** `docs/superpowers/specs/2026-06-30-quota-history-popup-design.md`

---

## File Structure

| File | Responsibility |
|------|----------------|
| `quota/quota_history.go` *(new)* | DB schema init + `RecordQuotaSnapshot` + `QueryQuotaHistory` + `cleanupOldQuota` |
| `quota/quota_history_test.go` *(new)* | Unit tests for schema, upsert, cleanup, query |
| `quota/quota.go` *(modify)* | Wire `RecordQuotaSnapshot` into `pollOnce()`; call cleanup on Init |
| `stats/stats.go` *(modify)* | Add `provider_token_history` table to `initDB`; upsert in `RecordUsage`; cleanup in `Init` |
| `stats/stats_test.go` *(modify)* | Test token history upsert + cleanup |
| `web/web.go` *(modify)* | Register two new GET routes; implement `getQuotaHistory` and `getTokenHistory` |
| `web/web_test.go` *(new)* | API tests: whitelist, injection, empty data, 7d aggregation |
| `web/static/index.html` *(modify)* | Modal DOM, CSS extensions, click handler, ECharts init, range switcher |

Each file does ONE thing. Backend and frontend can be developed in parallel after Task 3 (shared types / interfaces are implicit; the API contract is the boundary).

---

## Task 1: Provider token history table + upsert in stats

**Files:**
- Modify: `stats/stats.go:117-173` (initDB schema block)
- Modify: `stats/stats.go:343-465` (RecordUsage transaction)
- Modify: `stats/stats.go:86-115` (Init — call cleanup)
- Test: `stats/stats_test.go`

- [ ] **Step 1: Write the failing test**

Append to `stats/stats_test.go` (create file if missing — look at existing patterns first):

```go
package stats

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// helper: temp data dir
func setupTestDB(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	oldDataDir := os.Getenv("SWITCHAI_DATA_DIR")
	os.Setenv("SWITCHAI_DATA_DIR", tmp)
	t.Cleanup(func() { os.Setenv("SWITCHAI_DATA_DIR", oldDataDir) })
}

func TestRecordUsage_AccumulatesProviderTokenHistory(t *testing.T) {
	setupTestDB(t)
	Init()

	pid := "test-provider-token"
	RecordUsage(pid, "TestProv", "m1", "um1", "g", "chat",
		100, 50, 0.01, 200, 100, "k1", "1.2.3.4")
	RecordUsage(pid, "TestProv", "m1", "um1", "g", "chat",
		200, 80, 0.02, 200, 100, "k1", "1.2.3.4")

	tb := (time.Now().UnixNano() / 1e10) * 10

	var in, out, tot, cnt int
	row := db.QueryRow(`SELECT input_tokens, output_tokens, total_tokens, request_count
		FROM provider_token_history WHERE provider_id=? AND t_bucket=?`, pid, tb)
	if err := row.Scan(&in, &out, &tot, &cnt); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if in != 300 || out != 130 || tot != 430 || cnt != 2 {
		t.Errorf("want 300/130/430/2 got %d/%d/%d/%d", in, out, tot, cnt)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./stats/ -run TestRecordUsage_AccumulatesProviderTokenHistory -v`
Expected: FAIL with "no such table: provider_token_history"

- [ ] **Step 3: Add table to initDB schema**

In `stats/stats.go` `initDB()` schema string, append after the existing CREATE INDEX statements:

```sql
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
CREATE INDEX IF NOT EXISTS idx_pth_pid_t
    ON provider_token_history(provider_id, t_bucket DESC);
```

- [ ] **Step 4: Add cleanup call in Init**

In `stats/stats.go` `Init()`, right before `stats = &Stats{...}`:

```go
// 清理 7 天前的 token 历史
sevenDaysAgo := time.Now().AddDate(0, 0, -7).UnixNano() / 1e10 * 10
if _, err := db.Exec(`DELETE FROM provider_token_history WHERE t_bucket < ?`, sevenDaysAgo); err != nil {
    logger.Error("Failed to cleanup old provider_token_history: %v", err)
}
```

- [ ] **Step 5: Add upsert in RecordUsage transaction**

In `stats/stats.go` `RecordUsage()`, just before `tx.Commit()`:

```go
// 累加 provider_token_history（10s 桶）
tb := time.Now().UnixNano() / 1e10 * 10
_, err = tx.Exec(`
    INSERT INTO provider_token_history (provider_id, t_bucket, input_tokens, output_tokens, total_tokens, request_count)
    VALUES (?, ?, ?, ?, ?, 1)
    ON CONFLICT(provider_id, t_bucket) DO UPDATE SET
        input_tokens  = input_tokens  + excluded.input_tokens,
        output_tokens = output_tokens + excluded.output_tokens,
        total_tokens  = total_tokens  + excluded.total_tokens,
        request_count = request_count + 1`,
    providerID, tb, inputTokens, outputTokens, inputTokens+outputTokens)
if err != nil {
    logger.Error("Failed to upsert provider_token_history: %v", err)
    return
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./stats/ -run TestRecordUsage_AccumulatesProviderTokenHistory -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add stats/stats.go stats/stats_test.go
git commit -m "feat(stats): persist provider_token_history (10s bucket, 7d retention)"
```

---

## Task 2: Quota history table + recording in quota package

**Files:**
- Create: `quota/quota_history.go`
- Create: `quota/quota_history_test.go`
- Modify: `quota/quota.go` (wire init + cleanup + per-poll upsert)

- [ ] **Step 1: Write the failing test**

Create `quota/quota_history_test.go`:

```go
package quota

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"switchai/config"
)

func setupTestDB(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	os.Setenv("SWITCHAI_DATA_DIR", tmp)
	if cfg := config.GetConfig(); cfg == nil {
		// config not initialized in tests; quota_history uses its own DB
	}
}

func TestRecordQuotaSnapshot_UpsertSameBucket(t *testing.T) {
	setupTestDB(t)

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
	setupTestDB(t)

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
	// 30 个 10s 点 → 6 个 5min 桶（每桶 5 个点，取最后一个）
	if len(points) != 1 {
		t.Errorf("want 1 aggregated bucket, got %d", len(points))
	}
}

func TestQueryQuotaHistory_FilterZeroForToken(t *testing.T) {
	setupTestDB(t)

	pid := "test-zero"
	tb := time.Now().Unix() / 10 * 10

	recordTokenBucketForTest(pid, tb, 0, 0, 0, 0)
	recordTokenBucketForTest(pid, tb+10, 100, 50, 150, 1)

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
	setupTestDB(t)

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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./quota/ -run 'TestRecordQuotaSnapshot|TestQueryQuotaHistory|TestCleanupOldQuotaHistory' -v`
Expected: FAIL with "undefined: recordQuotaSnapshotForTest"

- [ ] **Step 3: Create quota_history.go (DB + helpers + recording)**

Create `quota/quota_history.go`:

```go
// Package quota — history persistence: writes per-10s snapshots of upstream
// quota snapshots and provider token consumption to SQLite, queries them back
// for the popup chart modal. Retention is 7 days; auto-cleaned on Init.
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

var (
	historyDB   *sql.DB
	historyOnce sync.Once
	historyMu   sync.Mutex
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
	T             int64 `json:"t"`
	InputTokens   int   `json:"input_tokens"`
	OutputTokens  int   `json:"output_tokens"`
	TotalTokens   int   `json:"total_tokens"`
	RequestCount  int   `json:"request_count"`
}

// InitHistory opens the quota DB and runs schema + cleanup. Idempotent.
func InitHistory() error {
	var openErr error
	historyOnce.Do(func() {
		dataDir := appdata.GetDataDir()
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
		CREATE INDEX IF NOT EXISTS idx_pth_pid_t
			ON provider_token_history(provider_id, t_bucket DESC);
		`
		if _, err := historyDB.Exec(schema); err != nil {
			openErr = fmt.Errorf("schema: %w", err)
			return
		}
		cleanupOldQuotaHistory()
	})
	return openErr
}

// ShutdownHistory closes the DB. Safe to call before Init.
func ShutdownHistory() {
	historyMu.Lock()
	defer historyMu.Unlock()
	if historyDB != nil {
		_ = historyDB.Close()
		historyDB = nil
	}
	historyOnce = sync.Once{}
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

// RecordTokenBucket upserts one 10s token bucket. Sum semantics (caller passes
// the per-event counts; this adds them).
func RecordTokenBucket(providerID string, inputTokens, outputTokens, requestCount int) error {
	if historyDB == nil {
		if err := InitHistory(); err != nil {
			return err
		}
	}
	tb := time.Now().Unix() / historyBucketSec * historyBucketSec
	historyMu.Lock()
	defer historyMu.Unlock()
	_, err := historyDB.Exec(`
		INSERT INTO provider_token_history (provider_id, t_bucket, input_tokens, output_tokens, total_tokens, request_count)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider_id, t_bucket) DO UPDATE SET
			input_tokens  = input_tokens  + excluded.input_tokens,
			output_tokens = output_tokens + excluded.output_tokens,
			total_tokens  = total_tokens  + excluded.total_tokens,
			request_count = request_count + 1`,
		providerID, tb, inputTokens, outputTokens, inputTokens+outputTokens, requestCount)
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

// QueryTokenHistory returns points in [fromTs, toTs]. aggregate=true → 5min SUM.
// Filters out buckets where total_tokens=0.
func QueryTokenHistory(providerID string, fromTs, toTs int64, aggregate bool) ([]TokenPoint, error) {
	if historyDB == nil {
		if err := InitHistory(); err != nil {
			return nil, err
		}
	}
	historyMu.Lock()
	defer historyMu.Unlock()
	var (
		rows *sql.Rows
		err  error
	)
	if aggregate {
		rows, err = historyDB.Query(`
			SELECT (t_bucket/?)*? AS bucket,
			       SUM(input_tokens), SUM(output_tokens), SUM(total_tokens), SUM(request_count)
			FROM provider_token_history
			WHERE provider_id=? AND t_bucket BETWEEN ? AND ?
			GROUP BY bucket
			ORDER BY bucket ASC`,
			aggregateBucket, aggregateBucket,
			providerID, fromTs, toTs)
	} else {
		rows, err = historyDB.Query(`
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
	if _, err := historyDB.Exec(`DELETE FROM provider_token_history WHERE t_bucket < ?`, cutoff); err != nil {
		fmt.Printf("quota: cleanup provider_token_history: %v\n", err)
	}
}

// --- test helpers ---

func recordQuotaSnapshotForTest(providerID, window string, tBucket int64, usedPercent float64, usageCount, totalCount int64) {
	if err := InitHistory(); err != nil {
		panic(err)
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

func recordTokenBucketForTest(providerID string, tBucket int64, in, out, tot, cnt int) {
	if err := InitHistory(); err != nil {
		panic(err)
	}
	historyMu.Lock()
	defer historyMu.Unlock()
	if _, err := historyDB.Exec(`
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

func countQuotaHistoryForTest(providerID string, tBucket int64) int {
	historyMu.Lock()
	defer historyMu.Unlock()
	var n int
	_ = historyDB.QueryRow(`SELECT COUNT(*) FROM quota_history WHERE provider_id=? AND t_bucket=?`,
		providerID, tBucket).Scan(&n)
	return n
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./quota/ -run 'TestRecordQuotaSnapshot|TestQueryQuotaHistory|TestCleanupOldQuotaHistory' -v`
Expected: PASS (4 tests)

- [ ] **Step 5: Wire into quota package**

In `quota/quota.go`:

a) Modify `Init()` to call `InitHistory()`:
```go
func Init(ctx context.Context) error {
	if err := InitHistory(); err != nil {
		return err
	}
	loadBlockFlagsFromConfig()
	stateMu.Lock()
	started = true
	stateMu.Unlock()
	go runLoop(ctx)
	return nil
}
```

b) Modify `pollOnce()` — after `for _, p := range providers` loop succeeds for each provider, write snapshots. Add this AFTER the goroutine block:

```go
// 持久化到 quota_history (window: interval, weekly)
writeQuotaSnapshots(p.id, snap)
```

Wait — `snap` is not in scope. Re-do: replace the `pollOnce` body so each goroutine writes its snapshot. Update `pollOnce`:

```go
func pollOnce() {
	providers := eligibleProviders()
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, p := range providers {
		wg.Add(1)
		sem <- struct{}{}
		go func(id, key string) {
			defer wg.Done()
			defer func() { <-sem }()
			snap, err := pollProviderFull(id, key, upstreamHost)
			if err != nil {
				markError(id, err.Error())
				return
			}
			persistQuotaSnapshot(id, snap)
		}(p.id, p.key)
	}
	wg.Wait()
}

// persistQuotaSnapshot writes interval + weekly (if enabled) into quota_history.
func persistQuotaSnapshot(id string, snap *Snapshot) {
	if snap == nil {
		return
	}
	if snap.Interval.Enabled {
		_ = RecordQuotaSnapshot(id, "interval",
			snap.Interval.UsedPercent, snap.Interval.UsageCount, snap.Interval.TotalCount)
	}
	if snap.Weekly.Enabled {
		_ = RecordQuotaSnapshot(id, "weekly",
			snap.Weekly.UsedPercent, snap.Weekly.UsageCount, snap.Weekly.TotalCount)
	}
}
```

But `pollProvider` currently returns only `error`, not the snapshot. Look at `quota/upstream.go` (mentioned by spec) — `pollProvider` likely constructs and stores the snapshot already via `setSnapshot`. Add a new helper `pollProviderFull` that returns the snapshot without changing `pollProvider`'s public signature:

```go
func pollProviderFull(id, key, host string) (*Snapshot, error) {
	// body of pollProvider(id, key, host), but return (*Snapshot, error) instead of just error
	// and skip the in-memory write — caller stores it via setSnapshot + persistQuotaSnapshot
}
```

Implementation detail: extract the existing `pollProvider` body from `quota/upstream.go` (read first), copy into a new helper `pollProviderFull` that returns `(*Snapshot, error)` and does NOT call `setSnapshot`. Then refactor the existing `pollProvider` to wrap it:

```go
func pollProvider(id, key, host string) error {
	snap, err := pollProviderFull(id, key, host)
	if err != nil {
		return err
	}
	setSnapshot(id, snap)
	return nil
}
```

Note: `setSnapshot` already exists (used by `SetSnapshotForTest`). Locate it (grep `setSnapshot` in quota package) — if internal, expose via package-internal call.

c) Wire `RecordTokenBucket` into the proxy layer. Find where `stats.RecordUsage` is called (likely in `proxy/proxy.go`). Add one line right after each call:

```go
quota.RecordTokenBucket(providerID, inputTokens, outputTokens, 1)
```

Read `proxy/proxy.go` first to find the exact call sites — there may be one or two (success path and possibly stream chunks). For per-request billing, one call per completed request is correct; if stream has intermediate token counts, use the FINAL count only (don't double-count).

- [ ] **Step 6: Run all quota tests**

Run: `go test ./quota/ -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add quota/quota_history.go quota/quota_history_test.go quota/quota.go proxy/proxy.go
git commit -m "feat(quota): persist quota_history snapshots + token buckets, 7d retention"
```

---

## Task 3: API endpoints — quota-history + token-history

**Files:**
- Modify: `web/web.go` (register routes + handlers)

- [ ] **Step 1: Write the failing test**

Create `web/web_test.go`:

```go
package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	r := newTestRouter()
	req := httptest.NewRequest("GET", "/api/providers/nonexistent/quota-history?window=interval&range=5h", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./web/ -run 'TestGetQuotaHistory|TestGetTokenHistory' -v`
Expected: FAIL with "undefined: getQuotaHistory"

- [ ] **Step 3: Implement handlers + whitelist**

Add to `web/web.go` (after `setQuotaBlockEnabled` handler, before `getStats`):

```go
var validWindows = map[string]bool{"interval": true, "weekly": true}
var validRanges  = map[string]bool{"5h": true, "1h": true, "7d": true}

func rangeToSeconds(r string) int64 {
	switch r {
	case "5h":
		return 5 * 3600
	case "1h":
		return 3600
	case "7d":
		return 7 * 24 * 3600
	}
	return 0
}

func getQuotaHistory(c *gin.Context) {
	pid := c.Param("id")
	window := c.Query("window")
	rng := c.Query("range")
	if !validWindows[window] || !validRanges[rng] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "window 必须为 interval/weekly，range 必须为 5h/1h/7d"})
		return
	}
	secs := rangeToSeconds(rng)
	now := time.Now().Unix()
	points, err := quota.QueryQuotaHistory(pid, window, now-secs, now, rng == "7d")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if points == nil {
		points = []quota.QuotaPoint{}
	}
	// 附加 current 快照（来自内存），便于 modal footer 显示
	current := gin.H{}
	if snap := quota.GetCurrentSnapshot(pid, window); snap != nil {
		current = gin.H{
			"used_percent":  snap.UsedPercent,
			"reset_in_human": snap.ResetInHuman,
			"end_time":      snap.EndTime,
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"window":  window,
		"range":   rng,
		"points":  points,
		"current": current,
	})
}

func getTokenHistory(c *gin.Context) {
	pid := c.Param("id")
	rng := c.Query("range")
	if !validRanges[rng] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "range 必须为 5h/1h/7d"})
		return
	}
	secs := rangeToSeconds(rng)
	now := time.Now().Unix()
	points, err := quota.QueryTokenHistory(pid, now-secs, now, rng == "7d")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if points == nil {
		points = []quota.TokenPoint{}
	}
	c.JSON(http.StatusOK, gin.H{
		"range":  rng,
		"points": points,
	})
}
```

Add `quota.GetCurrentSnapshot` in `quota/quota.go` (near other public getters):

```go
// GetCurrentSnapshot returns the latest IntervalWindow or WeeklyWindow for the
// provider's snapshot, or nil if unknown. Used by the API for modal footer.
func GetCurrentSnapshot(providerID, window string) *IntervalWindow {
	stateMu.RLock()
	defer stateMu.RUnlock()
	snap := snapshots[providerID]
	if snap == nil {
		return nil
	}
	switch window {
	case "interval":
		w := snap.Interval
		return &w
	case "weekly":
		w := snap.Weekly
		return &w
	}
	return nil
}
```

Register routes in `web/web.go` `RegisterRoutes`, inside `api` group, after `/providers/:id/quota-block-enabled`:

```go
api.GET("/providers/:id/quota-history", getQuotaHistory)
api.GET("/providers/:id/token-history", getTokenHistory)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./web/ -run 'TestGetQuotaHistory|TestGetTokenHistory' -v`
Expected: PASS (5 tests)

- [ ] **Step 5: Run full test suite to confirm no regression**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add web/web.go web/web_test.go quota/quota.go
git commit -m "feat(web): add quota-history and token-history APIs"
```

---

## Task 4: Frontend — modal DOM + CSS

**Files:**
- Modify: `web/static/index.html` (add Modal block + CSS)

- [ ] **Step 1: Add CSS rules**

After the `.quota-bar-right .pct.quota-red` rule (around line 248), append:

```css
.quota-bar-row[data-pid] { cursor: pointer; transition: background 150ms; padding: 2px 4px; border-radius: 4px; }
.quota-bar-row[data-pid]:hover { background: #f7fafc; }

.modal-header { display: flex; justify-content: space-between; align-items: center; margin-bottom: 16px; padding-bottom: 12px; border-bottom: 1px solid #e2e8f0; }
.modal-header h3 { margin: 0; font-size: 16px; color: #2d3748; }
.modal-close { background: none; border: none; font-size: 24px; line-height: 1; cursor: pointer; color: #999; padding: 0 4px; }
.modal-close:hover { color: #333; }
.modal-footer { display: flex; justify-content: space-between; margin-top: 12px; padding-top: 12px; border-top: 1px solid #e2e8f0; font-size: 13px; color: #4a5568; }
```

- [ ] **Step 2: Add Modal DOM**

After the `<!-- Edit Key Modal -->` block (around line 575, before `<script>` tag), insert:

```html
<!-- Quota Chart Modal -->
<div class="modal" id="quotaChartModal">
    <div class="modal-content" style="max-width: 760px;">
        <div class="modal-header">
            <h3 id="quotaChartTitle">限额趋势</h3>
            <button class="modal-close" onclick="hideQuotaChartModal()">×</button>
        </div>
        <div class="bucket-toggle" id="quotaRangeToggle">
            <button data-range="5h" class="active">5h</button>
            <button data-range="1h">1h</button>
            <button data-range="7d">7d</button>
        </div>
        <div id="quotaChart" style="height: 360px; margin-top: 12px;"></div>
        <div class="modal-footer">
            <span id="quotaChartCurrent"></span>
            <span id="quotaChartReset"></span>
        </div>
    </div>
</div>
```

- [ ] **Step 3: Verify no syntax errors**

Open the file in browser or `cat` the modal block. Look for matching tags. No code execution needed.

- [ ] **Step 4: Commit**

```bash
git add web/static/index.html
git commit -m "feat(web): add quota chart modal DOM + CSS"
```

---

## Task 5: Frontend — click handler + ECharts integration

**Files:**
- Modify: `web/static/index.html` (add JS near `keyCharts` definition at line 2036)

- [ ] **Step 1: Add data-pid/data-window to quota-bar-row**

Find `function renderQuotaBars(p)` (around line 1568). Modify the two row template strings. In `row5h`, change:

```js
let row5h = `
    <div class="quota-bar-row" data-pid="${p.id}" data-window="interval">
        ...
```

In `rowWeek`, change:

```js
let rowWeek = `
    <div class="quota-bar-row" data-pid="${p.id}" data-window="weekly">
        ...
```

DO NOT modify the error case (`if (p.quota_error)`) — that row has no `data-pid` and stays unclickable.

- [ ] **Step 2: Add event delegation + chart logic**

After the `keyCharts` definition (around line 2037), append:

```js
// --- Quota Chart Modal ---
let quotaChartState = null; // {chart, pid, window, range, reqSeq}

function showQuotaChartModal(pid, window) {
    const p = providers.find(x => x.id === pid);
    if (!p) return;
    const winLabel = window === 'interval' ? '5h 限额' : '周限额';
    document.getElementById('quotaChartTitle').textContent = `${winLabel}趋势 - ${p.name}`;

    const modal = document.getElementById('quotaChartModal');
    modal.classList.add('show');

    if (quotaChartState && quotaChartState.pid === pid && quotaChartState.window === window) {
        // already showing — refresh with current range
        loadQuotaChart(quotaChartState.range);
        return;
    }
    if (quotaChartState) {
        quotaChartState.chart.dispose();
    }

    const chartEl = document.getElementById('quotaChart');
    chartEl.innerHTML = '';
    const chart = echarts.init(chartEl, null, { renderer: 'canvas' });
    quotaChartState = { chart, pid, window, range: '5h', reqSeq: 0 };
    new ResizeObserver(() => chart.resize()).observe(chartEl);

    // range toggle
    const toggle = document.getElementById('quotaRangeToggle');
    toggle.querySelectorAll('button').forEach(b => {
        b.onclick = () => {
            toggle.querySelectorAll('button').forEach(x => x.classList.remove('active'));
            b.classList.add('active');
            quotaChartState.range = b.dataset.range;
            loadQuotaChart(b.dataset.range);
        };
    });

    loadQuotaChart('5h');
}

function hideQuotaChartModal() {
    document.getElementById('quotaChartModal').classList.remove('show');
    if (quotaChartState) {
        quotaChartState.chart.dispose();
        quotaChartState = null;
    }
}

async function loadQuotaChart(range) {
    if (!quotaChartState) return;
    const { chart, pid, window, reqSeq } = quotaChartState;
    const mySeq = quotaChartState.reqSeq = reqSeq + 1;
    try {
        const [quotaRes, tokenRes] = await Promise.all([
            fetch(`/api/providers/${pid}/quota-history?window=${window}&range=${range}`),
            fetch(`/api/providers/${pid}/token-history?range=${range}`),
        ]);
        if (mySeq !== quotaChartState.reqSeq) return;
        if (!quotaRes.ok || !tokenRes.ok) throw new Error(`HTTP ${quotaRes.status}/${tokenRes.status}`);

        const quotaData = await quotaRes.json();
        const tokenData = await tokenRes.json();

        chart.setOption(buildQuotaChartOption(quotaData, tokenData, range), true);

        // footer
        const current = quotaData.current || {};
        document.getElementById('quotaChartCurrent').textContent =
            current.used_percent != null ? `当前: ${current.used_percent.toFixed(1)}%` : '';
        document.getElementById('quotaChartReset').textContent =
            current.reset_in_human ? `下次重置: ${current.reset_in_human}` : '';
    } catch (err) {
        if (mySeq !== quotaChartState.reqSeq) return;
        chart.setOption({
            title: { text: `加载失败: ${err.message}`, left: 'center', top: 'center',
                     textStyle: { color: '#e53e3e', fontSize: 14 } }
        }, true);
    }
}

function buildQuotaChartOption(quotaData, tokenData, range) {
    const xAxisRange = { '5h': 5*3600*1000, '1h': 3600*1000, '7d': 7*24*3600*1000 };
    const nowMs = Date.now();
    const startMs = nowMs - xAxisRange[range];

    const qClass = quotaBarClass(quotaData.points.length ? quotaData.points[quotaData.points.length-1].used_percent : 0);
    const colorMap = { cyan: '#22d3ee', orange: '#e67e22', redOrange: '#e67e22', red: '#c0392b' };
    const quotaColor = colorMap[qClass] || '#22d3ee';

    return {
        grid: { left: 60, right: 70, top: 40, bottom: 50 },
        tooltip: { trigger: 'axis', axisPointer: { type: 'cross' } },
        legend: { top: 4, data: ['使用率', '输入', '输出', '总Token'] },
        xAxis: { type: 'time', min: startMs, max: nowMs },
        yAxis: [
            { type: 'value', name: '使用率 %', min: 0, max: 100, position: 'left',
              axisLabel: { formatter: '{value}%' } },
            { type: 'value', name: 'Token', position: 'right',
              axisLabel: { formatter: v => formatTokenCount(v) } },
        ],
        series: [
            { name: '使用率', type: 'line', yAxisIndex: 0, smooth: true,
              connectNulls: true, showSymbol: false,
              itemStyle: { color: quotaColor },
              areaStyle: { color: quotaColor, opacity: 0.15 },
              data: quotaData.points.map(p => [p.t * 1000, p.used_percent]) },
            { name: '输入', type: 'line', yAxisIndex: 1, smooth: true, connectNulls: true, showSymbol: false,
              itemStyle: { color: '#3b82f6' },
              data: tokenData.points.map(p => [p.t * 1000, p.input_tokens]) },
            { name: '输出', type: 'line', yAxisIndex: 1, smooth: true, connectNulls: true, showSymbol: false,
              itemStyle: { color: '#a855f7' },
              data: tokenData.points.map(p => [p.t * 1000, p.output_tokens]) },
            { name: '总Token', type: 'line', yAxisIndex: 1, smooth: true, connectNulls: true, showSymbol: false,
              lineStyle: { width: 3 },
              itemStyle: { color: '#111827' },
              data: tokenData.points.map(p => [p.t * 1000, p.total_tokens]) },
        ],
    };
}

// Event delegation on provider list (bind once, after DOMContentLoaded)
document.getElementById('providerList').addEventListener('click', (e) => {
    const row = e.target.closest('.quota-bar-row[data-pid]');
    if (!row) return;
    if (e.target.closest('.quota-block-toggle')) return; // don't conflict with checkbox
    showQuotaChartModal(row.dataset.pid, row.dataset.window);
});

// WSS-driven refresh: if modal is open and matches the updated provider, refetch.
const _origApplyQuotaUpdate = applyQuotaUpdate;
applyQuotaUpdate = function(quotas) {
    _origApplyQuotaUpdate(quotas);
    if (quotaChartState && quotas[quotaChartState.pid]) {
        loadQuotaChart(quotaChartState.range);
    }
};
```

- [ ] **Step 3: Click providerList binding guard**

Make sure `document.getElementById('providerList')` exists at script-eval time. The existing script is at end of `<body>` (line 2232), so DOM is ready. But check that the listener is bound AFTER `applyQuotaUpdate` is declared (it's a function declaration, so hoisted — safe).

Also check: the original `applyQuotaUpdate` is declared with `function applyQuotaUpdate(...)` at line 1461. Function declarations are hoisted, so the override `applyQuotaUpdate = function(...)` works at the bottom of the file. Verify by reading the area.

- [ ] **Step 4: Verify file loads**

Open the HTML file in a browser (or just check syntax). Look for unbalanced braces. The script is large; one way to validate is:

Run: `node -e "const fs=require('fs'); const html=fs.readFileSync('web/static/index.html','utf8'); const scripts=[...html.matchAll(/<script(?![^>]*src=)[^>]*>([\s\S]*?)<\/script>/g)].map(m=>m[1]); console.log('found '+scripts.length+' inline scripts'); scripts.forEach((s,i)=>{try{new Function(s);console.log('script '+i+' OK')}catch(e){console.log('script '+i+' SYNTAX ERROR: '+e.message)}})"`
Expected: "found N inline scripts" with all "OK"

- [ ] **Step 5: Manual smoke test in browser**

1. `go run .` to start server
2. Navigate to `http://localhost:8080/`, log in
3. In API Providers tab, click "5h 限额" bar
4. Expected: Modal opens with title "5h 限额趋势 - {provider}", default 5h range highlighted, 4 lines drawn
5. Click "周限额" bar → title changes to "周限额趋势 - ..."
6. Click range buttons 1h / 7d → chart reloads

- [ ] **Step 6: Commit**

```bash
git add web/static/index.html
git commit -m "feat(web): quota chart modal with 4-series dual-axis ECharts"
```

---

## Task 6: Verification + cleanup

- [ ] **Step 1: Run full test suite**

Run: `go test ./...`
Expected: all PASS

- [ ] **Step 2: Manual verification per spec §8 acceptance checklist**

Re-read `docs/superpowers/specs/2026-06-30-quota-history-popup-design.md` §8 and tick each box. For each item:
- "点击 5h 限额行 → 弹窗" — manually verify in browser
- "点击周限额行 → 弹窗" — manually verify
- "默认 tab 5h 高亮" — verify modal opens with 5h active
- "切换 1h/7d 重新拉数据" — verify Network tab shows new requests
- "图表包含 4 条线" — verify legend
- "右轴单位自适应 (k/M/B)" — verify by sending >1000 tokens
- "WSS 推送时自动 refetch" — leave modal open, send a request via proxy, verify refetch
- "关闭 modal → dispose" — verify via DevTools Memory profiler or just verify chart.dispose is called
- "7 天自动清理" — verify by inserting 8-day-old row manually + restart

- [ ] **Step 3: Update memory if needed**

If any non-obvious fact was discovered during implementation, save it as a memory. Otherwise skip.

- [ ] **Step 4: Final commit (if any cleanup)**

```bash
git status
# if clean, nothing to commit
# otherwise:
git add -A && git commit -m "chore: post-implementation cleanup"
```

---

## Self-Review Notes

- Spec §2.1 quota_history → Task 2 (RecordQuotaSnapshot, QueryQuotaHistory)
- Spec §2.2 provider_token_history → Task 1 (stats) + Task 2 (quota wrapper for cross-package reuse)
- Spec §3.1 quota-history API → Task 3 (getQuotaHistory, 5h/1h/7d aggregation)
- Spec §3.2 token-history API → Task 3 (getTokenHistory, 0-filter, 7d aggregation)
- Spec §4.1 Modal DOM → Task 4
- Spec §4.2 Trigger via event delegation → Task 5
- Spec §4.3 ECharts dual-axis 4-series → Task 5 (buildQuotaChartOption)
- Spec §4.4 WSS refetch → Task 5 (applyQuotaUpdate override)
- Spec §4.5 Error handling → Task 5 (try/catch + chart title)
- Spec §5 Tests → Tasks 1, 2, 3 all include test-first steps
- Spec §7 YAGNI — respected: no export, no threshold line, no multi-provider overlay

Potential follow-up (NOT in this plan): proxy-layer `RecordTokenBucket` wiring — exact call site depends on proxy.go structure; the engineer should confirm whether `stats.RecordUsage` is called once per request with final counts, or per stream chunk. If per chunk, sum them into the per-request bucket instead of multiple calls.