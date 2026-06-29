# 近 7 日趋势（含今天）接入密钥统计面板

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在密钥行"统计"面板里增加 `7d` 桶，展示**包含今天**的近 7 天每日消耗趋势，复用现有按钮、路由与缓存。

**Architecture:** 后端 `GetKeyTodayBuckets` 增加 `bucket="7d"` 分支，从 `key_daily_stats` 表 PK 查询近 7 天；前端面板新增第三个按钮、动态标题、`buildKeyChartOption` 增加 MM-DD 横轴与 tooltip 分支。

**Tech Stack:** Go (database/sql + SQLite) · 原生 JS + ECharts

**Spec:** [docs/superpowers/specs/2026-06-29-7d-trend-in-key-stats-design.md](../specs/2026-06-29-7d-trend-in-key-stats-design.md)

---

## File Structure

| 文件 | 责任 |
|------|------|
| `stats/stats.go` | 修改 `GetKeyTodayBuckets` 增加 `7d` 分支（不影响 5h/hour） |
| `stats/stats_test.go` | 新增 `TestGetKeyTodayBuckets_7d` |
| `web/static/index.html` | 三处改动：桶按钮、标题动态化、`buildKeyChartOption` 桶分支 |

---

## Task 1: 后端 — 增加 `7d` 桶分支

**Files:**
- Modify: `stats/stats.go:185-263` (`GetKeyTodayBuckets`)
- Test: `stats/stats_test.go` 新增 `TestGetKeyTodayBuckets_7d`

- [ ] **Step 1: 阅读现有 `stats/stats_test.go` 的 `TestGetKeyTodayBuckets_OneDayAbove1000`（213~260 行）理解测试 DB fixture 模式**

- [ ] **Step 2: 写失败的测试 `TestGetKeyTodayBuckets_7d`**

把下列测试追加到 `stats/stats_test.go`（紧贴现有 `TestGetKeyTodayBuckets_OneDayAbove1000` 之后）：

```go
// TestGetKeyTodayBuckets_7d 验证 7d 桶：返回 7 桶、含今天、按日期升序、缺失日期补 0。
func TestGetKeyTodayBuckets_7d(t *testing.T) {
    setupTestDB(t)
    defer teardownTestDB()

    keyID := "test-key-7d"

    now := time.Now()
    todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
    todayDate := todayStart.Format("2006-01-02")

    // 写入 7 天数据，但缺失 today-3（验证缺失桶补 0）
    rows := []struct {
        date     string
        input    int
        output   int
        requests int
        cost     float64
    }{
        {todayStart.AddDate(0, 0, -6).Format("2006-01-02"), 100, 50, 1, 0.10},
        {todayStart.AddDate(0, 0, -5).Format("2006-01-02"), 200, 80, 2, 0.20},
        {todayStart.AddDate(0, 0, -4).Format("2006-01-02"), 300, 90, 3, 0.30},
        // today-3 缺失
        {todayStart.AddDate(0, 0, -2).Format("2006-01-02"), 400, 100, 4, 0.40},
        {todayStart.AddDate(0, 0, -1).Format("2006-01-02"), 500, 110, 5, 0.50},
        {todayDate, 600, 120, 6, 0.60},
    }
    for _, r := range rows {
        _, err := db.Exec(`INSERT INTO key_daily_stats
            (key_id, date, input_tokens, output_tokens, request_count, total_cost)
            VALUES (?, ?, ?, ?, ?, ?)`,
            keyID, r.date, r.input, r.output, r.requests, r.cost)
        if err != nil {
            t.Fatalf("insert failed for %s: %v", r.date, err)
        }
    }

    stats, err := GetKeyTodayBuckets(keyID, "7d")
    if err != nil {
        t.Fatalf("GetKeyTodayBuckets(7d) returned error: %v", err)
    }

    if stats.Bucket != "7d" {
        t.Fatalf("expected bucket=7d, got %q", stats.Bucket)
    }
    if len(stats.Buckets) != 7 {
        t.Fatalf("expected 7 buckets, got %d", len(stats.Buckets))
    }

    // 验证升序：i=0 → today-6, i=6 → today
    for i, b := range stats.Buckets {
        expectedDate := todayStart.AddDate(0, 0, -(6 - i)).Format("2006-01-02")
        if got := b.T.Format("2006-01-02"); got != expectedDate {
            t.Errorf("bucket[%d].T = %s, want %s", i, got, expectedDate)
        }
    }

    // today-3 缺失桶应为 0（i=3 → today-3）
    missing := stats.Buckets[3]
    if missing.InputTokens != 0 || missing.OutputTokens != 0 ||
        missing.RequestCount != 0 || missing.Cost != 0 {
        t.Errorf("expected zero bucket at i=3, got %+v", missing)
    }

    // 今天桶（i=6）应能正确读出累计数据
    today := stats.Buckets[6]
    if today.InputTokens != 600 || today.OutputTokens != 120 ||
        today.RequestCount != 6 || today.Cost != 0.60 {
        t.Errorf("today bucket mismatch: %+v", today)
    }

    // Date 字段应为今天
    if stats.Date != todayDate {
        t.Errorf("stats.Date = %s, want %s", stats.Date, todayDate)
    }
}
```

- [ ] **Step 3: 运行测试确认失败**

```bash
go test ./stats/ -run TestGetKeyTodayBuckets_7d -v
```

Expected: FAIL（后端尚未支持 `"7d"`，函数 fallback 到 `"5h"`，导致测试失败）

- [ ] **Step 4: 实现 `7d` 分支**

在 `stats/stats.go` 的 `GetKeyTodayBuckets` 中，把 `if bucket != "5h" && bucket != "hour"` 改为 `if bucket != "5h" && bucket != "hour" && bucket != "7d"`，然后在 `totalBuckets := 24` 之前、`if bucket == "5h"` 之后追加新分支：

```go
if bucket == "7d" {
    buckets := make([]TimeBucket, 7)
    for i := 0; i < 7; i++ {
        d := todayStart.AddDate(0, 0, -(6 - i)) // i=0 → today-6, i=6 → today
        var b TimeBucket
        err := db.QueryRow(`
            SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
                   COALESCE(SUM(request_count), 0), COALESCE(SUM(total_cost), 0.0)
            FROM key_daily_stats
            WHERE date = ? AND key_id = ?`,
            d.Format("2006-01-02"), keyID).
            Scan(&b.InputTokens, &b.OutputTokens, &b.RequestCount, &b.Cost)
        if err != nil {
            return nil, err
        }
        b.T = d
        buckets[i] = b
    }
    return &TodayStats{
        KeyID:   keyID,
        Date:    todayStart.Format("2006-01-02"),
        Bucket:  "7d",
        Buckets: buckets,
    }, nil
}
```

注意：放在现有 5h/hour 分支**之前**还是**之后**？— 放在 `if bucket == "5h"` 之后、`totalBuckets := 24` 之前最安全（早 return 不污染后面逻辑）。最终插入位置：现有 `buckets := make([]TimeBucket, totalBuckets)` 之前（即函数结尾 return 之前）。

- [ ] **Step 5: 重新跑测试**

```bash
go test ./stats/ -run TestGetKeyTodayBuckets_7d -v
```

Expected: PASS

- [ ] **Step 6: 跑全部 stats 测试，确认未破坏 5h/hour 行为**

```bash
go test ./stats/ -v
```

Expected: ALL PASS

- [ ] **Step 7: 提交**

```bash
git add stats/stats.go stats/stats_test.go
git commit -m "feat(stats): add 7d bucket to GetKeyTodayBuckets"
```

---

## Task 2: 前端 — 桶切换按钮增加 `7d`

**Files:**
- Modify: `web/static/index.html:1843-1852` (面板 innerHTML 模板)

- [ ] **Step 1: 修改面板 innerHTML 模板**

定位到 `toggleKeyChart` 函数内：

```js
panel.innerHTML = `
    <div class="chart-header">
        <span class="chart-title">今日消耗趋势</span>
        <div class="bucket-toggle">
            <button class="active" data-bucket="5h">5h</button>
            <button data-bucket="hour">1h</button>
        </div>
    </div>
    <div class="chart" style="height: 280px;"></div>
`;
```

把第三个按钮加上：

```js
panel.innerHTML = `
    <div class="chart-header">
        <span class="chart-title">今日消耗趋势</span>
        <div class="bucket-toggle">
            <button class="active" data-bucket="5h">5h</button>
            <button data-bucket="hour">1h</button>
            <button data-bucket="7d">7d</button>
        </div>
    </div>
    <div class="chart" style="height: 280px;"></div>
`;
```

- [ ] **Step 2: 提交**

```bash
git add web/static/index.html
git commit -m "feat(web): add 7d button to key stats panel"
```

---

## Task 3: 前端 — `loadKeyChart` 动态切换标题

**Files:**
- Modify: `web/static/index.html:1874-1902` (`loadKeyChart`)

- [ ] **Step 1: 在两处 `state.chart.setOption(...)` 之前切换标题**

定位 `loadKeyChart` 函数。把整段替换为：

```js
async function loadKeyChart(keyId, bucket, state) {
    // 5 分钟内同 bucket 复用缓存；缓存必须与「今天」绑定，否则 23:58 缓存会被
    // 00:03 复用，但 GetKeyTodayBuckets 只返回「今天」的数据（旧缓存变成昨天的）。
    const cached = keyStatsCache.get(keyId)?.get(bucket);
    const todayKey = new Date().toISOString().slice(0, 10);
    if (cached && cached.day === todayKey && Date.now() - cached.ts < 5 * 60 * 1000) {
        state.panel.querySelector('.chart-title').textContent =
            bucket === '7d' ? '近 7 日趋势' : '今日消耗趋势';
        state.chart.setOption(buildKeyChartOption(cached.data), true);
        return;
    }

    const mySeq = ++state.reqSeq;
    try {
        const res = await fetch(`/api/server-keys/${keyId}/today-stats?bucket=${bucket}`);
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const data = await res.json();
        if (mySeq !== state.reqSeq) return;

        if (!keyStatsCache.has(keyId)) keyStatsCache.set(keyId, new Map());
        keyStatsCache.get(keyId).set(bucket, { day: todayKey, data, ts: Date.now() });

        state.panel.querySelector('.chart-title').textContent =
            bucket === '7d' ? '近 7 日趋势' : '今日消耗趋势';
        state.chart.setOption(buildKeyChartOption(data), true);
    } catch (err) {
        if (mySeq !== state.reqSeq) return;
        state.chart.setOption({
            title: { text: `加载失败: ${err.message}`, left: 'center', top: 'center',
                     textStyle: { color: '#e53e3e', fontSize: 14 } }
        }, true);
    }
}
```

- [ ] **Step 2: 提交**

```bash
git add web/static/index.html
git commit -m "feat(web): dynamic chart title based on bucket"
```

---

## Task 4: 前端 — `buildKeyChartOption` 支持 `7d` 桶

**Files:**
- Modify: `web/static/index.html:1904-1973` (`buildKeyChartOption`)

- [ ] **Step 1: 修改 labels 计算**

定位 `function buildKeyChartOption(data)`：

```js
const labels = data.bucket === '5h'
    ? ['00–05', '05–10', '10–15', '15–20', '20–24']
    : Array.from({ length: 24 }, (_, i) => String(i).padStart(2, '0'));
const buckets = data.buckets;
const isHour = data.bucket === 'hour';
```

替换为：

```js
const labels = data.bucket === '5h'
    ? ['00–05', '05–10', '10–15', '15–20', '20–24']
    : data.bucket === '7d'
        ? data.buckets.map(b => {
            const t = new Date(b.t);
            const m = String(t.getMonth() + 1).padStart(2, '0');
            const d = String(t.getDate()).padStart(2, '0');
            return `${m}-${d}`;
        })
        : Array.from({ length: 24 }, (_, i) => String(i).padStart(2, '0'));
const buckets = data.buckets;
const isHour = data.bucket === 'hour';
const is7d = data.bucket === '7d';
```

- [ ] **Step 2: 修改 tooltip formatter**

把：

```js
formatter: params => {
    const i = params[0].dataIndex;
    const b = buckets[i];
    const tStart = new Date(b.t);
    const tEnd = isHour
        ? new Date(tStart.getTime() + 3600 * 1000)
        : (i === buckets.length - 1 ? new Date() : new Date(tStart.getTime() + 18000 * 1000));
    return `<b>${tStart.toLocaleString()} - ${tEnd.toLocaleTimeString([], {hour:'2-digit', minute:'2-digit'})}</b><br/>
        输入 token: ${b.input_tokens.toLocaleString()}<br/>
        输出 token: ${b.output_tokens.toLocaleString()}<br/>
        请求数: ${b.request_count}<br/>
        花费: $${b.cost.toFixed(4)}`;
}
```

替换为：

```js
formatter: params => {
    const i = params[0].dataIndex;
    const b = buckets[i];
    const tStart = new Date(b.t);
    const tEnd = isHour
        ? new Date(tStart.getTime() + 3600 * 1000)
        : is7d
            ? (i === buckets.length - 1
                ? new Date()
                : new Date(tStart.getTime() + 86400 * 1000))
            : (i === buckets.length - 1 ? new Date() : new Date(tStart.getTime() + 18000 * 1000));
    const tEndLabel = is7d
        ? (i === buckets.length - 1
            ? '当前'
            : tEnd.toLocaleDateString())
        : tEnd.toLocaleTimeString([], {hour:'2-digit', minute:'2-digit'});
    return `<b>${tStart.toLocaleString()} - ${tEndLabel}</b><br/>
        输入 token: ${b.input_tokens.toLocaleString()}<br/>
        输出 token: ${b.output_tokens.toLocaleString()}<br/>
        请求数: ${b.request_count}<br/>
        花费: $${b.cost.toFixed(4)}`;
}
```

- [ ] **Step 3: 手工验证**

1. 启动服务：`./switchai`（或 `go run .`）。
2. 浏览器打开页面，登录。
3. 在任一 API 服务器密钥行点"统计"按钮，确认面板默认加载 5h 桶（行为不变）。
4. 切换到 `1h` 桶，确认行为不变。
5. 切换到 `7d` 桶：
   - 标题变为"近 7 日趋势"。
   - 横轴显示 7 个 `MM-DD`（如 `06-23`）。
   - 7 个柱/线显示近 7 天（含今天）数据。
   - 鼠标 hover tooltip 显示日期区间。
6. 反复切换三个桶，确认无残留 state。

- [ ] **Step 4: 提交**

```bash
git add web/static/index.html
git commit -m "feat(web): render 7d bucket with MM-DD labels"
```

---

## Task 5: 端到端验证

- [ ] **Step 1: 全量构建**

```bash
go build ./...
```

Expected: 无错误

- [ ] **Step 2: 全量测试**

```bash
go test ./...
```

Expected: ALL PASS

- [ ] **Step 3: 静态 lint**

```bash
go vet ./...
```

Expected: 无 warning

---

## Self-Review

**1. Spec coverage:**
- §3.1 后端 7d 分支 → Task 1 ✓
- §3.2 前端按钮 → Task 2 ✓
- §3.3 动态标题 → Task 3 ✓
- §3.4 labels + tooltip → Task 4 ✓
- §6 测试 → Task 1 Step 2 ✓
- §5 错误处理 → 复用现有 catch（Task 3）✓

**2. Placeholder scan:** 无 TBD/TODO；所有代码片段完整。

**3. Type consistency:**
- `TimeBucket.T` 字段是 `time.Time`（stats.go:60），前端 `b.t` 是 ISO string。
- `TodayStats.Date` 是 string（stats.go:69），Task 1 中 `todayStart.Format("2006-01-02")` 与现有 `Date` 字段用法一致。
- `Bucket` 字段在 5h/hour 分支也填了 `"5h"`/`"hour"`，Task 1 中填 `"7d"` 与之对齐。
- 前端 `data.bucket === '7d'` 字符串与后端 `bucket == "7d"` 一致。