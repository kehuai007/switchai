# 最近请求日志展示重试次数 — 设计文档

日期: 2026-06-26
状态: 待用户复核

## 背景

SwitchAI 的代理层 `proxy.doRequestWithRetry` 已有重试循环：对 5xx / 429 / 529 / 含"过载/限流"关键词的响应自动重试，最多重试 3 次（`proxy.go:362` 调用点硬编码）。

但**重试次数目前既没有持久化，也没有暴露给用户**。`history.RequestRecord` 结构体（`history.go:19-41`）和 SQLite `history` 表均无相关字段；写入 record 的代码只发生在重试循环结束后，记录的是「最终一次」的 status / duration / body，看不出来这次请求到底重试了几次。

当上游临时抖动时，用户在「最近请求」表格里看到一行「成功」记录，但实际背后可能已经重试了 2 次。重试本身消耗上游配额、影响延迟归因，需要让用户能直观识别。

## 设计目标

- 在不修改重试触发条件、不修改 `max_retries` 上限的前提下
- 把每次请求的「额外重试次数」记到 history
- 在最近请求日志表格里以「数字 + 重试高亮」形式展示，一眼扫出哪些是重试过的

---

## 一、数据模型

### 1.1 `RequestRecord` 新增字段（`history/history.go:19-41`）

```go
RetryCount int `json:"retry_count"` // 额外重试次数。0 = 一次成功，N = 重试过 N 次
```

### 1.2 `RecordSummary` 同步加同名字段（`history/history.go:380` 附近）

列表接口（`/api/history`、`/api/ws/history`、首页 `homeCache`）都需要透出该字段给前端。

### 1.3 SQLite 表加列 + 迁移（`history/history.go:166-191`）

沿用既有 `ALTER TABLE` 迁移模式（`history.go:197-198` 的 `user_model` 迁移）：

```sql
ALTER TABLE history ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0;
```

老记录的 `retry_count` 默认为 0，语义合理——它们都产生于本次改动之前，不会被错误归类为「未重试」。

INSERT（`history.go:307-347` `AddRecord`）和 `loadHomeCache` 的 SELECT（`history.go:101-105`）都需补上 `retry_count` 列。

---

## 二、采集重试次数

### 2.1 改 `doRequestWithRetry` 返回最终 attempt（`proxy/proxy.go:104-181`）

当前签名丢弃了 attempt 值。改为：

```go
func doRequestWithRetry(req *http.Request, bodyBytes []byte, provider *providers.Provider, maxRetries int) (*http.Response, int, error)
```

实现要点：
- `attempt` 从 1 开始计数（`proxy.go:109` `for attempt := 1; attempt <= maxRetries; attempt++`）
- 命名返回值 `finalAttempt int`，在所有 `return` 前赋值 `finalAttempt = attempt`
- 调用方计算 `retries := finalAttempt - 1`（额外重试次数 = 总尝试次数 - 1）

### 2.2 透传到两处 record 写入点（`proxy.go:623`、`proxy.go:752`）

`doRequestWithRetry` 唯一调用点在 `proxy.go:362`（分发函数）。它拿到 `finalAttempt` 后需要透传到流式/非流式处理函数：

1. `proxy.go:362` 改为：`resp, finalAttempt, err := doRequestWithRetry(...)`
2. `handleStreamResponse`（`proxy.go` 附近）签名加 `retryCount int` 参数
3. `handleNonStreamResponse`（`proxy.go:678` 定义）签名加 `retryCount int` 参数
4. `proxy.go:387`（stream 分支）把 `retryCount` 传进去
5. `proxy.go:752`/`proxy.go:623` 的 `RequestRecord{}` 字面量加 `RetryCount: retryCount`

`retryCount` 在调用方计算：`retryCount := finalAttempt - 1`

---

## 三、前端展示（`web/static/log.html`）

### 3.1 表格新增「重试」列

当前表格 12 列（表头 `log.html:128-139`、行渲染 `log.html:365-376`、首页 `log.html:717-728`）。在「类型」之后插入一列：

```
时间 | IP | Key | 供应商 | 模型 | 类型 | **重试** | 输入Token | 输出Token | 总Token | 花费 | 耗时 | 日志
```

行渲染：

```js
<td>${record.retry_count > 0
    ? `<span class="badge badge-retry">${record.retry_count}</span>`
    : '0'}</td>
```

两处 `colspan="12"` 同步改为 `colspan="13"`（`log.html:380`、`log.html:639`）。

### 3.2 新增 `.badge-retry` 样式

复用既有 badge 风格（参考 `.badge-stream` 实现位置附近），用琥珀色/橙色，区别于现有 OpenAI/Anthropic 协议 badge 的颜色。具体色值取实现时与既有 badge 协调的即可，本 spec 不锁死。

### 3.3 详情弹窗

`/api/history/:id` 返回的完整 record 已经包含 `retry_count`（因为是 `RequestRecord` 序列化）。在详情弹窗的「基本」信息区补一行「重试次数」即可，无额外 API 改动。

---

## 四、范围外（明确不做）

- ❌ 不把 `max_retries` 硬编码 3 改为可配置（独立话题）
- ❌ 不修改重试触发条件 / 退避策略
- ❌ 不加重试维度的筛选/排序/统计（用户未要求）
- ❌ 不改 ws 推送协议字段名（沿用 `retry_count`）

---

## 五、影响面与回归点

- **DB schema** 增量迁移，老数据 `retry_count=0`，无破坏
- **API 响应** 增字段，老客户端忽略未知字段，无破坏
- **`AddRecord`** 改动 INSERT 列数，必须同步改 SELECT（`loadHomeCache` 和 `GetRecords` / `GetRecord` / `GetRecordsSummary` 四处）
- **前端** 列数从 12 → 13，三处同步（首页 / 列表 / 空状态 colspan）
- **重试调用点** 透传链：分发函数 → handleStreamResponse / handleNonStreamResponse