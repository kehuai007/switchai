# API 密钥今日统计折线图 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 admin dashboard 每个 ServerKey 卡片上加 "今日统计" 按钮，点击展开 ECharts 折线图（默认 5h 桶 5 段，可切换 1h 桶 24 段），展示今日 tokens / 请求数 / 花费的时段分布与具体数值。

**Architecture:** 后端实时 SQL GROUP BY `usage_records`（不新建表），返回今日按桶切分的时间序列；前端按需加载 + 5 分钟内存缓存 + `reqSeq` 序号防竞态。ECharts 本地 vendor 拷贝。

**Tech Stack:** Go 1.25 + Gin + SQLite (modernc.org/sqlite) + ECharts 5 (canvas renderer) + 原生 HTML/JS

**Spec:** `docs/superpowers/specs/2026-06-29-key-today-stats-chart-design.md`

---

## 文件结构

修改：
- `stats/stats.go` — 新增 `TimeBucket`/`TodayStats` 类型（紧贴 `KeyStats` 后）；新增 `GetKeyTodayBuckets(keyID, bucket string) (*TodayStats, error)`
- `stats/stats_test.go` — 新增 6 个测试函数（基础 5h、基础 hour、空桶、非法 bucket、5h 边界、末段 4h）
- `web/web.go:81` 后 — 注册 `api.GET("/server-keys/:id/today-stats", getKeyTodayStats)` + 加 handler 函数
- `web/static/index.html` — 加 `<script src="static/vendor/echarts.min.js">`；`renderServerKeys()` actions 区域加 "今日统计" 按钮；末尾加 CSS/JS（`keyCharts`/`keyStatsCache` Map、`toggleKeyChart`/`loadKeyChart`/`buildKeyChartOption`）

新建：
- `web/static/vendor/echarts.min.js` — 从 `C:\Users\Admin\go\src\minimax-monitor\internal\server\web\vendor\echarts.min.js` 拷贝

无 DB schema 变更。

---

## 任务列表

- [ ] Task 1: 后端 — 写 `GetKeyTodayBuckets` 失败测试
- [ ] Task 2: 后端 — 实现 `TimeBucket` / `TodayStats` 类型 + `GetKeyTodayBuckets` 函数（最小版）
- [ ] Task 3: 后端 — 添加空桶 / 非法 bucket / 边界 / 末段测试
- [ ] Task 4: 后端 — 在 `web/web.go` 注册路由 + handler + smoke test
- [ ] Task 5: 前端 — 拷贝 ECharts 到 vendor + 加 script 标签
- [ ] Task 6: 前端 — 加 "今日统计" 按钮 + 折叠面板（无图表）+ CSS
- [ ] Task 7: 前端 — ECharts 集成 + `buildKeyChartOption` + 渲染
- [ ] Task 8: 前端 — bucket toggle + 5 分钟缓存 + `reqSeq` 竞态保护
- [ ] Task 9: 端到端验证（编译 / 测试 / 启动 / 浏览器走查）

---

## Task 1: 后端 — 写 `GetKeyTodayBuckets` 失败测试

**Files:**
- Modify: `stats/stats_test.go`（追加到文件末尾）

- [ ] **Step 1: 在 `stats/stats_test.go` 末尾追加失败测试**

```go
// TestGetKeyTodayBuckets_5h 守护：bucket=5h 应把今日 00:00-04:59 归为桶 1，
// 05:00-09:59 归为桶 2，依此类推。末段 20:00-23:59 是 4h。
func TestGetKeyTodayBuckets_5h(t *testing.T) {
	defer setupTestDB(t)()
	t.Cleanup(withTestStats(t))

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	// 桶 1：01:00 → input=10, output=5, cost=0.10
	// 桶 2：06:00 → input=20, output=8, cost=0.20
	// 桶 3：12:00 → input=30, output=12, cost=0.30
	// 桶 5：21:00 → input=40, output=15, cost=0.40（4h 段）
	stamp := func(h, m int) int64 {
		return todayStart.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute).Unix()
	}
	insertUsage := func(key string, ts int64, inTok, outTok int, cost float64) {
		_, err := db.Exec(`INSERT INTO usage_records
			(provider_id, model, input_tokens, output_tokens, total_tokens, cost, timestamp, key_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			"p", "m", inTok, outTok, inTok+outTok, cost, ts, key)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	insertUsage("k1", stamp(1, 0), 10, 5, 0.10)
	insertUsage("k1", stamp(6, 0), 20, 8, 0.20)
	insertUsage("k1", stamp(12, 0), 30, 12, 0.30)
	insertUsage("k1", stamp(21, 0), 40, 15, 0.40)

	got, err := GetKeyTodayBuckets("k1", "5h")
	if err != nil {
		t.Fatalf("GetKeyTodayBuckets: %v", err)
	}
	if got.Bucket != "5h" {
		t.Errorf("Bucket = %q, want 5h", got.Bucket)
	}
	if len(got.Buckets) != 5 {
		t.Fatalf("Buckets length = %d, want 5", len(got.Buckets))
	}
	// 桶 1：01:00 落入 00:00 桶
	if got.Buckets[0].InputTokens != 10 || got.Buckets[0].RequestCount != 1 {
		t.Errorf("桶1 input/count = %d/%d, want 10/1", got.Buckets[0].InputTokens, got.Buckets[0].RequestCount)
	}
	// 桶 2：06:00 落入 05:00 桶
	if got.Buckets[1].InputTokens != 20 {
		t.Errorf("桶2 input = %d, want 20", got.Buckets[1].InputTokens)
	}
	// 桶 3：12:00 落入 10:00 桶
	if got.Buckets[2].InputTokens != 30 {
		t.Errorf("桶3 input = %d, want 30", got.Buckets[2].InputTokens)
	}
	// 桶 4：15:00-19:59 → 应为空
	if got.Buckets[3].RequestCount != 0 {
		t.Errorf("桶4 request_count = %d, want 0", got.Buckets[3].RequestCount)
	}
	// 桶 5：21:00 落入 20:00 桶
	if got.Buckets[4].InputTokens != 40 {
		t.Errorf("桶5 input = %d, want 40", got.Buckets[4].InputTokens)
	}
}

// TestGetKeyTodayBuckets_Hour 守护：bucket=hour 应返回 24 个整点桶。
func TestGetKeyTodayBuckets_Hour(t *testing.T) {
	defer setupTestDB(t)()
	t.Cleanup(withTestStats(t))

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	stamp := func(h int) int64 {
		return todayStart.Add(time.Duration(h) * time.Hour).Add(5 * time.Minute).Unix() // hh:05 落入 hh 桶
	}
	insertUsage := func(key string, ts int64, inTok int) {
		_, err := db.Exec(`INSERT INTO usage_records
			(provider_id, model, input_tokens, output_tokens, total_tokens, cost, timestamp, key_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			"p", "m", inTok, 0, inTok, 0.0, ts, key)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	insertUsage("k1", stamp(3), 100)
	insertUsage("k1", stamp(15), 200)
	insertUsage("k1", stamp(23), 300)

	got, err := GetKeyTodayBuckets("k1", "hour")
	if err != nil {
		t.Fatalf("GetKeyTodayBuckets: %v", err)
	}
	if got.Bucket != "hour" {
		t.Errorf("Bucket = %q, want hour", got.Bucket)
	}
	if len(got.Buckets) != 24 {
		t.Fatalf("Buckets length = %d, want 24", len(got.Buckets))
	}
	if got.Buckets[3].InputTokens != 100 {
		t.Errorf("桶 03:00 input = %d, want 100", got.Buckets[3].InputTokens)
	}
	if got.Buckets[15].InputTokens != 200 {
		t.Errorf("桶 15:00 input = %d, want 200", got.Buckets[15].InputTokens)
	}
	if got.Buckets[23].InputTokens != 300 {
		t.Errorf("桶 23:00 input = %d, want 300", got.Buckets[23].InputTokens)
	}
	// 其他桶为空
	if got.Buckets[0].RequestCount != 0 || got.Buckets[10].RequestCount != 0 {
		t.Errorf("空桶不为 0: 桶0=%d, 桶10=%d", got.Buckets[0].RequestCount, got.Buckets[10].RequestCount)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./stats/... -run 'TestGetKeyTodayBuckets_(5h|Hour)' -v`
Expected: FAIL — `GetKeyTodayBuckets` 函数未定义（编译错误）

- [ ] **Step 3: commit（仅测试文件）**

```bash
git add stats/stats_test.go
git commit -m "test(stats): add GetKeyTodayBuckets 5h/hour bucket tests"
```

---

## Task 2: 后端 — 实现类型 + `GetKeyTodayBuckets`

**Files:**
- Modify: `stats/stats.go:57` 后（紧贴 `KeyStats` struct 定义后）

- [ ] **Step 1: 在 `stats/stats.go` `KeyStats` struct 定义后追加新类型**

找到 `KeyStats` struct 结束的位置（约 [stats/stats.go:57](stats/stats.go#L57)），在其后插入：

```go
type TimeBucket struct {
	T            time.Time `json:"t"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	RequestCount int       `json:"request_count"`
	Cost         float64   `json:"cost"`
}

type TodayStats struct {
	KeyID   string       `json:"key_id"`
	Date    string       `json:"date"`
	Bucket  string       `json:"bucket"`
	Buckets []TimeBucket `json:"buckets"`
}
```

- [ ] **Step 2: 在 `stats/stats.go` 文件末尾（`Shutdown` 之前）追加函数**

找到 `func Shutdown()` 位置，在其前插入：

```go
// GetKeyTodayBuckets 返回指定 key 今日按桶切分的时间序列。
// bucket 接受 "5h"（默认，一天 5 段）或 "hour"（24 段）；其他值 fallback 到 "5h"。
// 桶大小：5h=18000 秒，hour=3600 秒。
// 返回的 buckets 数组已补齐所有空桶（即使该桶 0 请求）。
func GetKeyTodayBuckets(keyID, bucket string) (*TodayStats, error) {
	if bucket != "5h" && bucket != "hour" {
		bucket = "5h"
	}

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	todayStartUnix := todayStart.Unix()

	div := int64(3600)
	if bucket == "5h" {
		div = 18000
	}

	query := `
		SELECT (timestamp / ?) * ? AS t,
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cost), 0),
		       COUNT(*)
		FROM usage_records
		WHERE key_id = ? AND timestamp >= ?
		GROUP BY t
		ORDER BY t
	`
	rows, err := db.Query(query, div, div, keyID, todayStartUnix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	raw := map[int64]TimeBucket{}
	for rows.Next() {
		var t int64
		var b TimeBucket
		if err := rows.Scan(&t, &b.InputTokens, &b.OutputTokens, &b.Cost, &b.RequestCount); err != nil {
			return nil, err
		}
		b.T = time.Unix(t, 0).In(now.Location())
		raw[t] = b
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	totalBuckets := 24
	if bucket == "5h" {
		totalBuckets = 5
	}
	buckets := make([]TimeBucket, totalBuckets)
	for i := 0; i < totalBuckets; i++ {
		t := todayStartUnix + int64(i)*div
		if b, ok := raw[t]; ok {
			buckets[i] = b
		} else {
			buckets[i] = TimeBucket{T: time.Unix(t, 0).In(now.Location())}
		}
	}

	return &TodayStats{
		KeyID:   keyID,
		Date:    todayStart.Format("2006-01-02"),
		Bucket:  bucket,
		Buckets: buckets,
	}, nil
}
```

- [ ] **Step 3: 跑 Task 1 的两个测试确认通过**

Run: `go test ./stats/... -run 'TestGetKeyTodayBuckets_(5h|Hour)' -v`
Expected: PASS（两个测试都通过）

- [ ] **Step 4: 跑全量 stats 测试确认无回归**

Run: `go test ./stats/... -v`
Expected: 全部 PASS，没有 fail

- [ ] **Step 5: commit**

```bash
git add stats/stats.go
git commit -m "feat(stats): add GetKeyTodayBuckets — 5h/hour 时间桶聚合"
```

---

## Task 3: 后端 — 添加空桶 / 非法 bucket / 边界 / 末段测试

**Files:**
- Modify: `stats/stats_test.go`（追加）

- [ ] **Step 1: 在 `stats/stats_test.go` 末尾追加边界测试**

```go
// TestGetKeyTodayBuckets_Empty 守护：key 今日无请求时，buckets 数组仍按完整长度返回（全 0）。
func TestGetKeyTodayBuckets_Empty(t *testing.T) {
	defer setupTestDB(t)()
	t.Cleanup(withTestStats(t))

	got, err := GetKeyTodayBuckets("nonexistent", "5h")
	if err != nil {
		t.Fatalf("GetKeyTodayBuckets: %v", err)
	}
	if len(got.Buckets) != 5 {
		t.Fatalf("5h buckets length = %d, want 5", len(got.Buckets))
	}
	for i, b := range got.Buckets {
		if b.RequestCount != 0 || b.InputTokens != 0 || b.OutputTokens != 0 || b.Cost != 0 {
			t.Errorf("桶 %d 不为空: %+v", i, b)
		}
	}

	gotHour, err := GetKeyTodayBuckets("nonexistent", "hour")
	if err != nil {
		t.Fatalf("GetKeyTodayBuckets hour: %v", err)
	}
	if len(gotHour.Buckets) != 24 {
		t.Errorf("hour buckets length = %d, want 24", len(gotHour.Buckets))
	}
}

// TestGetKeyTodayBuckets_InvalidBucket 守护：非法 bucket 值应 fallback 到 5h。
func TestGetKeyTodayBuckets_InvalidBucket(t *testing.T) {
	defer setupTestDB(t)()
	t.Cleanup(withTestStats(t))

	got, err := GetKeyTodayBuckets("k1", "day")
	if err != nil {
		t.Fatalf("GetKeyTodayBuckets: %v", err)
	}
	if got.Bucket != "5h" {
		t.Errorf("Bucket = %q, want 5h (fallback)", got.Bucket)
	}
	if len(got.Buckets) != 5 {
		t.Errorf("Buckets length = %d, want 5", len(got.Buckets))
	}
}

// TestGetKeyTodayBuckets_Boundary 守护：跨 5h/小时边界的请求必须落到正确桶。
// 5h 边界：04:59:59 落入桶 1，05:00:00 落入桶 2。
// 小时边界：00:59:59 落入桶 0，01:00:00 落入桶 1。
func TestGetKeyTodayBuckets_Boundary(t *testing.T) {
	defer setupTestDB(t)()
	t.Cleanup(withTestStats(t))

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	stamp := func(h, m, s int) int64 {
		return todayStart.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second).Unix()
	}
	insertUsage := func(key string, ts int64, tag string) {
		_, err := db.Exec(`INSERT INTO usage_records
			(provider_id, model, input_tokens, output_tokens, total_tokens, cost, timestamp, key_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			"p", "m", 1, 0, 1, 0.0, ts, key)
		if err != nil {
			t.Fatalf("insert %s: %v", tag, err)
		}
	}
	insertUsage("k1", stamp(4, 59, 59), "4:59:59")   // 桶 1
	insertUsage("k1", stamp(5, 0, 0), "5:00:00")     // 桶 2
	insertUsage("k1", stamp(0, 59, 59), "0:59:59")   // hour 桶 0
	insertUsage("k1", stamp(1, 0, 0), "1:00:00")     // hour 桶 1

	// 5h
	got5h, _ := GetKeyTodayBuckets("k1", "5h")
	if got5h.Buckets[0].RequestCount != 1 {
		t.Errorf("5h 桶 1 (04:59:59) request_count = %d, want 1", got5h.Buckets[0].RequestCount)
	}
	if got5h.Buckets[1].RequestCount != 1 {
		t.Errorf("5h 桶 2 (05:00:00) request_count = %d, want 1", got5h.Buckets[1].RequestCount)
	}

	// hour
	gotHour, _ := GetKeyTodayBuckets("k1", "hour")
	if gotHour.Buckets[0].RequestCount != 1 {
		t.Errorf("hour 桶 0 (00:59:59) request_count = %d, want 1", gotHour.Buckets[0].RequestCount)
	}
	if gotHour.Buckets[1].RequestCount != 1 {
		t.Errorf("hour 桶 1 (01:00:00) request_count = %d, want 1", gotHour.Buckets[1].RequestCount)
	}
}

// TestGetKeyTodayBuckets_LastBucket 守护：末段 20:00-23:59 是 4h（不补齐为 5h）。
// t 字段是 20:00:00，RequestCount 等数据按实际落入。
func TestGetKeyTodayBuckets_LastBucket(t *testing.T) {
	defer setupTestDB(t)()
	t.Cleanup(withTestStats(t))

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	stamp := func(h int) int64 {
		return todayStart.Add(time.Duration(h) * time.Hour).Unix()
	}
	insertUsage := func(key string, ts int64, inTok int) {
		_, err := db.Exec(`INSERT INTO usage_records
			(provider_id, model, input_tokens, output_tokens, total_tokens, cost, timestamp, key_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			"p", "m", inTok, 0, inTok, 0.0, ts, key)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	insertUsage("k1", stamp(22), 50) // 末段

	got, err := GetKeyTodayBuckets("k1", "5h")
	if err != nil {
		t.Fatalf("GetKeyTodayBuckets: %v", err)
	}
	if len(got.Buckets) != 5 {
		t.Fatalf("Buckets length = %d, want 5 (不补齐末段)", len(got.Buckets))
	}
	if got.Buckets[4].InputTokens != 50 {
		t.Errorf("末段 input = %d, want 50", got.Buckets[4].InputTokens)
	}
	// 末段 t 字段应为 20:00:00
	wantT := todayStart.Add(20 * time.Hour)
	if !got.Buckets[4].T.Equal(wantT) {
		t.Errorf("末段 t = %v, want %v", got.Buckets[4].T, wantT)
	}
}
```

- [ ] **Step 2: 跑新增 4 个测试**

Run: `go test ./stats/... -run 'TestGetKeyTodayBuckets_(Empty|InvalidBucket|Boundary|LastBucket)' -v`
Expected: 全部 PASS

- [ ] **Step 3: 跑全量 stats 测试**

Run: `go test ./stats/... -v`
Expected: 全部 PASS

- [ ] **Step 4: commit**

```bash
git add stats/stats_test.go
git commit -m "test(stats): add boundary/empty/invalid-bucket coverage for GetKeyTodayBuckets"
```

---

## Task 4: 后端 — 注册路由 + handler + smoke test

**Files:**
- Modify: `web/web.go:81` 后（紧贴 `getServerKeyStats` 路由后）
- Modify: `web/web.go` 末尾（在 `getServerKeyStats` 函数后追加 handler）

- [ ] **Step 1: 在 `web/web.go:81` 后注册新路由**

找到 `api.GET("/server-keys/:id/stats", getServerKeyStats)` 这一行（[web/web.go:81](web/web.go#L81)），在其后插入：

```go
		api.GET("/server-keys/:id/today-stats", getKeyTodayStats)
```

- [ ] **Step 2: 在 `web/web.go` `getServerKeyStats` 函数后追加 handler**

找到 `func getServerKeyStats(c *gin.Context)` 结束位置（约 [web/web.go:385](web/web.go#L385)），在其后插入：

```go
// getKeyTodayStats 返回指定 key 今日按桶切分的时间序列。
// Query: bucket=5h|hour（默认 5h）。
func getKeyTodayStats(c *gin.Context) {
	id := c.Param("id")
	bucket := c.DefaultQuery("bucket", "5h")
	result, err := stats.GetKeyTodayBuckets(id, bucket)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}
```

- [ ] **Step 3: 编译检查**

Run: `go build ./...`
Expected: 无错误

- [ ] **Step 4: 启动服务 + smoke test**

```bash
# 后台启动（端口默认 8080，看实际配置）
go run . &
sleep 3
```

然后在另一个 shell 跑 curl 验证（需要替换 `<KEY_ID>` 为实际某个 ServerKey 的 id，可从 `GET /api/server-keys` 取）：

```bash
# 先登录拿 token / cookie（按现有登录方式）
# 简化：默认未登录会 401，确认我们的路由也走鉴权中间件
curl -s http://localhost:8080/api/server-keys/<KEY_ID>/today-stats | head -c 200
```

Expected: 返回 JSON，包含 `key_id`、`date: "2026-06-29"`、`bucket: "5h"`、`buckets: [{t, input_tokens, ...}, ...]`，长度 5。

```bash
# 测试 hour
curl -s 'http://localhost:8080/api/server-keys/<KEY_ID>/today-stats?bucket=hour' | python -c "import json,sys; d=json.load(sys.stdin); print(len(d['buckets']))"
```

Expected: `24`

- [ ] **Step 5: 关闭后台进程**

```bash
kill %1 2>/dev/null
```

- [ ] **Step 6: commit**

```bash
git add web/web.go
git commit -m "feat(web): add /server-keys/:id/today-stats endpoint"
```

---

## Task 5: 前端 — 拷贝 ECharts 到 vendor + 加 script 标签

**Files:**
- Create: `web/static/vendor/echarts.min.js`（从 minimax-monitor 拷贝）
- Modify: `web/static/index.html` — 在 `</body>` 前加 `<script>` 标签

- [ ] **Step 1: 拷贝 ECharts 文件**

Run:
```bash
mkdir -p web/static/vendor
cp "C:/Users/Admin/go/src/minimax-monitor/internal/server/web/vendor/echarts.min.js" web/static/vendor/echarts.min.js
ls -lh web/static/vendor/echarts.min.js
```

Expected: 文件约 1MB（约 1029203 bytes）

- [ ] **Step 2: 在 `index.html` `</body>` 前加 script 标签**

找到 `index.html` 末尾的 `</body>` 标签，在其前一行插入：

```html
<script src="static/vendor/echarts.min.js"></script>
```

- [ ] **Step 3: 启动服务并访问 admin 首页**

```bash
go run . &
sleep 3
```

浏览器访问 `http://localhost:8080/`（登录后到 admin），打开 DevTools 控制台，运行：

```js
typeof echarts
```

Expected: `"object"`（不是 `"undefined"`）

- [ ] **Step 4: 关闭后台进程**

```bash
kill %1 2>/dev/null
```

- [ ] **Step 5: commit**

```bash
git add web/static/vendor/echarts.min.js web/static/index.html
git commit -m "feat(web): vendor ECharts 5 + load in index.html"
```

---

## Task 6: 前端 — 加 "今日统计" 按钮 + 折叠面板 + CSS

**Files:**
- Modify: `web/static/index.html` — `renderServerKeys()` 的 actions 区域（约 [web/static/index.html:817](web/static/index.html#L817) `复制` 按钮前）
- Modify: `web/static/index.html` — 末尾 `<script>` 区域加 `toggleKeyChart` 骨架（先不含 ECharts 渲染）
- Modify: `web/static/index.html` — `<style>` 区域加 CSS

- [ ] **Step 1: 加按钮**

在 `renderServerKeys()` 返回模板的 actions 区域（约 line 814-818 之间），在 `<button class="btn-primary" onclick="copyKey('${k.key}')">复制</button>` 前插入：

```html
<button class="btn-info" onclick="toggleKeyChart('${k.id}', this)">今日统计</button>
```

- [ ] **Step 2: 加 CSS（在现有 `<style>` 块末尾）**

```css
.btn-info {
    background: #4299e1; color: #fff; border: none;
    padding: 8px 16px; border-radius: 6px;
    cursor: pointer; font-size: 13px; transition: background 200ms;
}
.btn-info:hover { background: #3182ce; }
.key-chart-panel {
    margin-top: 12px; padding: 16px;
    background: #ffffff; border: 1px solid #e2e8f0;
    border-radius: 8px; box-shadow: 0 2px 8px rgba(0,0,0,.04);
}
.chart-header {
    display: flex; justify-content: space-between; align-items: center;
    margin-bottom: 12px;
}
.chart-title { font-size: 14px; font-weight: 500; color: #2d3748; }
.bucket-toggle { display: inline-flex; gap: 4px; }
.bucket-toggle button {
    background: #fff; color: #4a5568; border: 1px solid #cbd5e0;
    padding: 4px 12px; border-radius: 6px;
    cursor: pointer; font-size: 12px; transition: all 200ms;
}
.bucket-toggle button.active {
    color: #667eea; border-color: #667eea; background: rgba(102,126,234,.08);
}
.bucket-toggle button:hover { background: #f7fafc; }
.chart-loading { text-align: center; padding: 40px; color: #999; }
```

- [ ] **Step 3: 加 `toggleKeyChart` 骨架（在 `<script>` 末尾）**

```js
const keyCharts = new Map();

function toggleKeyChart(keyId, btn) {
    const existing = keyCharts.get(keyId);
    if (existing) {
        if (existing.chart) existing.chart.dispose();
        keyCharts.delete(keyId);
        existing.panel.remove();
        btn.textContent = '今日统计';
        return;
    }

    btn.textContent = '收起统计';
    const panel = document.createElement('div');
    panel.className = 'key-chart-panel';
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
    btn.closest('.key-item').appendChild(panel);
    keyCharts.set(keyId, { chart: null, panel, toggleBtn: btn });
}
```

- [ ] **Step 4: 启动服务 + 浏览器手动验证**

```bash
go run . &
sleep 3
```

浏览器步骤：
1. 访问 admin dashboard，登录
2. 找到任意一张 key 卡片，确认多了 "今日统计" 按钮（蓝色）
3. 点击 → 卡片下方出现折叠面板（标题"今日消耗趋势" + 5h/1h toggle + 空白图表区域）
4. 再点击 "收起统计" → 面板消失，按钮文字变回 "今日统计"
5. 反复点击两个 key，确认 `keyCharts` Map 正确增删（DevTools: `keyCharts.size`）

Expected: 步骤 3-5 全部正常

- [ ] **Step 5: 关闭后台进程**

```bash
kill %1 2>/dev/null
```

- [ ] **Step 6: commit**

```bash
git add web/static/index.html
git commit -m "feat(web): add 今日统计 button + collapsible panel + CSS"
```

---

## Task 7: 前端 — ECharts 集成 + `buildKeyChartOption` + 渲染

**Files:**
- Modify: `web/static/index.html` — 替换 Task 6 的 `toggleKeyChart` 骨架，加入 ECharts init + `loadKeyChart` + `buildKeyChartOption`

- [ ] **Step 1: 替换 `toggleKeyChart` 函数，加入 ECharts 初始化**

把 Task 6 步骤 3 加的 `toggleKeyChart` 函数整体替换为：

```js
const keyCharts = new Map();
const keyStatsCache = new Map();

async function toggleKeyChart(keyId, btn) {
    const existing = keyCharts.get(keyId);
    if (existing) {
        existing.chart.dispose();
        keyCharts.delete(keyId);
        existing.panel.remove();
        btn.textContent = '今日统计';
        return;
    }

    btn.textContent = '收起统计';
    const panel = document.createElement('div');
    panel.className = 'key-chart-panel';
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
    btn.closest('.key-item').appendChild(panel);

    const chartEl = panel.querySelector('.chart');
    const chart = echarts.init(chartEl, null, { renderer: 'canvas' });
    const state = { chart, panel, toggleBtn: btn, reqSeq: 0 };
    keyCharts.set(keyId, state);
    new ResizeObserver(() => chart.resize()).observe(chartEl);

    await loadKeyChart(keyId, '5h', state);

    panel.querySelectorAll('.bucket-toggle button').forEach(b => {
        b.addEventListener('click', () => {
            panel.querySelectorAll('.bucket-toggle button').forEach(x => x.classList.remove('active'));
            b.classList.add('active');
            loadKeyChart(keyId, b.dataset.bucket, state);
        });
    });
}

async function loadKeyChart(keyId, bucket, state) {
    const mySeq = ++state.reqSeq;
    try {
        const res = await fetch(`/api/server-keys/${keyId}/today-stats?bucket=${bucket}`);
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const data = await res.json();
        if (mySeq !== state.reqSeq) return;

        if (!keyStatsCache.has(keyId)) keyStatsCache.set(keyId, new Map());
        keyStatsCache.get(keyId).set(bucket, { data, ts: Date.now() });

        state.chart.setOption(buildKeyChartOption(data), true);
    } catch (err) {
        if (mySeq !== state.reqSeq) return;
        state.chart.setOption({
            title: { text: `加载失败: ${err.message}`, left: 'center', top: 'center',
                     textStyle: { color: '#e53e3e', fontSize: 14 } }
        });
    }
}

function buildKeyChartOption(data) {
    const labels = data.bucket === '5h'
        ? ['00–05', '05–10', '10–15', '15–20', '20–24']
        : Array.from({ length: 24 }, (_, i) => String(i).padStart(2, '0'));
    const buckets = data.buckets;
    const isHour = data.bucket === 'hour';

    return {
        animation: true,
        animationDuration: 400,
        tooltip: {
            trigger: 'axis',
            backgroundColor: '#ffffff',
            borderColor: '#cbd5e0',
            textStyle: { color: '#2d3748' },
            axisPointer: { type: 'cross', label: { backgroundColor: '#667eea' } },
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
        },
        legend: {
            data: ['输入 token', '输出 token', '请求数', '花费'],
            top: 0,
            textStyle: { color: '#4a5568', fontSize: 12 }
        },
        grid: { left: 60, right: 60, top: 40, bottom: 40 },
        xAxis: {
            type: 'category',
            data: labels,
            axisLine: { lineStyle: { color: '#cbd5e0' } },
            axisLabel: { color: '#718096', fontSize: 11 },
            splitLine: { show: false }
        },
        yAxis: [
            { type: 'value', name: 'Tokens / 请求数', position: 'left',
              axisLabel: { color: '#718096', formatter: v => v >= 1000 ? (v/1000)+'k' : v },
              splitLine: { lineStyle: { color: '#edf2f7' } } },
            { type: 'value', name: '花费 ($)', position: 'right',
              axisLabel: { color: '#718096', formatter: '$${value}' },
              splitLine: { show: false } }
        ],
        series: [
            { name: '输入 token', type: 'line', smooth: true, symbol: 'none',
              itemStyle: { color: '#667eea' },
              areaStyle: { color: 'rgba(102,126,234,0.18)' },
              data: buckets.map(b => b.input_tokens) },
            { name: '输出 token', type: 'line', smooth: true, symbol: 'none',
              itemStyle: { color: '#764ba2' },
              data: buckets.map(b => b.output_tokens) },
            { name: '请求数', type: 'bar', yAxisIndex: 0,
              itemStyle: { color: 'rgba(237,137,54,0.4)' },
              barWidth: '40%',
              data: buckets.map(b => b.request_count) },
            { name: '花费', type: 'line', yAxisIndex: 1, smooth: true, symbol: 'none',
              itemStyle: { color: '#11998e' },
              lineStyle: { color: '#11998e', width: 1.5, type: 'dashed' },
              data: buckets.map(b => b.cost) }
        ]
    };
}
```

> 注：Task 6 的 Step 3 加的 `keyCharts` Map 改成 `keyCharts = new Map()` + `keyStatsCache = new Map()`，并把 `state.chart = null` 改为真实 init。此步骤直接覆盖 Task 6 的代码。

- [ ] **Step 2: 启动服务 + 浏览器手动验证**

```bash
go run . &
sleep 3
```

浏览器步骤：
1. 访问 admin dashboard，登录
2. 找一张今日有请求的 key（如未触发请求，先用 curl 模拟：`curl -X POST http://localhost:8080/v1/chat/completions -H "Authorization: Bearer <KEY>" -d '{"model":"...","messages":[...]}'`，参考 [ReadMe2.md](ReadMe2.md) 或现有 API 调用方式）
3. 点击 "今日统计" → 看到 ECharts 折线图
   - X 轴 5 个等宽段：`00–05`、`05–10`、`10–15`、`15–20`、`20–24`
   - 4 条数据系列：输入 token（蓝线带面积）、输出 token（紫线）、请求数（橙色柱）、花费（绿色虚线）
   - 双 Y 轴：左 tokens/请求数，右 花费($)
4. hover 任意柱/线 → tooltip 显示该段时间 + 4 个具体数值
5. 点击 toggle "1h" → X 轴变 24 段，数据刷新
6. 点击 toggle "5h" → 回到 5 段
7. 点击 "收起统计" → 图表消失

Expected: 步骤 3-7 全部正常

- [ ] **Step 3: 关闭后台进程**

```bash
kill %1 2>/dev/null
```

- [ ] **Step 4: commit**

```bash
git add web/static/index.html
git commit -m "feat(web): render ECharts line chart with multi-series + dual Y-axis"
```

---

## Task 8: 前端 — bucket toggle + 5 分钟缓存 + `reqSeq` 竞态保护

**Files:**
- Modify: `web/static/index.html` — `loadKeyChart` 加缓存逻辑（已在 Task 7 Step 1 一次性写完；本任务只验证）

> 注：Task 7 Step 1 已经把 `keyStatsCache` Map + `reqSeq` 序号 + 缓存命中短路逻辑全部包含在内。本任务仅做手动验证 + 必要时微调。

- [ ] **Step 1: 启动服务**

```bash
go run . &
sleep 3
```

- [ ] **Step 2: 验证缓存命中**

浏览器步骤：
1. 点 "今日统计" → Network 标签记一个 `/today-stats?bucket=5h` 请求
2. 点 "1h" → 记一个 `/today-stats?bucket=hour` 请求
3. 点回 "5h" → **不应有**新网络请求（5 分钟内复用缓存）
4. DevTools console: `keyStatsCache.get('<KEY_ID>').size` → 应为 `2`（5h + hour 各一）

- [ ] **Step 3: 验证竞态保护**

浏览器步骤：
1. 点 "今日统计" 立即连续点 toggle "5h" → "1h" → "5h"（快速切换）
2. Network 应看到 2-3 个请求并发发出，但**最终显示** 5h 的数据（最后一次切换结果）
3. 没有错误或空白图表

- [ ] **Step 4: 验证收起后内存清理**

浏览器步骤：
1. 点 "今日统计" 展开 → DevTools: `keyCharts.size === 1`
2. 点 "收起统计" → DevTools: `keyCharts.size === 0`（`chart.dispose()` 被调用，DOM 节点移除）

- [ ] **Step 5: 关闭后台进程**

```bash
kill %1 2>/dev/null
```

- [ ] **Step 6: commit（如有微调）**

如未调整代码，无需 commit；如有调整：

```bash
git add web/static/index.html
git commit -m "fix(web): tuning cache/race protection behavior"
```

---

## Task 9: 端到端验证 + 回归

**Files:** 无（验证任务）

- [ ] **Step 1: 编译检查**

Run: `go build ./...`
Expected: 无错误

- [ ] **Step 2: 全量测试**

Run: `go test ./... -count=1`
Expected: 全部 PASS

- [ ] **Step 3: 启动服务**

```bash
go run . &
sleep 3
```

- [ ] **Step 4: 浏览器全流程验证**

浏览器步骤：
1. 登录 → admin dashboard 渲染正常
2. key 列表渲染正常（包括新加的 "今日统计" 按钮）
3. provider 列表渲染正常
4. 现有 4 个 key-stat（今日请求/总请求/今日花费/总花费）显示正确
5. WebSocket 实时更新 key-stat 仍工作（用 curl 触发请求，看数字变化）
6. 点击 "今日统计" → 折线图渲染正常
7. toggle 5h/1h → 数据正确切换
8. hover tooltip 显示具体数值
9. 收起 → 内存清理
10. log.html 页面（`/log.html`）渲染正常，无回归
11. 登出 → 重新登录 → 状态保持

Expected: 步骤 1-11 全部通过

- [ ] **Step 5: 关闭后台进程**

```bash
kill %1 2>/dev/null
```

- [ ] **Step 6: commit（如有发现并修复）**

如有修复：

```bash
git add -A
git commit -m "fix: e2e regression fixes for today-stats chart"
```

---

## 任务依赖图

```
Task 1 (失败测试)
   ↓
Task 2 (实现类型 + 函数)
   ↓
Task 3 (边界测试)
   ↓
Task 4 (注册路由)
   ↓
Task 5 (拷贝 ECharts)
   ↓
Task 6 (按钮 + 折叠面板 + CSS)
   ↓
Task 7 (ECharts 渲染)
   ↓
Task 8 (缓存 + 竞态验证)
   ↓
Task 9 (端到端)
```

每个任务都包含自己的 commit。Task 4 和 Task 5 可以并行（无依赖）。