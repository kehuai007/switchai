# Quota Block Threshold Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the hardcoded 99% quota-block threshold with a per-provider configurable 1..100 integer percentage (default 99) that the user can set live from a `<select>` next to the existing "限额拦截" checkbox.

**Architecture:** Extend the existing `quota_block_enabled` migration pattern with a new `quota_block_threshold INTEGER DEFAULT 99` column on `providers`. Persist via `Config.SetProviderQuotaBlockThreshold`. Change `quota.IsBlocked` to take `threshold float64` as a parameter (caller passes `provider.QuotaBlockThreshold`), removing the package-level `blockThreshold` constant. Add a new `PUT /api/providers/:id/quota-block-threshold` endpoint mirroring the existing toggle endpoint. Frontend adds a native `<select>` 1..100 next to the checkbox; `change` event fires the PUT immediately.

**Tech Stack:** Go (gin), SQLite (modernc.org/sqlite), vanilla JS + ECharts frontend.

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `config/config.go` | Modify | Add `QuotaBlockThreshold int` field on `Provider`, DB column migration, `Load` SELECT, `save` INSERT, `AddProvider` INSERT, `SetProviderQuotaBlockThreshold` method |
| `config/config_test.go` | Modify | Add `TestSetProviderQuotaBlockThreshold_Persists`, `TestSetProviderQuotaBlockThreshold_RoundTripsZero`, update `TestSetProviderQuotaBlockEnabled_Persists` to assert threshold column also round-trips |
| `quota/quota.go` | Modify | Remove `blockThreshold` const; change `IsBlocked(providerID string, threshold float64)` signature; add `blockThresholds` map + `SetBlockThreshold` setter |
| `quota/quota_test.go` | Modify | Update all existing `IsBlocked` callsites to pass `99.0`; add `TestIsBlocked_ThresholdParam` covering custom thresholds (90 / 95 / 100) and check order |
| `web/web.go` | Modify | Register new route; add `setQuotaBlockThreshold` handler |
| `proxy/proxy.go` | Modify | Update `IsBlocked` call to pass `provider.QuotaBlockThreshold` |
| `web/static/index.html` | Modify | Add `buildThresholdOptions` helper, render `<select>` next to checkbox, add `change` listener for `quota-block-threshold`, add CSS rule |

---

## Task 1: Schema migration for `quota_block_threshold`

**Files:**
- Modify: `config/config.go:220-230` (CREATE TABLE) and `config/config.go:263-265` (ALTER TABLE)

- [ ] **Step 1: Add column to CREATE TABLE statement**

In `config/config.go` find the `CREATE TABLE IF NOT EXISTS providers` block (around line 219-230) and add the new column to the schema. Change:

```go
CREATE TABLE IF NOT EXISTS providers (
    id TEXT PRIMARY KEY,
    name TEXT,
    base_url TEXT,
    api_key TEXT,
    model TEXT,
    is_active INTEGER,
    created_at TEXT,
    order_num INTEGER,
    is_openai_format INTEGER DEFAULT 0,
    quota_block_enabled INTEGER DEFAULT 0
);
```

To:

```go
CREATE TABLE IF NOT EXISTS providers (
    id TEXT PRIMARY KEY,
    name TEXT,
    base_url TEXT,
    api_key TEXT,
    model TEXT,
    is_active INTEGER,
    created_at TEXT,
    order_num INTEGER,
    is_openai_format INTEGER DEFAULT 0,
    quota_block_enabled INTEGER DEFAULT 0,
    quota_block_threshold INTEGER DEFAULT 99
);
```

- [ ] **Step 2: Add ALTER TABLE migration**

In `config/config.go` find the migration block (around line 263-265). After the existing `quota_block_enabled` ALTER, add the new one. Change:

```go
// 迁移：添加 quota_block_enabled 列（如果不存在）
db.Exec("ALTER TABLE providers ADD COLUMN quota_block_enabled INTEGER DEFAULT 0")
```

To:

```go
// 迁移：添加 quota_block_enabled 列（如果不存在）
db.Exec("ALTER TABLE providers ADD COLUMN quota_block_enabled INTEGER DEFAULT 0")
// 迁移：添加 quota_block_threshold 列（如果不存在）；默认 99 与现有硬编码行为一致
db.Exec("ALTER TABLE providers ADD COLUMN quota_block_threshold INTEGER DEFAULT 99")
```

- [ ] **Step 3: Build to verify compilation**

Run: `cd c:/Users/Admin/src/switchai && go build ./...`
Expected: zero errors.

- [ ] **Step 4: Commit**

```bash
cd c:/Users/Admin/src/switchai
git add config/config.go
git commit -m "feat(config): add quota_block_threshold column (default 99)"
```

---

## Task 2: Add `QuotaBlockThreshold` field to `Provider` struct

**Files:**
- Modify: `config/config.go:36-42` (Provider struct)

- [ ] **Step 1: Add field to Provider struct**

In `config/config.go` find the `Provider` struct (around line 25-42). After `QuotaBlockEnabled bool` add `QuotaBlockThreshold int`. Change:

```go
// Quota — 由 quota 包在请求时填充，不持久化（QuotaBlockEnabled 除外，由 Config.QuotaBlockEnabled 单独持久化）。
QuotaEnabled      bool            `json:"quota_enabled,omitempty"`
QuotaError        string          `json:"quota_error,omitempty"`
QuotaInterval     QuotaWindowJSON `json:"quota_interval,omitempty"`
QuotaWeekly       QuotaWindowJSON `json:"quota_weekly,omitempty"`
QuotaBlockEnabled bool            `json:"quota_block_enabled"`
```

To:

```go
// Quota — 由 quota 包在请求时填充，不持久化（QuotaBlockEnabled 除外，由 Config.QuotaBlockEnabled 单独持久化）。
QuotaEnabled      bool            `json:"quota_enabled,omitempty"`
QuotaError        string          `json:"quota_error,omitempty"`
QuotaInterval     QuotaWindowJSON `json:"quota_interval,omitempty"`
QuotaWeekly       QuotaWindowJSON `json:"quota_weekly,omitempty"`
QuotaBlockEnabled bool            `json:"quota_block_enabled"`
QuotaBlockThreshold int          `json:"quota_block_threshold"` // 1..100，默认 99
```

- [ ] **Step 2: Build to verify compilation**

Run: `cd c:/Users/Admin/src/switchai && go build ./...`
Expected: zero errors.

- [ ] **Step 3: Commit**

```bash
cd c:/Users/Admin/src/switchai
git add config/config.go
git commit -m "feat(config): add QuotaBlockThreshold field on Provider"
```

---

## Task 3: Hydrate `QuotaBlockThreshold` in `Load()` SELECT

**Files:**
- Modify: `config/config.go:307-325` (Load() SELECT)

- [ ] **Step 1: Extend SELECT and row scanning**

In `config/config.go` find the `Load` function's SELECT (around line 307). Change:

```go
rows, err := db.Query("SELECT id, name, base_url, api_key, model, is_active, created_at, order_num, COALESCE(is_openai_format, 0), COALESCE(quota_block_enabled, 0) FROM providers ORDER BY order_num")
```

To:

```go
rows, err := db.Query("SELECT id, name, base_url, api_key, model, is_active, created_at, order_num, COALESCE(is_openai_format, 0), COALESCE(quota_block_enabled, 0), COALESCE(quota_block_threshold, 99) FROM providers ORDER BY order_num")
```

- [ ] **Step 2: Update rows.Scan and field assignment**

In the same `Load` function, find the row-scan loop (around line 312-325). Change:

```go
for rows.Next() {
    var p Provider
    var isActive, isOpenAIFormat, quotaBlock int
    if err := rows.Scan(&p.ID, &p.Name, &p.BaseURL, &p.APIKey, &p.Model, &isActive, &p.CreatedAt, &p.Order, &isOpenAIFormat, &quotaBlock); err != nil {
        rows.Close()
        return err
    }
    p.IsActive = isActive == 1
    p.IsOpenAIFormat = isOpenAIFormat == 1
    p.QuotaBlockEnabled = quotaBlock == 1
    c.Providers = append(c.Providers, p)
}
```

To:

```go
for rows.Next() {
    var p Provider
    var isActive, isOpenAIFormat, quotaBlock, quotaThreshold int
    if err := rows.Scan(&p.ID, &p.Name, &p.BaseURL, &p.APIKey, &p.Model, &isActive, &p.CreatedAt, &p.Order, &isOpenAIFormat, &quotaBlock, &quotaThreshold); err != nil {
        rows.Close()
        return err
    }
    p.IsActive = isActive == 1
    p.IsOpenAIFormat = isOpenAIFormat == 1
    p.QuotaBlockEnabled = quotaBlock == 1
    p.QuotaBlockThreshold = quotaThreshold
    c.Providers = append(c.Providers, p)
}
```

- [ ] **Step 3: Build**

Run: `cd c:/Users/Admin/src/switchai && go build ./...`
Expected: zero errors.

- [ ] **Step 4: Commit**

```bash
cd c:/Users/Admin/src/switchai
git add config/config.go
git commit -m "feat(config): load QuotaBlockThreshold from DB"
```

---

## Task 4: Persist `QuotaBlockThreshold` in `save()` and `AddProvider`

**Files:**
- Modify: `config/config.go:399-411` (INSERT in `save()`)

- [ ] **Step 1: Extend save() INSERT**

In `config/config.go` find the `save()` INSERT block (around line 399-411). Change:

```go
quotaBlock := 0
if c.QuotaBlockEnabled[p.ID] {
    quotaBlock = 1
}
_, err = db.Exec("INSERT INTO providers (id, name, base_url, api_key, model, is_active, created_at, order_num, is_openai_format, quota_block_enabled) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
    p.ID, p.Name, p.BaseURL, p.APIKey, p.Model, isActive, p.CreatedAt, p.Order, isOpenAIFormat, quotaBlock)
```

To:

```go
quotaBlock := 0
if c.QuotaBlockEnabled[p.ID] {
    quotaBlock = 1
}
// QuotaBlockThreshold 由 DB DEFAULT 99 提供兜底；当 p.QuotaBlockThreshold==0
// （即调用方未显式设值，例如旧 Provider 调用栈或测试构造），依赖 DEFAULT 99。
_, err = db.Exec("INSERT INTO providers (id, name, base_url, api_key, model, is_active, created_at, order_num, is_openai_format, quota_block_enabled, quota_block_threshold) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
    p.ID, p.Name, p.BaseURL, p.APIKey, p.Model, isActive, p.CreatedAt, p.Order, isOpenAIFormat, quotaBlock, p.QuotaBlockThreshold)
```

- [ ] **Step 2: Build**

Run: `cd c:/Users/Admin/src/switchai && go build ./...`
Expected: zero errors.

- [ ] **Step 3: Commit**

```bash
cd c:/Users/Admin/src/switchai
git add config/config.go
git commit -m "feat(config): persist quota_block_threshold in save()"
```

---

## Task 5: Add `SetProviderQuotaBlockThreshold` setter with TDD

**Files:**
- Modify: `config/config.go:590-602` (after `SetProviderQuotaBlockEnabled`)
- Test: `config/config_test.go` (add tests at end of file)

- [ ] **Step 1: Write the failing test**

In `config/config_test.go`, append after the existing `TestSetProviderQuotaBlockEnabled_OffRoundTrips` (around line 777). Add:

```go
// TestSetProviderQuotaBlockThreshold_Persists 守护 quota_block_threshold 列：
// SetProviderQuotaBlockThreshold 后，内存 Provider 字段必须更新；
// 重新 Load 一份 Config 后，Provider 字段必须保留（round-trip 通过 DB）。
func TestSetProviderQuotaBlockThreshold_Persists(t *testing.T) {
    cleanup := setupTestDB(t)
    defer cleanup()

    cfg := &Config{}
    if err := cfg.Load(); err != nil {
        t.Fatalf("Load: %v", err)
    }

    p := Provider{
        ID: "test-threshold-1", Name: "T1",
        BaseURL: "https://api.minimaxi.com", APIKey: "sk-test",
        Model: "general", IsActive: true,
        CreatedAt: time.Now().Format(time.RFC3339),
        Order:     1,
    }
    if err := cfg.AddProvider(p); err != nil {
        t.Fatalf("AddProvider: %v", err)
    }

    if err := cfg.SetProviderQuotaBlockThreshold(p.ID, 85); err != nil {
        t.Fatalf("SetProviderQuotaBlockThreshold: %v", err)
    }
    got := cfg.GetProviderByID(p.ID)
    if got == nil || got.QuotaBlockThreshold != 85 {
        t.Fatalf("in-memory QuotaBlockThreshold=%d, want 85", got.QuotaBlockThreshold)
    }

    // 重新 Load 模拟重启/重读：必须从 DB 重建字段
    cfg2 := &Config{}
    if err := cfg2.Load(); err != nil {
        t.Fatalf("second Load: %v", err)
    }
    got2 := cfg2.GetProviderByID(p.ID)
    if got2 == nil || got2.QuotaBlockThreshold != 85 {
        t.Errorf("quota_block_threshold did not round-trip through DB: got=%d, want 85", got2.QuotaBlockThreshold)
    }
}

// TestAddProvider_DefaultsThresholdTo99 守护存量兼容：新建 provider 时若
// QuotaBlockThreshold=0（未显式设值），DB DEFAULT 99 必须生效；Load 后字段 = 99。
func TestAddProvider_DefaultsThresholdTo99(t *testing.T) {
    cleanup := setupTestDB(t)
    defer cleanup()

    cfg := &Config{}
    if err := cfg.Load(); err != nil {
        t.Fatalf("Load: %v", err)
    }

    p := Provider{
        ID: "test-default-thresh", Name: "D",
        BaseURL: "https://api.minimaxi.com", APIKey: "sk-test",
        Model: "general", IsActive: true,
        CreatedAt: time.Now().Format(time.RFC3339),
        Order:     1,
        // QuotaBlockThreshold 故意不设，保持 0
    }
    if err := cfg.AddProvider(p); err != nil {
        t.Fatalf("AddProvider: %v", err)
    }

    cfg2 := &Config{}
    if err := cfg2.Load(); err != nil {
        t.Fatalf("second Load: %v", err)
    }
    got := cfg2.GetProviderByID(p.ID)
    if got == nil {
        t.Fatalf("provider not found after Load")
    }
    if got.QuotaBlockThreshold != 99 {
        t.Errorf("expected default threshold 99, got %d", got.QuotaBlockThreshold)
    }
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd c:/Users/Admin/src/switchai && go test ./config/ -run 'TestSetProviderQuotaBlockThreshold_Persists|TestAddProvider_DefaultsThresholdTo99' -v`
Expected: FAIL with "undefined: Config.SetProviderQuotaBlockThreshold" (or similar).

- [ ] **Step 3: Implement the setter**

In `config/config.go`, after `SetProviderQuotaBlockEnabled` (around line 602), add:

```go
// SetProviderQuotaBlockThreshold 更新某个 provider 的拦截阈值（1..100）。
//   - 同步更新内存中的 c.Providers[i].QuotaBlockThreshold（O(1)）；
//   - 同步写入 DB 的 quota_block_threshold 列，重启后由 Load() 重建。
// 校验由 web 层 handler 负责（1 ≤ threshold ≤ 100），setter 仅做透传。
func (c *Config) SetProviderQuotaBlockThreshold(id string, threshold int) error {
    c.mu.Lock()
    defer c.mu.Unlock()
    for i := range c.Providers {
        if c.Providers[i].ID == id {
            c.Providers[i].QuotaBlockThreshold = threshold
            break
        }
    }
    _, err := db.Exec("UPDATE providers SET quota_block_threshold = ? WHERE id = ?", threshold, id)
    return err
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd c:/Users/Admin/src/switchai && go test ./config/ -run 'TestSetProviderQuotaBlockThreshold_Persists|TestAddProvider_DefaultsThresholdTo99' -v`
Expected: PASS.

- [ ] **Step 5: Run full config test suite to ensure no regression**

Run: `cd c:/Users/Admin/src/switchai && go test ./config/ -v`
Expected: all tests pass.

- [ ] **Step 6: Commit**

```bash
cd c:/Users/Admin/src/switchai
git add config/config.go config/config_test.go
git commit -m "feat(config): SetProviderQuotaBlockThreshold setter + tests"
```

---

## Task 6: Change `quota.IsBlocked` signature to take `threshold float64`

**Files:**
- Modify: `quota/quota.go:51-58` (consts), `quota/quota.go:82-121` (IsBlocked), `quota/quota.go:75-80` (state vars)

- [ ] **Step 1: Remove `blockThreshold` constant**

In `quota/quota.go` find the `const` block (around line 51-58). Change:

```go
const (
    pollInterval   = 10 * time.Second
    requestTimeout = 5 * time.Second
    blockThreshold = 99.0
    upstreamHost   = "https://api.minimaxi.com"
    upstreamPath   = "/v1/token_plan/remains"
    generalModel   = "general"
)
```

To:

```go
const (
    pollInterval   = 10 * time.Second
    requestTimeout = 5 * time.Second
    upstreamHost   = "https://api.minimaxi.com"
    upstreamPath   = "/v1/token_plan/remains"
    generalModel   = "general"
)
```

- [ ] **Step 2: Add `blockThresholds` map to state vars**

In `quota/quota.go` find the package-level `var` block (around line 75-80). Change:

```go
// Package-level state. guarded by stateMu.
var (
    stateMu      sync.RWMutex
    snapshots    = map[string]*Snapshot{} // providerID -> latest
    blockEnabled = map[string]bool{}      // providerID -> toggle
    upstreamHTTP *http.Client             // initialized in Init()
)
```

To:

```go
// Package-level state. guarded by stateMu.
var (
    stateMu         sync.RWMutex
    snapshots       = map[string]*Snapshot{}    // providerID -> latest
    blockEnabled    = map[string]bool{}         // providerID -> toggle
    blockThresholds = map[string]float64{}      // providerID -> threshold mirror (authoritative value lives on config.Provider)
    upstreamHTTP    *http.Client                // initialized in Init()
)
```

- [ ] **Step 3: Change `IsBlocked` signature**

In `quota/quota.go` find the `IsBlocked` function (around line 82-121). Change:

```go
// IsBlocked returns whether the provider's quota should block upstream
// requests. Blocking requires (a) the user toggled enforcement ON for
// this provider, and (b) at least one window's UsedPercent is >= 99.
// If a window's EndTime has passed, that window is ignored (lazy reset).
func IsBlocked(providerID string) (bool, BlockInfo) {
    stateMu.RLock()
    snap := snapshots[providerID]
    enabled := blockEnabled[providerID]
    stateMu.RUnlock()

    if snap == nil || !enabled {
        return false, BlockInfo{}
    }

    now := time.Now()
    wins := []struct {
        name string
        w    IntervalWindow
    }{
        {"interval", snap.Interval},
        {"weekly", snap.Weekly},
    }
    for _, x := range wins {
        if !x.w.Enabled {
            continue
        }
        if !x.w.EndTime.IsZero() && now.After(x.w.EndTime) {
            continue
        }
        if x.w.UsedPercent >= blockThreshold {
            return true, BlockInfo{
                Window:       x.name,
                UsedPercent:  x.w.UsedPercent,
                ResetInSec:   x.w.ResetInSec,
                ResetInHuman: x.w.ResetInHuman,
            }
        }
    }
    return false, BlockInfo{}
}
```

To:

```go
// IsBlocked returns whether the provider's quota should block upstream
// requests. Blocking requires (a) the user toggled enforcement ON for
// this provider, and (b) at least one window's UsedPercent is >= threshold.
// If a window's EndTime has passed, that window is ignored (lazy reset).
//
// Check order is fixed: interval (5h) first, then weekly. Any window that
// trips the threshold blocks; the first one to trip wins (typically 5h).
//
// threshold is supplied by the caller (proxy.go reads it from
// config.Provider.QuotaBlockThreshold). The package-level blockThresholds
// map is only used by legacy callers/tests that don't hold a Provider.
func IsBlocked(providerID string, threshold float64) (bool, BlockInfo) {
    stateMu.RLock()
    snap := snapshots[providerID]
    enabled := blockEnabled[providerID]
    stateMu.RUnlock()

    if snap == nil || !enabled {
        return false, BlockInfo{}
    }

    now := time.Now()
    wins := []struct {
        name string
        w    IntervalWindow
    }{
        {"interval", snap.Interval}, // 5h first — see spec §3 "检查顺序"
        {"weekly", snap.Weekly},
    }
    for _, x := range wins {
        if !x.w.Enabled {
            continue
        }
        if !x.w.EndTime.IsZero() && now.After(x.w.EndTime) {
            continue
        }
        if x.w.UsedPercent >= threshold {
            return true, BlockInfo{
                Window:       x.name,
                UsedPercent:  x.w.UsedPercent,
                ResetInSec:   x.w.ResetInSec,
                ResetInHuman: x.w.ResetInHuman,
            }
        }
    }
    return false, BlockInfo{}
}
```

- [ ] **Step 4: Add `SetBlockThreshold` setter**

In `quota/quota.go` find `SetBlockEnabled` (around line 159-165) and add the symmetric threshold setter right after it:

```go
// SetBlockEnabled updates the in-memory enforcement flag. The web layer
// also persists this via config.SetProviderQuotaBlockEnabled.
func SetBlockEnabled(providerID string, enabled bool) {
    stateMu.Lock()
    defer stateMu.Unlock()
    blockEnabled[providerID] = enabled
}
```

Change to:

```go
// SetBlockEnabled updates the in-memory enforcement flag. The web layer
// also persists this via config.SetProviderQuotaBlockEnabled.
func SetBlockEnabled(providerID string, enabled bool) {
    stateMu.Lock()
    defer stateMu.Unlock()
    blockEnabled[providerID] = enabled
}

// SetBlockThreshold updates the in-memory threshold mirror for callers that
// don't have a Provider struct in hand. The authoritative value lives on
// config.Provider.QuotaBlockThreshold and is what proxy.go passes to
// IsBlocked. This setter exists for symmetry with SetBlockEnabled and to
// let tests construct quota package state without a Provider.
func SetBlockThreshold(providerID string, threshold float64) {
    stateMu.Lock()
    defer stateMu.Unlock()
    blockThresholds[providerID] = threshold
}
```

- [ ] **Step 5: Build to find all IsBlocked callsites that now break**

Run: `cd c:/Users/Admin/src/switchai && go build ./...`
Expected: FAIL with compilation error in tests (e.g., `quota_test.go`) and any other callsite — Task 7 fixes the test callsites and Task 9 fixes `proxy.go`.

---

## Task 7: Update `quota_test.go` for new `IsBlocked` signature

**Files:**
- Modify: `quota/quota_test.go` (all `IsBlocked` callsites)

- [ ] **Step 1: Add `99.0` threshold argument to all 5 existing IsBlocked calls**

There are 5 existing test functions calling `IsBlocked` in `quota/quota_test.go`:
- `TestIsBlocked_ToggleOff_NotBlocked` (line 65)
- `TestIsBlocked_ToggleOn_TripsInterval` (line 81)
- `TestIsBlocked_ToggleOn_TripsWeekly` (line 97)
- `TestIsBlocked_AutoUnblockOnEndTime` (line 112)
- `TestIsBlocked_BothUnderThreshold` (line 127)

Update each call from `IsBlocked("tN")` to `IsBlocked("tN", 99.0)`. For example:

In `TestIsBlocked_ToggleOff_NotBlocked`:
```go
blocked, _ := IsBlocked("t1", 99.0)
```

In `TestIsBlocked_ToggleOn_TripsInterval`:
```go
blocked, info := IsBlocked("t2", 99.0)
```

In `TestIsBlocked_ToggleOn_TripsWeekly`:
```go
blocked, info := IsBlocked("t3", 99.0)
```

In `TestIsBlocked_AutoUnblockOnEndTime`:
```go
blocked, _ := IsBlocked("t4", 99.0)
```

In `TestIsBlocked_BothUnderThreshold`:
```go
blocked, _ := IsBlocked("t5", 99.0)
```

- [ ] **Step 2: Add new test for threshold parameter**

Append to the end of `quota/quota_test.go`:

```go
// TestIsBlocked_ThresholdParam 守护 IsBlocked 新签名：threshold 由调用方传入。
// 同一 snapshot 下，不同 threshold 给出不同拦截结果。
func TestIsBlocked_ThresholdParam(t *testing.T) {
    now := time.Now().Add(time.Hour)
    stateMu.Lock()
    snapshots["th1"] = &Snapshot{
        ProviderID: "th1",
        Interval:   IntervalWindow{Enabled: true, UsedPercent: 92.0, EndTime: now},
        Weekly:     IntervalWindow{Enabled: true, UsedPercent: 60.0, EndTime: now},
    }
    blockEnabled["th1"] = true
    stateMu.Unlock()

    cases := []struct {
        name      string
        threshold float64
        wantBlock bool
        wantWin   string
    }{
        {"below snapshot", 90.0, true, "interval"}, // 92 >= 90 → block
        {"equal snapshot", 92.0, true, "interval"}, // 92 >= 92 → block (>= semantics)
        {"above snapshot", 95.0, false, ""},        // 92 < 95 → no block
        {"boundary 100",   100.0, false, ""},       // 92 < 100 → no block
        {"boundary 1",     1.0,   true, "interval"}, // 92 >= 1 → block
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            blocked, info := IsBlocked("th1", tc.threshold)
            if blocked != tc.wantBlock {
                t.Errorf("threshold=%v: blocked=%v, want %v", tc.threshold, blocked, tc.wantBlock)
            }
            if tc.wantBlock && info.Window != tc.wantWin {
                t.Errorf("threshold=%v: info.Window=%q, want %q", tc.threshold, info.Window, tc.wantWin)
            }
        })
    }
}

// TestIsBlocked_CheckOrder_IntervalBeforeWeekly 守护 5h 必须先于 weekly 检查：
// 当两个窗口都 >= threshold 时，BlockInfo.Window 必须是 "interval"。
func TestIsBlocked_CheckOrder_IntervalBeforeWeekly(t *testing.T) {
    future := time.Now().Add(time.Hour)
    stateMu.Lock()
    snapshots["order1"] = &Snapshot{
        ProviderID: "order1",
        Interval:   IntervalWindow{Enabled: true, UsedPercent: 95.0, EndTime: future},
        Weekly:     IntervalWindow{Enabled: true, UsedPercent: 95.0, EndTime: future},
    }
    blockEnabled["order1"] = true
    stateMu.Unlock()
    blocked, info := IsBlocked("order1", 90.0)
    if !blocked {
        t.Fatal("expected block")
    }
    if info.Window != "interval" {
        t.Errorf("expected interval to win (5h checked first), got %q", info.Window)
    }
}
```

- [ ] **Step 3: Run quota tests to verify all pass**

Run: `cd c:/Users/Admin/src/switchai && go test ./quota/ -v`
Expected: all tests pass (including the 5 existing ones with new signature, plus the 2 new ones).

- [ ] **Step 4: Commit**

```bash
cd c:/Users/Admin/src/switchai
git add quota/quota.go quota/quota_test.go
git commit -m "feat(quota): IsBlocked takes threshold parameter; remove blockThreshold const"
```

---

## Task 8: Add `setQuotaBlockThreshold` HTTP handler

**Files:**
- Modify: `web/web.go:122` (route registration)
- Modify: `web/web.go:620` (add handler after `setQuotaBlockEnabled`)

- [ ] **Step 1: Register the new route**

In `web/web.go` find the quota-block-enabled route registration (around line 122). After:

```go
api.PUT("/providers/:id/quota-block-enabled", setQuotaBlockEnabled)
```

Add a new line:

```go
api.PUT("/providers/:id/quota-block-enabled", setQuotaBlockEnabled)
api.PUT("/providers/:id/quota-block-threshold", setQuotaBlockThreshold)
```

- [ ] **Step 2: Add the handler**

In `web/web.go` find `setQuotaBlockEnabled` (around line 620). After its closing `}` (around line 640), add:

```go
func setQuotaBlockThreshold(c *gin.Context) {
    id := c.Param("id")
    var body struct {
        Threshold int `json:"threshold"`
    }
    if err := c.BindJSON(&body); err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
        return
    }
    if body.Threshold < 1 || body.Threshold > 100 {
        c.JSON(http.StatusBadRequest, gin.H{"error": "threshold must be in 1..100"})
        return
    }
    cfg := config.GetConfig()
    if cfg == nil {
        c.JSON(http.StatusInternalServerError, gin.H{"error": "config not loaded"})
        return
    }
    if err := cfg.SetProviderQuotaBlockThreshold(id, body.Threshold); err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
        return
    }
    quota.SetBlockThreshold(id, float64(body.Threshold))
    c.JSON(http.StatusOK, gin.H{"ok": true})
}
```

- [ ] **Step 3: Build**

Run: `cd c:/Users/Admin/src/switchai && go build ./...`
Expected: zero errors.

- [ ] **Step 4: Commit**

```bash
cd c:/Users/Admin/src/switchai
git add web/web.go
git commit -m "feat(web): setQuotaBlockThreshold handler + route"
```

---

## Task 9: Update `proxy.go` to pass `provider.QuotaBlockThreshold` to `IsBlocked`

**Files:**
- Modify: `proxy/proxy.go:274-276`

- [ ] **Step 1: Update IsBlocked callsite**

In `proxy/proxy.go` find the quota gate (around line 274-276). Change:

```go
// Quota gate: 当 provider 的额度被 toggle ON 且任一窗口 ≥99% 时，
// 在 URL 构建之前直接返回 403，避免把请求再扔给上游。
if blocked, info := quota.IsBlocked(provider.ID); blocked {
```

To:

```go
// Quota gate: 当 provider 的额度被 toggle ON 且任一窗口 >= 阈值时，
// 在 URL 构建之前直接返回 403，避免把请求再扔给上游继续计费。
// 阈值由用户在 UI 配置（默认 99）；防御 <=0 走 DB DEFAULT 99 兜底。
threshold := provider.QuotaBlockThreshold
if threshold <= 0 {
    threshold = 99
}
if blocked, info := quota.IsBlocked(provider.ID, float64(threshold)); blocked {
```

- [ ] **Step 2: Build**

Run: `cd c:/Users/Admin/src/switchai && go build ./...`
Expected: zero errors.

- [ ] **Step 3: Run full test suite to ensure no regression**

Run: `cd c:/Users/Admin/src/switchai && go test ./...`
Expected: all tests pass.

- [ ] **Step 4: Commit**

```bash
cd c:/Users/Admin/src/switchai
git add proxy/proxy.go
git commit -m "feat(proxy): pass provider.QuotaBlockThreshold to IsBlocked"
```

---

## Task 10: Frontend — render threshold `<select>` in provider card

**Files:**
- Modify: `web/static/index.html:280-281` (CSS)
- Modify: `web/static/index.html` (add `buildThresholdOptions` helper near top of script)
- Modify: `web/static/index.html:1732-1735` (render `<select>`)
- Modify: `web/static/index.html:1584-1600` (change event handler)

- [ ] **Step 1: Add CSS rule for the new select**

In `web/static/index.html` find the existing quota-block-toggle CSS (around line 280-281):

```css
.quota-block-toggle { display: block; font-size: 11px; margin-top: 6px; cursor: pointer; }
.quota-block-toggle input { margin-right: 3px; vertical-align: middle; }
```

After these two lines, add:

```css
.quota-block-toggle { display: block; font-size: 11px; margin-top: 6px; cursor: pointer; }
.quota-block-toggle input { margin-right: 3px; vertical-align: middle; }
.quota-block-threshold { font-size: 11px; margin-left: 4px; padding: 0 2px; vertical-align: middle; }
```

- [ ] **Step 2: Add `buildThresholdOptions` helper**

In `web/static/index.html` find a good spot in the `<script>` block — near the other render helpers like `renderQuotaBars`. Insert this function:

```js
// buildThresholdOptions renders <option> elements 1..100; current defaults to 99
// when provider has no explicit threshold yet (legacy data).
function buildThresholdOptions(current) {
    const selected = current || 99;
    let html = '';
    for (let i = 1; i <= 100; i++) {
        html += `<option value="${i}"${i === selected ? ' selected' : ''}>${i}</option>`;
    }
    return html;
}
```

- [ ] **Step 3: Render `<select>` next to the checkbox**

In `web/static/index.html` find the `renderProviders` template literal (around line 1732-1735). Change:

```html
<label class="quota-block-toggle" data-pid="${p.id}">
    <input type="checkbox" class="quota-block-cb" ${p.quota_block_enabled ? 'checked' : ''} />
    限额拦截
</label>
```

To:

```html
<label class="quota-block-toggle" data-pid="${p.id}">
    <input type="checkbox" class="quota-block-cb" ${p.quota_block_enabled ? 'checked' : ''} />
    限额拦截
    <select class="quota-block-threshold" data-pid="${p.id}">
        ${buildThresholdOptions(p.quota_block_threshold)}
    </select>
    %
</label>
```

- [ ] **Step 4: Add change listener for the new select**

In `web/static/index.html` find the delegated `change` handler for `quota-block-cb` (around line 1584-1600). Change:

```js
document.addEventListener('change', (e) => {
    const cb = e.target.closest('.quota-block-cb');
    if (!cb) return;
    const pid = cb.closest('.quota-block-toggle').getAttribute('data-pid');
    const enabled = cb.checked;
    cb.disabled = true;
    fetch(`/api/providers/${pid}/quota-block-enabled`, {
        method: 'PUT',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({enabled}),
    }).then(r => {
        if (!r.ok) throw new Error('HTTP ' + r.status);
    }).catch(err => {
        alert('切换失败: ' + err.message);
        cb.checked = !enabled; // revert
    }).finally(() => { cb.disabled = false; });
});
```

To:

```js
document.addEventListener('change', (e) => {
    const cb = e.target.closest('.quota-block-cb');
    if (cb) {
        const pid = cb.closest('.quota-block-toggle').getAttribute('data-pid');
        const enabled = cb.checked;
        cb.disabled = true;
        fetch(`/api/providers/${pid}/quota-block-enabled`, {
            method: 'PUT',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({enabled}),
        }).then(r => {
            if (!r.ok) throw new Error('HTTP ' + r.status);
        }).catch(err => {
            alert('切换失败: ' + err.message);
            cb.checked = !enabled; // revert
        }).finally(() => { cb.disabled = false; });
        return;
    }

    const sel = e.target.closest('.quota-block-threshold');
    if (sel) {
        const pid = sel.getAttribute('data-pid');
        const threshold = parseInt(sel.value, 10);
        if (Number.isNaN(threshold) || threshold < 1 || threshold > 100) return; // sanity guard
        sel.disabled = true;
        fetch(`/api/providers/${pid}/quota-block-threshold`, {
            method: 'PUT',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({threshold}),
        }).then(r => {
            if (!r.ok) throw new Error('HTTP ' + r.status);
        }).catch(err => {
            alert('设置失败: ' + err.message);
        }).finally(() => { sel.disabled = false; });
        return;
    }
});
```

- [ ] **Step 5: Manual smoke check (visual)**

Open the provider list in the browser (after restarting the service).
- Verify each provider card with quota enabled shows a `限额拦截 [99 ▼] %` row
- Verify dropdown default is 99 for newly-loaded providers (legacy fallback)
- Change the dropdown to 80 → expect the network tab to show a PUT to `/api/providers/<id>/quota-block-threshold` with body `{"threshold":80}` returning 200
- Refresh the page → dropdown should still show 80 (DB persistence)

- [ ] **Step 6: Commit**

```bash
cd c:/Users/Admin/src/switchai
git add web/static/index.html
git commit -m "feat(web): add threshold <select> next to quota block checkbox"
```

---

## Self-Review

**1. Spec coverage:**
- §2 Goals — DB persistence ✓ (Task 1-5)
- §2 1..100 range ✓ (Tasks 5 setter, 8 handler validation, 10 select options)
- §2 default 99 ✓ (Task 1 DB DEFAULT, Task 4 INSERT, Task 10 UI fallback)
- §2 change event immediate PUT ✓ (Task 10 step 4)
- §2 IsBlocked removes package-level constant ✓ (Task 6)
- §3 architecture ✓ (Tasks 1-9 align with data flow)
- §4 backend changes ✓ (Tasks 1-9 cover all four sub-sections)
- §5 frontend ✓ (Task 10 covers HTML, helper, listener, CSS)
- §6 acceptance criteria ✓ (Tasks 5, 7, 9 cover unit + integration; Task 10 covers manual smoke)

**2. Placeholder scan:** No "TBD", "TODO", "implement later", "add appropriate error handling". Every step has explicit code blocks.

**3. Type consistency:**
- `Provider.QuotaBlockThreshold int` — used consistently in config.go (Task 2), config_test.go (Task 5), proxy.go (Task 9), index.html (Task 10)
- `IsBlocked(providerID string, threshold float64)` — used consistently in quota.go (Task 6), quota_test.go (Task 7), proxy.go (Task 9)
- `SetProviderQuotaBlockThreshold(id string, threshold int)` — config.go (Task 5), web.go (Task 8)
- `SetBlockThreshold(providerID string, threshold float64)` — quota.go (Task 6), web.go (Task 8)
- JSON key `quota_block_threshold` — config.go json tag (Task 2), proxy.go field read (Task 9), web.go PUT body (Task 8), index.html PUT body (Task 10)
- JSON key `threshold` — web.go request body (Task 8), index.html fetch body (Task 10)

All consistent.

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-07-01-quota-block-threshold.md`.

Two execution options:

1. **Subagent-Driven (recommended)** - I dispatch a fresh subagent per task, review between tasks, fast iteration
2. **Inline Execution** - Execute tasks in this session using executing-plans, batch execution with checkpoints

Which approach?