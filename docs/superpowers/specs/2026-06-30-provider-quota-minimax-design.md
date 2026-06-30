# Provider Quota (MiniMax Token-Plan) — Design v2

Date: 2026-06-30 (revised after live API response captured)
Status: Draft — pending user review

## Goal

For every active Provider whose `base_url` matches the MiniMax token-plan host, poll the upstream account's remaining token-plan quota every 10 seconds. Each provider exposes **two independent windows** — `interval` (5h) and `weekly`. Surface both windows as a card in `provider-item-stats` with a per-window used-percent and reset countdown. Allow the user to toggle **per-provider block enforcement** (a checkbox on each card, default OFF, persisted to DB). When enforcement is ON **and** any window reaches used-percent ≥ 99%, block all upstream traffic routed through that Provider with HTTP 403 plus a human-readable message indicating which window tripped. When the offending window's `end_time` passes, auto-unblock.

## Non-goals

- Quota for non-MiniMax providers (Anthropic / OpenAI / others).
- Historical quota charts / long-term storage of quota samples. Memory-only, latest snapshot.
- Manual override / unlock button in MVP.
- Per-server-key quota splitting — quota is account-scoped, all keys share the same upstream pool.

## Constraints

- Must follow existing `ALTER TABLE providers ADD COLUMN …` migration pattern (the same one used for `is_openai_format` — see `config/config.go:238`). Migration runs at startup with ignored errors.
- Must not change existing `stats` package — quota is a separate, peer-level package; its WebSocket push piggybacks on the existing `/api/ws` channel that `stats` already broadcasts on.
- Must not intercept `testProvider` or `fetchModelsByCredentials` admin endpoints.
- Must follow existing patterns: `time.NewTicker + for range` like `logger.checkLogRotation`; `init/Shutdown` like `stats.Init/Shutdown`.

## Architecture

New package `quota/` (peer of `stats/`, `history/`).

```
proxy/proxy.go ──► quota.IsBlocked(providerID) ──► 403 if block-enabled AND any window ≥99%
web/web.go     ──► quota.Snapshot()           ──► /api/providers returns quota_*
web/web.go     ──► PUT /api/providers/:id/quota-block-enabled
main.go        ──► quota.Init(ctx) / Shutdown()
quota/quota.go ──► 10s ticker → upstream GET → updates map[providerID]*Snapshot
                  └──► pushes to stats.broadcast on every change → /api/ws → frontend
```

No new WebSocket endpoint — quota events ride the existing `data.provider_stats` (or a new sibling event) on `/api/ws`.

## Upstream response shape (live capture)

The endpoint `https://api.minimaxi.com/v1/token_plan/remains` returns:

```json
{
  "model_remains": [
    {
      "start_time": 1782784800000,
      "end_time": 1782802800000,
      "remains_time": 12367424,
      "current_interval_total_count": 0,
      "current_interval_usage_count": 0,
      "model_name": "general",
      "current_weekly_total_count": 0,
      "current_weekly_usage_count": 0,
      "weekly_start_time": 1782662400000,
      "weekly_end_time": 1783267200000,
      "weekly_remains_time": 476767424,
      "current_interval_status": 1,
      "current_interval_remaining_percent": 81,
      "current_weekly_status": 3,
      "current_weekly_remaining_percent": 100
    },
    {
      "start_time": 1782748800000,
      "end_time": 1782835200000,
      "model_name": "video",
      "current_interval_remaining_percent": 100,
      "current_weekly_remaining_percent": 100,
      "..."
    }
  ],
  "base_resp": { "status_code": 0, "status_msg": "success" }
}
```

Key facts that drive the design:

- `model_remains` is **an array** — multiple models per account.
- We **always pick the entry where `model_name == "general"`**. If absent, the response is treated as "no quota data" (snapshot stays in last-error state, never blocks).
- `provider.Model` is **not used** for quota model selection — only `general` matters regardless of what models the provider serves.
- Timestamps are **milliseconds** (Unix epoch ms), not seconds.
- Each entry has **two independent windows**: `interval` (≈5h) and `weekly`.
- Used-percent is derived from `remaining_percent`, **not** computed from `total - remains`: `used_pct = 100 - remaining_percent`.
- `base_resp.status_code != 0` is an error response (treat as upstream failure).
- The `www.minimaxi.com` host is **not** tried — the live-tested working endpoint is `api.minimaxi.com`. Spec removes the host fallback list and pins `https://api.minimaxi.com` directly.

## Host detection

`isQuotaHost(baseURL string) bool` — hardcoded suffix `minimaxi.com`, case-insensitive, exact-host or `*.minimaxi.com` accepted. Same as v1.

## Components

### 1. `quota/quota.go` — new package

Public types:

```go
package quota

// Snapshot holds the latest quota state for one provider's two windows.
type Snapshot struct {
    ProviderID string `json:"-"`

    // Per-window data. Both populated independently from the upstream "general" entry.
    Interval IntervalWindow `json:"interval"`
    Weekly   IntervalWindow `json:"weekly"`
}

type IntervalWindow struct {
    Enabled         bool      `json:"enabled"`                  // false until first successful poll
    RemainingPercent float64  `json:"remaining_percent,omitempty"`
    UsedPercent     float64   `json:"used_percent"`             // = 100 - remaining_percent
    StartTime       time.Time `json:"start_time,omitempty"`
    EndTime         time.Time `json:"end_time,omitempty"`
    ResetInSec      int       `json:"reset_in_sec,omitempty"`
    ResetInHuman    string    `json:"reset_in_human,omitempty"` // "3h 24m"

    // Source counters, surfaced for debugging / future charts.
    TotalCount  int64 `json:"total_count,omitempty"`
    UsageCount  int64 `json:"usage_count,omitempty"`
    Status      int   `json:"status,omitempty"`  // upstream status code (1=ok, 3=inactive, etc.)
}

type BlockInfo struct {
    Window          string  // "interval" or "weekly"
    UsedPercent     float64
    ResetInSec      int
    ResetInHuman    string
}
```

Public functions:

- `Init(ctx context.Context) error` — spawns the 10s ticker goroutine and registers a broadcast hook on `stats.broadcast`.
- `Shutdown()` — stops the ticker, drains in-flight poll (max 5s).
- `Snapshot() map[string]*Snapshot` — read-only view; shallow copies.
- `IsBlocked(providerID string) (bool, BlockInfo)` — see §6.
- `SetBlockEnabled(providerID string, enabled bool)` — called by `web` after PUT, updates in-memory flag without waiting for DB roundtrip.
- `RefreshNow(providerID string)` — synchronous one-shot poll, used in tests.

Internal:

- `var blockEnabled = map[string]bool{}` — providerID → toggle; loaded from DB at startup, updated by `SetBlockEnabled`, persisted via `web` PUT handler.
- `const pollInterval = 10 * time.Second`
- `const requestTimeout = 5 * time.Second`
- `const blockThreshold = 99.0` — block if `window.UsedPercent >= 99.0` (i.e. `remaining_percent <= 1`).
- `const upstreamHost = "https://api.minimaxi.com"` — pinned; no fallback.

### 2. Polling loop

```go
func (q *quotad) runLoop(ctx context.Context) {
    ticker := time.NewTicker(pollInterval)
    defer ticker.Stop()
    q.pollOnce(ctx) // initial sweep so the UI has data on first render
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            q.pollOnce(ctx)
        }
    }
}
```

`pollOnce` iterates `config.GetConfig().Providers`, filters by `IsActive && APIKey != "" && isQuotaHost(BaseURL)`, polls each concurrently (max 4 in-flight via semaphore). Panicking goroutines are recovered; the ticker survives.

### 3. Upstream request

```go
GET https://api.minimaxi.com/v1/token_plan/remains
Authorization: Bearer <provider.APIKey>
User-Agent: switchai-quota/1.0
Timeout: 5s
```

Response parser (against live capture):

1. Verify HTTP status 200.
2. Parse JSON; check `base_resp.status_code == 0` (treat non-zero as failure).
3. Find entry in `model_remains` where `model_name == "general"`. If none, set `LastError = "general 模型不在响应中"`, do not update snapshot.
4. For the `general` entry, populate both `Interval` and `Weekly` subwindows from the same entry — `interval_remaining_percent` → `Interval.RemainingPercent`, `weekly_remaining_percent` → `Weekly.RemainingPercent`. Timestamps: convert ms→time.Time. `UsedPercent = 100 - RemainingPercent`, clamped to `[0, 100]`, rounded to 2 decimals.
5. `ResetInSec` = `int(time.Until(EndTime).Seconds())`, floored at 0. `ResetInHuman` via `formatDuration`.

Failure modes:
- Network error / non-2xx / `base_resp.status_code != 0` → keep previous snapshot; set `LastError` + `LastErrorAt`. Do **not** change block state.
- 401 → set `LastError = "token 失效"`, force both windows to `UsedPercent = 100`, mark blocked (conservative).
- 200 + no `general` entry → set `LastError`, keep previous state.
- 200 + unparseable JSON → set `LastError = "无法解析响应"`, keep previous state.

### 4. WebSocket push

Quota updates ride the existing `/api/ws` channel that `stats` already broadcasts on. To avoid coupling, the quota package pushes a typed message into a sibling channel that `stats` already drains and forwards.

**Implementation**: extend the existing `stats.broadcast` channel payload type or add a parallel `quota.broadcast chan any` that `stats.WebSocketHub` (or its equivalent) drains alongside `stats.broadcast`. The frontend already handles messages keyed by `data.provider_stats`; quota messages reuse the same key but add a `quota` sub-object — keeping the wire format consistent with what the frontend already parses.

Concrete wire payload sent over `/api/ws`:

```json
{
  "provider_stats": [
    { "provider_id": "p1", "requests": 12, "...": "..." },
    { "provider_id": "p2", "...": "..." }
  ],
  "provider_quotas": {
    "p1": {
      "interval": { "used_percent": 19.0, "reset_in_human": "3h 14m", "...": "..." },
      "weekly":   { "used_percent":  0.0, "reset_in_human": "6d 23h", "...": "..." }
    }
  }
}
```

The frontend's `ws.onmessage` handler (line 1318) is extended to read `data.provider_quotas` and call `renderProviders()` (or a narrower `applyQuotaUpdate()`) when present. `loadProviders()` remains the initial-load path; subsequent updates flow through WS.

### 5. `web/web.go` changes

#### `getProviders` — populate quota fields

After existing logic, merge in quota snapshots:

```go
snaps := quota.Snapshot()
flags := quota.BlockEnabledFlags() // returns providerID → bool
for i := range providers {
    pid := providers[i].ID
    if snap, ok := snaps[pid]; ok && snap != nil {
        providers[i].QuotaInterval = snap.Interval
        providers[i].QuotaWeekly   = snap.Weekly
        providers[i].QuotaEnabled  = snap.Interval.Enabled || snap.Weekly.Enabled || snap.Interval.LastError != ""
    }
    providers[i].QuotaBlockEnabled = flags[pid] // persisted flag, NOT derived from usage
}
```

#### New endpoint — toggle block enforcement

```go
// PUT /api/providers/:id/quota-block-enabled
// body: {"enabled": true}
func setQuotaBlockEnabled(c *gin.Context) {
    id := c.Param("id")
    var body struct{ Enabled bool `json:"enabled"` }
    if err := c.BindJSON(&body); err != nil { c.JSON(400, gin.H{"error": err.Error()}); return }
    if err := config.SetProviderQuotaBlockEnabled(id, body.Enabled); err != nil {
        c.JSON(500, gin.H{"error": err.Error()}); return
    }
    quota.SetBlockEnabled(id, body.Enabled)
    c.JSON(200, gin.H{"ok": true})
}
```

Registered at line 89-97 alongside existing provider routes.

### 6. Block detection — `quota.IsBlocked`

```go
func IsBlocked(providerID string) (bool, BlockInfo) {
    snap := getSnapshot(providerID)
    if snap == nil { return false, BlockInfo{} }

    // Block toggle OFF → never block, regardless of usage.
    if !getBlockEnabled(providerID) {
        return false, BlockInfo{}
    }

    now := time.Now()
    // Check both windows; first to trip wins.
    for _, w := range []struct{
        name string
        win  IntervalWindow
    }{
        {"interval", snap.Interval},
        {"weekly",   snap.Weekly},
    } {
        if !w.win.Enabled { continue }
        // Auto-unblock if the window has already rolled.
        if !w.win.EndTime.IsZero() && now.After(w.win.EndTime) { continue }
        if w.win.UsedPercent >= blockThreshold {
            return true, BlockInfo{
                Window:       w.name,
                UsedPercent:  w.win.UsedPercent,
                ResetInSec:   w.win.ResetInSec,
                ResetInHuman: w.win.ResetInHuman,
            }
        }
    }
    return false, BlockInfo{}
}
```

### 7. Proxy insertion — `proxy/proxy.go`

Insert immediately after `resolveRouteTarget` returns (around line 270, before URL building):

```go
if blocked, info := quota.IsBlocked(provider.ID); blocked {
    windowName := "区间"
    if info.Window == "weekly" { windowName = "本周" }
    c.JSON(http.StatusForbidden, gin.H{
        "error": fmt.Sprintf("上游账户额度不足(%s窗口已用 %.1f%%)，将于 %s 后重置",
            windowName, info.UsedPercent, info.ResetInHuman),
        "window":       info.Window,
        "used_percent": info.UsedPercent,
        "reset_in_sec": info.ResetInSec,
    })
    return
}
```

`testProvider` and `fetchModelsByCredentials` are **not** gated — admin endpoints must work even when quota is exhausted.

### 8. DB schema — `config/config.go`

Add a new column via the existing migration pattern. **No change to the `Provider` Go struct's persistence path** — the column is read at startup and written by the new endpoint, but the value is stored in a separate small map in `config.Config` (`providerQuotaBlockEnabled map[string]bool`) rather than as a struct field, to keep `Provider` lean.

```go
// in config.Config
QuotaBlockEnabled map[string]bool `json:"-"` // runtime-only, loaded from DB
```

`initDB` (around line 238), alongside the existing `is_openai_format` migration:

```go
// ignore errors (column may already exist)
db.Exec(`ALTER TABLE providers ADD COLUMN quota_block_enabled INTEGER DEFAULT 0`)
```

`save()` (line 360) — extend the existing `INSERT OR REPLACE` to include `quota_block_enabled`:

```sql
INSERT OR REPLACE INTO providers (id, name, base_url, api_key, model, is_active, created_at, order_num, is_openai_format, quota_block_enabled)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
```

`SetProviderQuotaBlockEnabled(id, enabled)`:

```go
func (c *Config) SetProviderQuotaBlockEnabled(id string, enabled bool) error {
    c.mu.Lock()
    defer c.mu.Unlock()
    // update QuotaBlockEnabled map
    if c.QuotaBlockEnabled == nil { c.QuotaBlockEnabled = map[string]bool{} }
    c.QuotaBlockEnabled[id] = enabled
    // update the existing single-row write path for that provider
    _, err := c.db.Exec(`UPDATE providers SET quota_block_enabled = ? WHERE id = ?`, boolToInt(enabled), id)
    return err
}
```

At startup (in `loadConfig` or equivalent), hydrate `c.QuotaBlockEnabled[id] = row.quota_block_enabled == 1` for every provider.

### 9. Provider struct extension — `config/config.go`

Add to `Provider`:

```go
type Provider struct {
    // ... existing fields ...
    QuotaInterval      IntervalWindowJSON `json:"quota_interval,omitempty"`
    QuotaWeekly        IntervalWindowJSON `json:"quota_weekly,omitempty"`
    QuotaEnabled       bool               `json:"quota_enabled,omitempty"`
    QuotaBlockEnabled  bool               `json:"quota_block_enabled"` // always serialize (drives UI)
    QuotaError         string             `json:"quota_error,omitempty"`
}
```

`IntervalWindowJSON` is a flat DTO that mirrors the runtime `quota.IntervalWindow` minus the `Enabled` private flag — kept separate to avoid coupling the persisted struct to the runtime package. `web.getProviders` maps between them.

### 10. Frontend card — `web/static/index.html`

In `renderProviders()` (line 1424), inside `.provider-item-stats`, append after the existing `格式` div:

```js
<div class="provider-stat quota-stat" data-pid="${p.id}" style="display:${p.quota_enabled ? '' : 'none'}">
    <div class="provider-stat-value quota-value">${renderQuotaValue(p)}</div>
    <div class="provider-stat-label quota-label">额度</div>
    <label class="quota-block-toggle" data-pid="${p.id}">
        <input type="checkbox" class="quota-block-cb" ${p.quota_block_enabled ? 'checked' : ''} />
        拦截
    </label>
</div>
```

`renderQuotaValue(p)` — picks whichever window is **less remaining** (the more critical one):

```js
function renderQuotaValue(p) {
    if (p.quota_error) return `<span class="quota-err">错误</span>`;
    const iv = p.quota_interval?.used_percent ?? 0;
    const wk = p.quota_weekly?.used_percent ?? 0;
    // Use the higher used% as the headline (the more critical window).
    const used = Math.max(iv, wk);
    const reset = iv >= wk
        ? (p.quota_interval?.reset_in_human || '—')
        : (p.quota_weekly?.reset_in_human || '—');
    const window = iv >= wk ? '区间' : '本周';
    return `<span class="quota-pct quota-${colorClass(used)}">${used.toFixed(1)}%</span>
            <span class="quota-reset">${window} · ${reset}</span>`;
}
```

Color buckets (matches screenshot tone):
- `< 80` → `#22d3ee` (cyan/teal, primary brand color)
- `80–94` → `#e67e22` (orange)
- `95–98` → `#c0392b` (red-orange)
- `≥ 99` (only shown when block-enabled) → `#c0392b` with `1px solid` border + label `已拦截`

WSS integration — extend `ws.onmessage` handler (line 1318) to also call `applyQuotaUpdate(data.provider_quotas)`:

```js
function applyQuotaUpdate(quotas) {
    if (!quotas) return;
    // Mutate the local providers array in place; re-render quota stats only.
    for (const pid in quotas) {
        const p = providers.find(x => x.id === pid);
        if (!p) continue;
        p.quota_interval = quotas[pid].interval;
        p.quota_weekly   = quotas[pid].weekly;
        applyQuotaToCard(p); // narrow DOM update, NOT a full renderProviders()
    }
}
```

Toggle handler — POST to the new endpoint when the checkbox changes:

```js
$(document).on('change', '.quota-block-cb', function() {
    const pid = this.closest('.quota-block-toggle').dataset.pid;
    const enabled = this.checked;
    fetch(`/api/providers/${pid}/quota-block-enabled`, {
        method: 'PUT',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({enabled}),
    }).catch(err => toast('切换失败: ' + err.message));
});
```

`renderProviders()` continues to fire on initial load + on provider CRUD (config WS event) — the toggle's checked state survives re-renders because it's bound to `p.quota_block_enabled` returned from the server.

## Data flow

```
T+0s   ticker fires
       │
       ▼
config.GetConfig().Providers
       │ filter IsActive && APIKey != "" && isQuotaHost(BaseURL)
       ▼
for each provider (concurrent, max 4)
       │
       ▼
GET https://api.minimaxi.com/v1/token_plan/remains
Authorization: Bearer <provider.APIKey>
       │
       ▼
parse JSON → find model_name=="general" entry
       │
       ▼
populate Snapshot.Interval + Snapshot.Weekly
       │
       ▼
state[providerID] = &Snapshot{...}
       │
       ├──► push to stats.broadcast ──► /api/ws ──► frontend ws.onmessage
       │                                    │
       │                                    ▼
       │                              applyQuotaUpdate(quotas)
       │                                    │
       │                                    ▼
       │                              narrow DOM update on .quota-stat[data-pid]
       │
       └──► on next /v1/chat request → quota.IsBlocked(providerID)
                                          │
                                          ▼
                                    [toggle ON] AND [any window UsedPercent ≥ 99]?
                                          │
                                       yes ──► 403 {error, window, used_percent, reset_in_sec}
```

## Error handling

| Situation | Behavior |
|---|---|
| Network error / non-2xx / `base_resp.status_code != 0` | Keep previous snapshot; set `LastError` + `LastErrorAt`; do **not** change block state |
| 200 + no `model_name == "general"` entry | Set `LastError = "general 模型不在响应中"`; keep previous snapshot |
| 401 (token invalid) | Set `LastError = "token 失效"`; force both windows to `UsedPercent = 100`; do **not** auto-block (still gated by toggle) |
| 200 + unparseable JSON | Set `LastError = "无法解析响应"`; keep previous state |
| Window's `end_time` in the past | Auto-unblock on next `IsBlocked` call (lazy) |
| Poll goroutine panic | `defer recover()`; log error; ticker continues |
| Provider deleted while poll in-flight | Next iteration drops it from the filter set; `state[id]` is also pruned by `pollOnce` before iterating |
| Toggle ON but snapshot never populated (e.g. startup race) | `IsBlocked` returns `false` — there is no window to trip |

## Testing

`quota/quota_test.go`:

- `TestIsQuotaHost` — `api.minimaxi.com`, `https://api.minimaxi.com/v1`, `www.minimaxi.com`, `evil.com`, empty, `MiniMax.com`, `notminimaxi.com`.
- `TestParseResponse_PicksGeneral` — multi-model response, only `general` entry's windows are stored; `video` ignored.
- `TestParseResponse_NoGeneral` — snapshot stays empty, `LastError = "general 模型不在响应中"`.
- `TestComputeUsedPct` — `remaining 81 → used 19.0`, `remaining 1 → used 99.0`, `remaining 0 → used 100.0`, `remaining 100 → used 0.0`, clamp.
- `TestIsBlocked_ToggleRespected` — used ≥ 99 but toggle OFF → not blocked.
- `TestIsBlocked_ToggleOn_TripsInterval` — toggle ON, interval used 99.5, weekly 50 → blocked, info.Window = "interval".
- `TestIsBlocked_ToggleOn_TripsWeekly` — toggle ON, interval 50, weekly 99.5 → blocked, info.Window = "weekly".
- `TestIsBlocked_BothTrip_IntervalWins` — both at 99.5 → blocked, info.Window = "interval" (first checked wins).
- `TestIsBlocked_AutoUnblockOnEndTime` — pre-seeded window with past `EndTime` returns `false`.
- `TestSnapshot_StaleError` — pre-seeded success + failed poll → old values preserved, `LastError` populated.
- `TestPollOnce_401ForcesFull` — 401 → both windows `UsedPercent = 100`.
- `TestFormatDuration` — `3h 14m`, `45m`, `12s`, `0s`.

`proxy/proxy_test.go`:

- New case `TestProxy_QuotaBlocked` — pre-seed quota snapshot with toggle ON + interval used 99.5; expect `403` with `{error, window: "interval", used_percent: 99.5, reset_in_sec: …}`.
- New case `TestProxy_QuotaBlockedToggleOff` — same snapshot but toggle OFF; expect request proceeds (200 or whatever upstream returns).

`config/config_test.go`:

- `TestSetProviderQuotaBlockEnabled_Persists` — toggle, reload from DB, verify value persists.
- `TestLoadConfig_QuotaBlockEnabledHydrated` — pre-seed DB row with `quota_block_enabled=1`, load, verify map populated.

## Out of scope (follow-up)

- Historical quota chart / 24h trend (only `MiniMax_TokenPlan_UsageReport.png` referenced in README).
- Multi-region quota handling.
- Manual unlock button in admin UI.
- Other providers (Anthropic usage API, OpenAI billing API).
- Per-window toggle (currently one toggle per provider covers both windows).