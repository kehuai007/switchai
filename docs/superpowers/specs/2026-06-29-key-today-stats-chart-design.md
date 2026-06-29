# 每个 API 密钥的今日统计折线图 — 设计文档

日期: 2026-06-29
状态: 待用户复核

## 背景

SwitchAI 网关的 admin dashboard（`web/static/index.html`）目前为每个 ServerKey 卡片展示当日聚合数字：

- 今日请求数 (`today_req_count`)
- 今日花费 (`today_cost`)

数据来自 `GET /api/stats`（`stats/stats.go:410` 处通过 LEFT JOIN `key_daily_stats` 取到）。

用户希望**点击展开**每张 key 卡片后，能看到一条折线图，展示今日按时间桶分布的 token 消耗统计与具体数值，方便快速看出该 key 的流量时段。

视觉风格参考 `C:\Users\Admin\go\src\minimax-monitor\internal\server\web\`（ECharts 折线图），但保持 switchai 当前的亮色主题。

## 设计目标

- 每张 key 卡片加一个 "今日统计" 按钮，点击在卡片下方展开/折叠折线图
- 默认按 5 小时一个桶（一天 5 段），可切换到按小时（24 段）
- 折线图同时展示：输入 tokens、输出 tokens、请求数、花费($)
- tooltip 展示每个桶的具体数值
- 后端不新建表，复用 `usage_records` 做实时 GROUP BY 查询
- 视觉保持 switchai 亮色（白底 + 紫绿蓝 accent），不切暗色主题

---

## 一、后端 API

### 1.1 端点

`GET /api/server-keys/:id/today-stats?bucket=5h|hour`

- 鉴权：复用现有 admin 鉴权中间件，与 `GET /api/server-keys` 一致
- 路径参数 `:id` 为 ServerKey ID
- Query 参数 `bucket`：可选值 `5h`（默认）、`hour`

### 1.2 响应

```json
{
  "key_id": "abc123",
  "date": "2026-06-29",
  "bucket": "5h",
  "buckets": [
    {"t": "2026-06-29T00:00:00+08:00", "input_tokens": 120, "output_tokens": 80, "request_count": 3, "cost": 0.0021},
    {"t": "2026-06-29T05:00:00+08:00", "input_tokens": 0,  "output_tokens": 0,  "request_count": 0, "cost": 0.0},
    {"t": "2026-06-29T10:00:00+08:00", "input_tokens": 450, "output_tokens": 320, "request_count": 12, "cost": 0.0089},
    {"t": "2026-06-29T15:00:00+08:00", "input_tokens": 0,  "output_tokens": 0,  "request_count": 0, "cost": 0.0},
    {"t": "2026-06-29T20:00:00+08:00", "input_tokens": 80,  "output_tokens": 50,  "request_count": 2, "cost": 0.0015}
  ]
}
```

`bucket=hour` 时 `buckets` 长度为 24，`t` 字段是各整点时间戳。

**空桶补齐**：即使某个桶 0 请求也要保留在数组中（值为 0），便于前端画等宽 X 轴。

### 1.3 桶边界

| 桶 | 时间段 | 长度 |
|----|--------|------|
| 1 | 00:00-04:59:59 | 5h |
| 2 | 05:00-09:59:59 | 5h |
| 3 | 10:00-14:59:59 | 5h |
| 4 | 15:00-19:59:59 | 5h |
| 5 | 20:00-23:59:59 | 4h（自然切分，不补） |

末段不满 5h 是事实，不做特殊补齐；tooltip 会显示真实时间范围（如 `20:00 - 当前`）。

### 1.4 SQL

按小时切桶的 SQL 模板：

```sql
SELECT
  CASE
    WHEN ? = '5h' THEN (timestamp / 18000) * 18000
    ELSE (timestamp / 3600) * 3600
  END AS t,
  COALESCE(SUM(input_tokens), 0),
  COALESCE(SUM(output_tokens), 0),
  COALESCE(SUM(cost), 0),
  COUNT(*)
FROM usage_records
WHERE key_id = ? AND timestamp >= ?
GROUP BY t
ORDER BY t
```

参数顺序：`bucket`, `key_id`, `today_start_unix`。

**`today_start_unix` 取值**：本地时区今天 00:00 的 Unix 秒戳（与 `key_daily_stats.date` 字段 `today` 取值保持一致，见 `stats/stats.go:410`）。

### 1.5 后空桶补齐

SQL GROUP BY 只返回有请求的桶。需要在 Go 层补齐所有空桶：

- `bucket=5h`：固定 5 个桶
- `bucket=hour`：固定 24 个桶
- 用 `map[int64]Bucket` 按 `t` 索引，缺失的桶补 0

---

## 二、后端改动（Go）

### 2.1 新增类型（`stats/stats.go`）

紧贴现有 `KeyStats` struct 后：

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

### 2.2 新增函数 `stats/stats.go`

```go
func GetKeyTodayBuckets(keyID, bucket string) (*TodayStats, error) {
    if bucket != "5h" && bucket != "hour" {
        bucket = "5h"
    }

    now := time.Now()
    todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
    todayStartUnix := todayStart.Unix()

    div := 3600
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

    // 补齐空桶
    totalBuckets := 24
    if bucket == "5h" {
        totalBuckets = 5
    }
    buckets := make([]TimeBucket, totalBuckets)
    for i := 0; i < totalBuckets; i++ {
        t := todayStartUnix + int64(i)*int64(div)
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

### 2.3 路由注册（`service/server.go` 或对应的路由文件）

参考 `GET /api/server-keys` 的注册方式，新增：

```go
r.GET("/api/server-keys/:id/today-stats", handleGetKeyTodayStats)
```

handler 实现：

```go
func handleGetKeyTodayStats(c *gin.Context) {
    id := c.Param("id")
    bucket := c.DefaultQuery("bucket", "5h")
    stats, err := statspkg.GetKeyTodayBuckets(id, bucket)
    if err != nil {
        c.JSON(500, gin.H{"error": err.Error()})
        return
    }
    c.JSON(200, stats)
}
```

（具体函数名与现有 handler 风格保持一致；放在与现有 `/api/server-keys/*` 路由相邻的位置）

### 2.4 不在范围内

- 不新建聚合表
- 不修改 `key_stats` / `key_daily_stats` 表
- 不动 `GET /api/stats`
- 不暴露跨日查询（只支持今日）

---

## 三、前端改动（`web/static/index.html`）

### 3.1 引入 ECharts

从 `C:\Users\Admin\go\src\minimax-monitor\internal\server\web\vendor\echarts.min.js` 拷贝到 `web/static/vendor/echarts.min.js`。

在 `index.html` `</body>` 之前加：

```html
<script src="static/vendor/echarts.min.js"></script>
```

### 3.2 key 卡片加按钮（`index.html:817` 编辑/删除按钮旁）

在 `renderServerKeys()` 的 actions 区域插入：

```html
<button class="btn-info" onclick="toggleKeyChart('${k.id}', this)">今日统计</button>
```

CSS（沿用现有 `.btn-info` 蓝色；若没有则新增）：

```css
.btn-info {
    background: #4299e1; color: #fff; border: none; padding: 8px 16px;
    border-radius: 6px; cursor: pointer; font-size: 13px;
}
.btn-info:hover { background: #3182ce; }
```

### 3.3 展开/折叠 JS

```js
const keyCharts = new Map();        // key_id -> { chart, panel, toggleBtn }
const keyStatsCache = new Map();    // key_id -> Map<bucket, { data, ts }>

async function toggleKeyChart(keyId, btn) {
    const existing = keyCharts.get(keyId);
    if (existing) {
        // 折叠
        existing.chart.dispose();
        keyCharts.delete(keyId);
        existing.panel.remove();
        btn.textContent = '今日统计';
        return;
    }

    // 展开
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
    keyCharts.set(keyId, { chart, panel, toggleBtn: btn });
    new ResizeObserver(() => chart.resize()).observe(chartEl);

    await loadKeyChart(keyId, '5h', chart);

    panel.querySelectorAll('.bucket-toggle button').forEach(b => {
        b.addEventListener('click', () => {
            panel.querySelectorAll('.bucket-toggle button').forEach(x => x.classList.remove('active'));
            b.classList.add('active');
            loadKeyChart(keyId, b.dataset.bucket, chart);
        });
    });
}

async function loadKeyChart(keyId, bucket, chart) {
    // 5 分钟内同 bucket 复用缓存
    const cached = keyStatsCache.get(keyId)?.get(bucket);
    if (cached && Date.now() - cached.ts < 5 * 60 * 1000) {
        chart.setOption(buildKeyChartOption(cached.data), true);
        return;
    }

    try {
        const res = await fetch(`/api/server-keys/${keyId}/today-stats?bucket=${bucket}`);
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const data = await res.json();

        if (!keyStatsCache.has(keyId)) keyStatsCache.set(keyId, new Map());
        keyStatsCache.get(keyId).set(bucket, { data, ts: Date.now() });

        chart.setOption(buildKeyChartOption(data), true);
    } catch (err) {
        chart.setOption({
            title: { text: `加载失败: ${err.message}`, left: 'center', top: 'center',
                     textStyle: { color: '#e53e3e', fontSize: 14 } }
        });
    }
}
```

### 3.4 ECharts option

```js
function buildKeyChartOption(data) {
    const labels = data.bucket === '5h'
        ? ['00–05', '05–10', '10–15', '15–20', '20–24']
        : Array.from({ length: 24 }, (_, i) => String(i).padStart(2, '0'));

    const buckets = data.buckets;
    const isHour = data.bucket === 'hour';
    const timeSuffix = isHour ? '时' : '段';

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

### 3.5 样式

```css
.key-chart-panel {
    margin-top: 12px;
    padding: 16px;
    background: #ffffff;
    border: 1px solid #e2e8f0;
    border-radius: 8px;
    box-shadow: 0 2px 8px rgba(0,0,0,.04);
}
.chart-header {
    display: flex; justify-content: space-between; align-items: center;
    margin-bottom: 12px;
}
.chart-title { font-size: 14px; font-weight: 500; color: #2d3748; }
.bucket-toggle { display: inline-flex; gap: 4px; }
.bucket-toggle button {
    background: #fff; color: #4a5568; border: 1px solid #cbd5e0;
    padding: 4px 12px; border-radius: 6px; cursor: pointer; font-size: 12px;
}
.bucket-toggle button.active {
    color: #667eea; border-color: #667eea; background: rgba(102,126,234,.08);
}
.bucket-toggle button:hover { background: #f7fafc; }
```

### 3.6 不在范围内

- 折线图不接 WebSocket 实时刷新（5 分钟缓存足够，避免抖动）
- key 卡片顶部数字（今日请求/花费）继续走 WebSocket，不动
- 其他页面（log.html）不加此图表

---

## 四、错误处理 + 边界

### 4.1 边界情况

| 场景 | 处理 |
|------|------|
| key 今日无任何请求 | 返回空桶数组（值全 0），前端正常画图（X 轴完整，Y 轴全 0） |
| key 不存在 | SQL 查不到记录 → 返回空桶数组（视为"今日无请求"）；无需 404 |
| key 被禁用 | 与"无请求"一致，禁用不影响统计查询 |
| bucket 参数非法 | 后端 fallback 到 `5h` |
| 跨日 00:00 重置 | 每次点击重新拉，缓存 5 分钟过期后下次拉新 |
| DB 错误 | 500 + Go 层错误信息，前端显示 "加载失败: ..." |
| 图表库加载失败 | `<script>` 标签在 `</body>` 前同步加载；失败则 `echarts` undefined → toggleKeyChart 直接 alert 错误 |

### 4.2 测试要点

**后端单测（`stats/stats_test.go`）**：

1. `TestGetKeyTodayBuckets_5h`：插入跨 5h 边界的请求，验证桶分组正确
2. `TestGetKeyTodayBuckets_Hour`：插入跨小时边界的请求，验证 24 桶补齐
3. `TestGetKeyTodayBuckets_Empty`：key 今日无请求 → 返回全 0 的 5/24 个桶
4. `TestGetKeyTodayBuckets_InvalidBucket`：传 `bucket=day` → fallback 到 5h
5. `TestGetKeyTodayBuckets_Boundary`：00:00:01、04:59:59、05:00:00 等边界值分组正确
6. `TestGetKeyTodayBuckets_LastBucket`：末段 20:00-23:59:59 实际是 4h，前端只显示数据，不补齐时间

**端到端（手动）**：

1. 进入 admin → 看到 key 列表，每张卡片多一个 "今日统计" 按钮
2. 点击 → 卡片下方展开图表，X 轴 5 个等宽段
3. 切换到 "1h" → X 轴变 24 段，Y 轴数据刷新
4. hover 任意点 → tooltip 显示该段时间 + 具体数值
5. 点击 "收起统计" → 图表消失
6. 反复点击同一 key 的不同 bucket → 第二次点击不重新请求（5 分钟内）

**回归重点**：

- 现有 `renderServerKeys` 顶部的 4 个 key-stat（今日请求/总请求/今日花费/总花费）未动
- 现有 WebSocket 实时更新 key-stat 逻辑未动
- 现有 `/api/stats` 返回结构未动

---

## 五、改动文件清单

| 文件 | 改动类型 | 大致行数 |
|---|---|---|
| `stats/stats.go` | 新增 `TimeBucket`/`TodayStats` 类型 + `GetKeyTodayBuckets` | +90 / -0 |
| `stats/stats_test.go` | 新增 6 个测试函数 | +180 / -0 |
| 路由文件（`service/` 或 `web/web.go`） | 注册新端点 + handler | +15 / -0 |
| `web/static/index.html` | 按钮 + toggle + JS 渲染 + 缓存 + CSS | +150 / -5 |
| `web/static/vendor/echarts.min.js` | 从 minimax-monitor 拷贝（无改动） | +0 |

无 DB schema 变更，无新 Go 依赖（仅静态文件）。

---

## 六、设计决策摘要

1. **5h 桶（5 段）作为默认展示**——管理员视角：自然对应"凌晨/上午/下午/傍晚/夜间"4 个工作段 + 1 个收尾段，比 24h 折线更易读。
2. **可切换到 1h（24 段）**——保留细粒度查看能力，但需要主动切换避免默认过密。
3. **后端不预聚合**——`usage_records` 已有 `idx_usage_key` 索引，今日数据量（一个 key 几百~几千条）远低于性能瓶颈；新建 `key_hourly_stats` 表的复杂度收益不匹配。
4. **前端缓存 5 分钟**——避免反复点击 toggle 重复请求；5 分钟后自动失效保证数据新鲜。
5. **不接 WebSocket 实时刷新**——避免曲线抖动；管理员想看新数据手动收起再展开即可。
6. **保持亮色**——switchai 整体亮色，切暗色是范围更大的改动，不在本次 scope。