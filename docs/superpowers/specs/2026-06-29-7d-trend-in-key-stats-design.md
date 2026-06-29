# 近 7 日趋势（含今天）接入密钥统计面板

**日期**: 2026-06-29
**状态**: 待用户审阅
**关联代码**: [stats/stats.go](../../stats/stats.go), [web/static/index.html](../../web/static/index.html), [stats/stats_test.go](../../stats/stats_test.go)

## 1. 背景与目标

目前"今日消耗趋势"图表通过密钥行的"统计"按钮展开，只支持 `5h`（一天 5 段）和 `1h`（一天 24 段）两个时间桶。

用户希望增加 `7d` 桶，展示**包含今天**的近 7 天每日消耗趋势，且复用现有入口，不新增按钮、不新增路由。

**成功标准**：
1. 密钥行"统计"按钮点开后，面板桶切换从 `[5h, 1h]` 变为 `[5h, 1h, 7d]`。
2. 选择 `7d` 时图表展示 7 个分日桶（today-6 到 today），横轴 `MM-DD` 格式，标题切换为"近 7 日趋势"。
3. 5h/1h 桶行为完全不变。
4. 后端 SQL 参数化，无注入风险。
5. 7d 桶在跨日后能正确刷新（沿用现有缓存失效策略）。

## 2. 范围

**改动**：
- 后端：[stats/stats.go](../../stats/stats.go) `GetKeyTodayBuckets(keyID, bucket)` 增加 `bucket="7d"` 分支。
- 前端：[web/static/index.html](../../web/static/index.html) `toggleKeyChart` / `loadKeyChart` / `buildKeyChartOption` 三处做桶相关适配。
- 测试：[stats/stats_test.go](../../stats/stats_test.go) 新增 7d 桶单测。

**不动**：
- [web/web.go](../../web/web.go) 路由不变（`GET /api/server-keys/:id/today-stats?bucket=...` 沿用）。
- `key_daily_stats` 表结构不变（已有 `(key_id, date)` 主键）。
- `/api/stats/daily` 接口不变。
- "近7日统计"卡片（全局汇总）行为不变。

## 3. 设计

### 3.1 后端 — `GetKeyTodayBuckets` 扩展

在现有 5h/hour 分支后追加 `bucket == "7d"` 分支，从 `key_daily_stats` 取 **today-6 到 today** 共 7 天数据，按日期升序补齐缺失桶为 0。

```go
// 伪代码（最终实现按 stats.go 现有风格）
if bucket == "7d" {
    todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
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
            Scan(&b.InputTokens, &b.OutputTokens, &b.RequestCount, &b.TotalCost)
        if err != nil {
            logger.Error("Failed to get 7d bucket for %s: %v", d.Format("2006-01-02"), err)
        }
        b.T = d // 起点 = 当日 00:00
        buckets[i] = b
    }
    return &TodayStats{KeyID: keyID, Bucket: "7d", Buckets: buckets}, nil
}
```

**设计取舍**：
- 函数名 `GetKeyTodayBuckets` 保留 — 语义仍是"按时间桶切分"。
- 用 `key_daily_stats` 表而非 `usage_records` — 性能优先（7 个 PK 查询 vs 全表聚合），且避免老数据 user_model 迁移等历史坑。
- 桶的 `t` 设为当日起点（00:00）— 前端 tooltip 据此计算"该日 00:00 – 次日 00:00"，对今天则截到当前时刻。
- 缺失日期返回 0 桶 — 与 5h/1h 行为一致。

### 3.2 前端 — `toggleKeyChart`

桶切换 HTML 由两个按钮扩展为三个：

```html
<div class="bucket-toggle">
    <button class="active" data-bucket="5h">5h</button>
    <button data-bucket="hour">1h</button>
    <button data-bucket="7d">7d</button>
</div>
```

初始 `loadKeyChart(keyId, '5h', state)` 不变。

### 3.3 前端 — `loadKeyChart`

URL 不变（`GET /api/server-keys/:id/today-stats?bucket=${bucket}`），后端已识别 `7d`。缓存键 `keyStatsCache` 按 `(keyId, bucket)` 隔离，无需变更。跨日失效仍由 `todayKey`（`new Date().toISOString().slice(0,10)`）保证。

**新增**：在 `state.chart.setOption(...)` 之前，根据 `bucket` 切换面板标题：

```js
const titleEl = panel.querySelector('.chart-title');
titleEl.textContent = bucket === '7d' ? '近 7 日趋势' : '今日消耗趋势';
```

### 3.4 前端 — `buildKeyChartOption`

x 轴 labels 与 tooltip 分支扩展：

```js
const labels = data.bucket === '5h'
    ? ['00–05', '05–10', '10–15', '15–20', '20–24']
    : data.bucket === '7d'
        ? buckets.map(b => {
            const m = String(b.t.getMonth() + 1).padStart(2, '0');
            const d = String(b.t.getDate()).padStart(2, '0');
            return `${m}-${d}`;
        })
        : Array.from({ length: 24 }, (_, i) => String(i).padStart(2, '0'));

const is7d = data.bucket === '7d';
// tooltip formatter:
const tEnd = isHour
    ? new Date(tStart.getTime() + 3600 * 1000)
    : is7d
        ? (i === buckets.length - 1
            ? new Date()
            : new Date(new Date(b.t).getTime() + 86400 * 1000))
        : (i === buckets.length - 1 ? new Date() : new Date(tStart.getTime() + 18000 * 1000));
```

系列样式、图例、双 y 轴、ResizeObserver 等均复用，无改动。

## 4. 数据流

```
用户点密钥行"统计" → toggleKeyChart 创建 echarts 面板
                    → loadKeyChart(keyId, '5h', state) 默认加载
用户点 7d         → loadKeyChart(keyId, '7d', state)
                    → GET /api/server-keys/:id/today-stats?bucket=7d
                    → 后端 7 次 PK 查询 key_daily_stats
                    → 返回 7 桶，t = 当日 00:00
                    → 前端缓存 5 分钟（按 keyId+bucket 隔离）
                    → buildKeyChartOption 渲染 MM-DD 横轴 + 动态标题
```

## 5. 错误处理

- 沿用 `loadKeyChart` 现有 try/catch：失败显示 `加载失败: HTTP {status}`。
- SQL 查询失败：日志记录 + 返回 0 桶（与 5h/1h 现有行为一致）。
- 空数据：返回 7 个 0 桶，前端正常绘制横轴；不写新错误码。

## 6. 测试

**`stats/stats_test.go`** 新增 `TestGetKeyTodayBuckets_7d`：
1. 给 `key_daily_stats` 写入 7 天测试数据（含今天），但缺失中间 1 天。
2. 调用 `GetKeyTodayBuckets(keyID, "7d")`。
3. 断言：
   - `len(buckets) == 7`
   - `buckets[i].t` 按 today-6 → today 升序
   - 缺失日期的桶 `input_tokens/output_tokens/request_count/cost` 均为 0
   - 今天的桶能正确读到当天累计数据

## 7. 风险与回退

**风险**：低。纯加法，不改 5h/1h 行为；SQL 用 `?` 参数化无注入；`key_daily_stats` 已有 `(key_id, date)` 主键，7 次查询是 O(1)。

**回退**：删除第三个按钮 + 删除后端 7d 分支即可，前后端独立可拆。

## 8. 工作量估算

| 任务 | 行数（估） |
|------|-----------|
| 后端 `GetKeyTodayBuckets` 7d 分支 | ~30 |
| 单测 `TestGetKeyTodayBuckets_7d` | ~50 |
| 前端 `toggleKeyChart` 加按钮 | ~3 |
| 前端 `loadKeyChart` 动态标题 | ~3 |
| 前端 `buildKeyChartOption` 桶分支扩展 | ~10 |
| **合计** | **~95** |

半天内可完成。

## 9. 兼容性与迁移

- 老的 `bucket=5h|1h` 客户端完全兼容。
- 无需数据库迁移（`key_daily_stats` 已存在）。
- WebSocket 推送不变（仍是 token usage event → loadStats/loadDailyStats）。