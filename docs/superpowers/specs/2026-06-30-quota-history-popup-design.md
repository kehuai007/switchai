# 提供商额度曲线弹窗 — 设计文档

- 日期：2026-06-30
- 作者：brainstorming → spec
- 状态：已批准，等待实现

## 1. 目标

点击「API 提供商」列表中的 `quota-bar-row`（5h 限额行 / 周限额行），
弹出 Modal 展示该 provider 对应窗口（`interval` / `weekly`）的近期趋势曲线。

Modal 中包含：

- **使用百分比**曲线（左 Y 轴 0~100%）
- **输入 / 输出 / 总 token** 三条曲线（右 Y 轴，自适应 k/M/B）
- **时间粒度切换**：5h / 1h / 7d（默认 5h）
- 当前快照（最新值、下次重置时间）

## 2. 数据层

### 2.1 新表 `quota_history`

```sql
CREATE TABLE IF NOT EXISTS quota_history (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_id  TEXT    NOT NULL,
    window       TEXT    NOT NULL,        -- 'interval' | 'weekly'
    t_bucket     INTEGER NOT NULL,        -- 秒级时间桶，按 10s 取整
    used_percent REAL    NOT NULL,
    usage_count  INTEGER,                 -- 来自上游 usage_count，可空
    total_count  INTEGER,                 -- 来自上游 total_count，可空
    UNIQUE(provider_id, window, t_bucket)
);
CREATE INDEX IF NOT EXISTS idx_qh_pid_window_t
    ON quota_history(provider_id, window, t_bucket DESC);
```

写入路径：在现有 `quota.go` 的 `pollOnce()` 末尾，遍历已写入
`snapshots` 的每个 provider × 每个 enabled window，
按 `(provider_id, window, t_bucket)` upsert `used_percent / usage_count / total_count`。
同一个 10s 桶的多次轮询会被覆盖（避免抖动）。

保留策略：启动时清理 `t_bucket < now - 7d`，写完后异步清理同条件。

### 2.2 新表 `provider_token_history`

```sql
CREATE TABLE IF NOT EXISTS provider_token_history (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_id   TEXT    NOT NULL,
    t_bucket      INTEGER NOT NULL,    -- 10s 桶
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens  INTEGER NOT NULL DEFAULT 0,
    request_count INTEGER NOT NULL DEFAULT 0,
    UNIQUE(provider_id, t_bucket)
);
CREATE INDEX IF NOT EXISTS idx_pth_pid_t
    ON provider_token_history(provider_id, t_bucket DESC);
```

写入路径：在 `stats.RecordUsage()` 的同一事务内追加：
对 `t_bucket = floor(now.UnixNano()/1e10) * 10` 做 upsert
`input_tokens = input_tokens + ?, output_tokens = output_tokens + ?, ...`
（与现有 `provider_stats` 累加风格一致）。

保留策略：7 天。

> 不复用 `usage_records`：它 1000 行上限无法支撑 7 天；
> 不调大它：避免影响 `/api/history` 行为。

## 3. 后端 API

### 3.1 `GET /api/providers/:id/quota-history`

查询参数：
- `window` — `interval` | `weekly`，**必填**
- `range` — `5h` | `1h` | `7d`，**必填**

响应：
```json
{
  "window": "interval",
  "range": "5h",
  "points": [
    {"t": 1751328000, "used_percent": 12.3, "usage_count": 123, "total_count": 1000}
  ],
  "current": {"used_percent": 23.4, "reset_in_human": "3h 12m",
              "end_time": "2026-06-30T22:00:00Z"}
}
```

实现：
- `from_ts = now - (5h|1h|7d).Seconds()`
- SQL: `SELECT t_bucket, used_percent, usage_count, total_count
         FROM quota_history
         WHERE provider_id=? AND window=? AND t_bucket >= ?
         ORDER BY t_bucket ASC`
- 1h/5h 透传原始 10s 点；**7d 时按 `GROUP BY (t_bucket/300)*300` 在 SQL 里聚合到 5min 桶**（≈2016 点），
  每桶取该 5min 内**最后一个** `used_percent`（贴近"此刻"）。
  前端 ECharts `connectNulls: true` 自动连成线。

### 3.2 `GET /api/providers/:id/token-history`

查询参数：`range` — `5h` | `1h` | `7d`，**必填**。

响应：
```json
{
  "range": "5h",
  "points": [
    {"t": 1751328000, "input_tokens": 1200, "output_tokens": 800,
     "total_tokens": 2000, "request_count": 3}
  ]
}
```

实现：
- SQL: `SELECT t_bucket, input_tokens, output_tokens, total_tokens, request_count
         FROM provider_token_history
         WHERE provider_id=? AND t_bucket >= ?
         ORDER BY t_bucket ASC`
- 同样的 7d 聚合：5min 桶 `SUM(input_tokens/output_tokens/total_tokens/request_count)`。
- **0 值桶不返回**（`total_tokens > 0`）：前端只画有请求的桶，符合用户偏好。

两个 API 共用 `:id`，挂在 `web.go` 的 `api` group 里
（紧跟 `/providers/:id/quota-block-enabled` 路由），
自动受 `authMiddleware` 保护。

参数校验：`window`、`range` 必须白名单匹配，否则 400。

## 4. 前端

### 4.1 Modal 结构（index.html）

```html
<div class="modal" id="quotaChartModal">
  <div class="modal-content" style="max-width: 760px;">
    <div class="modal-header">
      <span id="quotaChartTitle">5h 限额趋势</span>
      <button onclick="hideQuotaChartModal()">×</button>
    </div>
    <div class="bucket-toggle">
      <button data-range="5h" class="active">5h</button>
      <button data-range="1h">1h</button>
      <button data-range="7d">7d</button>
    </div>
    <div id="quotaChart" style="height: 360px;"></div>
    <div class="modal-footer">
      <span id="quotaChartCurrent"></span>
      <span id="quotaChartReset"></span>
    </div>
  </div>
</div>
```

复用现有 `.modal` / `.modal-content` 样式
（[index.html:95-97](web/static/index.html#L95-L97)）。
新增 `.modal-header`、`.modal-footer`、`.bucket-toggle` 样式
（颜色/间距对齐 key-chart-panel 的 [index.html:164-168](web/static/index.html#L164-L168)）。

### 4.2 触发

`renderQuotaBars()` 给两个 `.quota-bar-row` 各加：
- `data-pid="${p.id}"`
- `data-window="interval"` / `"weekly"`
- 整行 `cursor: pointer; hover 高亮` 样式

绑定（用事件委托）：
```js
document.getElementById('providerList').addEventListener('click', (e) => {
  const row = e.target.closest('.quota-bar-row[data-pid]');
  if (!row) return;
  if (e.target.closest('.quota-block-toggle')) return; // 不和拦截 checkbox 冲突
  showQuotaChartModal(row.dataset.pid, row.dataset.window);
});
```

### 4.3 图表配置（ECharts）

双 Y 轴：

| 轴 | 内容 | 范围 | 颜色映射 |
|----|------|------|---------|
| 左 | 使用百分比 `used_percent` | 0–100，固定 | line，颜色由 `quotaBarClass()` 决定（cyan/orange/red） |
| 右 | Token 量 | 自适应 max → k/M/B formatter | input = 蓝、output = 紫、total = 黑粗线 |

系列：
- `quota-percent` — line + areaStyle，X 轴时间
- `token-input` — line，颜色 `#3b82f6`
- `token-output` — line，颜色 `#a855f7`
- `token-total` — line，`lineStyle.width=3`，颜色 `#111827`

`connectNulls: true`，数据中只画有请求的桶（token-history）；
quota-percent 用全量历史（含 0% 桶），用同色实线连接。

X 轴 `type: 'time'`，三个 range 都用同一份数据
（5h≈1800 点，1h≈360 点，7d≈2016 点）。
X 轴标签根据 range 自适应：`5h → 每小时`，`7d → 每天`。

formatter 复用 `formatTokenCount()` ([index.html:2118](web/static/index.html#L2118))。

### 4.4 状态管理

- 全局 `let quotaChartState = null; // {chart, pid, window, range, reqSeq}`
- 打开 Modal 时 `echarts.init`，关闭时 `chart.dispose()` 释放
- WSS 收到 `provider_quotas` 更新时，**仅在当前打开的 modal 对应 provider** 自动 refetch
- reqSeq 防过期请求（同 key-chart-panel 模式）

### 4.5 错误处理

- API 失败 → 图表显示居中红色错误文本（沿用 `loadKeyChart` 模式）
- `quota_enabled=false` → bar-row 整行不渲染，自然不可点
- provider 被删除 / quota 错误状态 → `renderQuotaBars` 已渲染「错误」行，
  但它不可点击（无 `data-pid`）

## 5. 测试

`quota_test.go` / 新增 `quota_history_test.go`：

| 用例 | 覆盖 |
|------|------|
| 写 10s 桶：同桶多次 upsert 后只剩最新值 | `RecordUsage` 累加路径 |
| 7 天清理：注入 8 天前的行，启动后被清掉 | `cleanupOldRows` |
| 7d range 聚合：注入 7d 范围内 600 个 10s 桶，返回 ≤ 2016 个 5min 桶 | quota handler |
| 0 值桶过滤：total_tokens=0 的桶不出现在结果里 | token handler |
| 空 provider 查询：返回 `points: []`，不报错 | API handler |
| window/range 非法值：400 | API handler |
| SQL 注入：window='interval"; DROP...' → 400 | API handler |

## 6. 文件改动清单

| 文件 | 改动 |
|------|------|
| `quota/quota.go` | `Init()` 里建 quota_history 表 + 清理；`pollOnce()` 末尾写 history |
| `quota/quota_history.go` *(新)* | `RecordQuotaSnapshot` / `QueryQuotaHistory` / `cleanupOldQuota` |
| `quota/quota_history_test.go` *(新)* | 单测 |
| `stats/stats.go` | 建 provider_token_history 表 + 清理；`RecordUsage` 事务里追加 upsert |
| `stats/stats_test.go` | 增补 1~2 个 token 累加用例 |
| `web/web.go` | 新增两个 GET handler + 路由注册 |
| `web/web_test.go` *(如有)* | API 单测：白名单、注入、空数据 |
| `web/static/index.html` | Modal DOM + 样式 + 触发 + 图表初始化 + 事件处理 |
| `docs/superpowers/specs/2026-06-30-quota-history-popup-design.md` | 本文档 |

## 7. 不做什么（YAGNI）

- 不做曲线导出 / 截图 / CSV 下载
- 不做多条 provider 叠加对比
- 不做窗口重置点（start_time / end_time）标注线
- 不做 Mobile 触屏优化（沿用现有 .modal，移动端只保证可读）
- 不改 `usage_records` 上限 / 不动 `/api/history`
- 不在图表里画「限额拦截」阈值参考线

## 8. 验收

- [ ] 点击 5h 限额行 → 弹出 modal，标题"5h 限额趋势 - {provider}"
- [ ] 点击周限额行 → 弹出 modal，标题"周限额趋势 - {provider}"
- [ ] 默认 tab 5h 高亮；切换 1h/7d 重新拉数据
- [ ] 图表包含 4 条线：使用百分比（带色阶）、输入/输出/总 token
- [ ] 右轴单位自适应（k/M/B）
- [ ] WSS 推送 quota 更新时，已打开 modal 自动 refetch
- [ ] 关闭 modal → echarts.dispose 释放
- [ ] quota_history / provider_token_history 自动清理 7 天前数据