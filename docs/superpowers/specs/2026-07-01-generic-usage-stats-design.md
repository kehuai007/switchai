# 通用用量统计 Modal — 设计文档

- 日期：2026-07-01
- 作者：brainstorming → spec
- 状态：待用户审阅

## 1. 目标

把当前**仅 MiniMax 可见**的"限额趋势" Modal 重构为**通用"用量统计" Modal**,**所有 provider**
都能打开看到 token 消耗曲线;**MiniMax 额外多画**使用率曲线、显示"当前 X% / 下次重置 / 每 1% ≈ X token"。
**后端零改动**(沿用现有 `/api/providers/:id/token-history` 与 `/api/providers/:id/quota-history`)。

打开 Modal 后,token 曲线随每次请求实时刷新,使用率曲线随 MiniMax 10s 轮询自动刷新。

## 2. 现状摘要

| 模块 | 现状 |
|------|------|
| 数据源 | `quota_history`(仅 MiniMax 轮询写入)+ `provider_token_history`(所有活跃 provider 都写) |
| 后端 API | `GET /api/providers/:id/quota-history` + `GET /api/providers/:id/token-history`,**已按 pid 工作,与 provider 类型无关** |
| 前端入口 | 仅 `.quota-bar-row[data-pid][data-window]` 的 click 委托;`renderQuotaBars` 只在 `p.quota_enabled` 时渲染该行 → 非 MiniMax 无入口 |
| Modal | `#quotaChartModal`,标题"限额趋势",固定 4 系列折线 |
| 实时刷新 | `applyQuotaUpdate` 包装器检测到当前 modal pid 匹配时调 `loadQuotaChart(range)` 刷新;**stats 推送不触发 modal 刷新** |

## 3. 用户视角流程

- 用户进入主页 → 每个 provider 卡片在 `provider-item-stats` 行**末尾**多一个 📊 图标按钮(始终显示)
- 点击 📊 → 打开 `#usageStatsModal`,标题固定为 `用量统计 - {provider.name}`(与是否 MiniMax、是否有 quota 无关)
- 如果是从已有 quota-bar-row 点击进入且传入了 `window=interval|weekly`,modal 内使用率虚线标记指向该 window,但**标题仍只是 `用量统计 - {provider.name}`**,不再追加"5h / 周限额"字样(因为 modal 现在承载通用语义)
- 所有 provider 看到:3 条 token 曲线(输入 / 输出 / 总 token),range 切换 `5h / 1h / 7d`
- footer:**始终**显示「时间段汇总:输入 X / 输出 Y / 总 token Z / 请求数 N」;**MiniMax 且有 quota snapshot 时** 追加「当前 X.X% · 下次重置 Xh · 每 1% ≈ X token」
- MiniMax 但 quota 未轮询成功 / 未启用 → 退化为通用视图(不画使用率线、不显示限额相关 footer)
- 非 MiniMax → 永远只有 3 条 token 线 + 通用 footer
- 已有的 quota-bar-row(5h / 周限额条)的 click 行为**保留**,点击后仍打开合并后的 modal,并把 `data-window` 作为初始 quota 窗口传入
- 尚未发起请求的新 provider → echarts 画布显示"暂无用量记录,请先使用一次"占位,footer 同样显示"暂无数据"

## 4. 实时刷新

### 4.1 推送来源(已存在)

| 曲线 | 推送者 | 触发频率 | WSS payload |
|------|--------|----------|-------------|
| token 曲线 | `stats.RecordUsage` | 每次成功请求 | 现有 `stats_update` 推送,包含 provider 级 token 计数 |
| 使用率曲线 | `quota` 包 10s 轮询 | 每 10s | 现有 `provider_quotas` 摘要推送 |

### 4.2 前端挂钩

- 把现有 `applyQuotaUpdate` 包装器从 `quotaChartState` 迁移到 `usageChartState`(同 pid 时调 `loadUsageChart(range)`)
- **新增**对称的 `applyStatsUpdate` 包装器:现有 WSS 收到 stats 推送时,若 `usageChartState && usageChartState.pid === updatedPid`,调 `loadUsageChart(range)` 触发 reload
- `reqSeq` 自增守卫沿用现状:切换 range 时旧的 fetch 响应被丢弃
- modal 关闭时**不断开 WSS**,其他 UI 仍需接收推送;`usageChartState=null` 后包装器 noop,无资源浪费

### 4.3 冷启动

新建 provider 首次打开 modal → token/quota history 都为空 → WSS 推送到来时若 `usageChartState.pid` 匹配,自动 reload → 看到第一条点出现。**无需额外代码**。

## 5. 改动范围

**仅前端**(`web/static/index.html` 内嵌 JS + CSS):

| # | 改动 |
|---|---|
| 1 | `renderProviders` 输出的 `provider-item-stats` 末尾追加 `<button class="usage-stats-btn" data-pid="${p.id}" title="用量统计">📊</button>`,**始终**显示(不依赖 quota_enabled) |
| 2 | 把 `#quotaChartModal` 的标题文案改为 `用量统计`(id 保留以减小 diff — 详见 §6) |
| 3 | 抽出 `showUsageStatsModal(pid, opts={window?})` 统一入口;旧 `showQuotaChartModal` 改为薄包装调用新函数 |
| 4 | `loadUsageChart(range)`:始终调 `token-history`;仅当 `hasQuota` 时调 `quota-history` |
| 5 | `buildUsageChartOption(...)`:根据 `hasQuota` 决定是否画使用率系列 + 左 Y 轴 + markLine + 限额 footer |
| 6 | `renderQuotaBars` 中 `quota-bar-row` 的 click 委托保持不变(仍调 `showQuotaChartModal(pid, window)`),通过包装函数走到合并 modal |
| 7 | 新增 `applyStatsUpdate` 包装器(对称于现有 `applyQuotaUpdate` 包装器),推送 pid 匹配时调 `loadUsageChart(range)` |
| 8 | 全局状态 `quotaChartState` → 改名为 `usageChartState`(更贴近通用语义);新增 `hasQuota` 字段 |
| 9 | CSS 新增 `.usage-stats-btn` 样式:蓝色文字按钮,8px 内边距,圆角 4px,hover 高亮;引用现有 `.btn-link` 配色风格 |
| 10 | footer 计算:无论是否 hasQuota,都汇总 token-history 的 total input/output/total/request_count 并展示 |

**后端零改动**。`web_test.go` / `quota_history_test.go` 不受影响(API 形状不变)。

## 6. ID / class 重命名策略

为减小 diff、便于 git blame 回溯:
- **保留现有 DOM id**:`quotaChartModal` / `quotaChart` / `quotaRangeToggle` / `quotaChartTitle` / `quotaChartCurrent` / `quotaChartReset` / `quotaChartPerPercent`,**仅修改它们的文本内容与用途**
- 只新增 `id="usageStatsBtn-${pid}"`(模板按钮)、新增 class `usage-stats-btn`
- JS 内部状态变量 `quotaChartState` → `usageChartState`,因为承载通用语义

理由:避免改 6 个 id + 所有引用点的连锁变更;旧 id 保留也方便回溯 git 历史理解"这是从 quota chart 演化来的"。

## 7. 数据流

```
点击 📊  (provider-item-stats 末尾)
   │
   ▼
showUsageStatsModal(pid, opts={window?})
   │
   ├─ 找到 provider 对象 → 写 title = "用量统计 - {name}"(固定文案,见 §3)
   ├─ hasQuota = !!(p.quota_enabled && p.quota_interval && p.quota_interval.enabled)
   ├─ initialWindow = opts.window || 'interval'(决定 quota-history 查询的 window,以及使用率虚线指向哪条)
   ├─ 初始化 echarts → 建立 ResizeObserver
   ├─ 写入 usageChartState = {chart, pid, window:initialWindow,
   │                          hasQuota, range:'5h', reqSeq, resizeObserver}
   │
   ▼
loadUsageChart(range)
   │
   ├─ 并行 fetch:
   │     GET /api/providers/{pid}/token-history?range={range}      ← 始终
   │     GET /api/providers/{pid}/quota-history?window={window}... ← 仅当 hasQuota
   │
   ├─ 若两者都空 → setOption({title:{text:'暂无用量记录'}}) + footer 提示"暂无数据"
   ├─ 若只有 token 数据 → 画 3 条 token 线
   └─ 若都有 → 画 4 条线(沿用 buildQuotaChartOption 行为)
   │
   ▼
footer 更新:
   始终: 总输入 X / 总输出 Y / 总 token Z / 请求数 N
   hasQuota: + "当前 X.X%  下次重置 Xh" + "每 1% ≈ X token"
```

## 8. hasQuota 判定(避免硬编码 MiniMax)

不引入"该 provider 是否 MiniMax"判断,沿用现有的运行时信号:

```js
const hasQuota = !!(p.quota_enabled
                 && p.quota_interval
                 && p.quota_interval.enabled);
```

- `quota_enabled=true` 且 `quota_interval.enabled=true` ⇒ 有 quota snapshot ⇒ `hasQuota=true` ⇒ 画使用率线
- 其它情况 `hasQuota=false` ⇒ 退化通用视图

这与现有 `renderQuotaBars` 的判定路径一致。**不增加新的硬编码,不修改 config**。

## 9. 测试

### 9.1 后端测试
无新增;`web/web_test.go` / `quota_history_test.go` 现有用例不受影响。

### 9.2 前端手动验证清单

| 场景 | 期望 |
|------|------|
| 打开非 MiniMax provider 的 📊 | 见 3 条 token 线,无使用率 Y 轴,footer 仅汇总 token |
| 打开 MiniMax 已轮询成功的 provider 的 📊 | 4 条线齐全,使用率虚线标记当前值,footer 含"每 1% ≈" |
| 打开 MiniMax 但 quota 轮询失败/未启用的 provider | 退化为通用视图 |
| 点击 MiniMax 已有 quota-bar-row(5h / 周限额) | 打开合并后 modal,使用率虚线指向对应 window |
| 新建未请求过的 provider 打开 modal | 画布显示"暂无用量记录"占位,footer 提示"暂无数据" |
| modal 打开中触发真实请求 | token 曲线立即出现新点(无需手动 refresh) |
| modal 打开中等 10s | MiniMax 使用率虚线自动移动到最新值 |
| 关闭 modal | echarts.dispose,ResizeObserver disconnect,WSS 推送不再触发 reload |
| 切换 range 5h/1h/7d | 重新拉数据;旧 fetch 响应被 reqSeq 丢弃 |

## 10. 文件改动清单

| 文件 | 改动 |
|------|------|
| `web/static/index.html` | 新增 📊 按钮、Modal 标题文案、状态改名、新增 stats 推送挂钩、CSS |
| `docs/superpowers/specs/2026-07-01-generic-usage-stats-design.md` | 本文档 |

## 11. 不做什么(YAGNI)

- 不加"花费"曲线 / 不加"总花费"统计 → 当前 `token-history` 接口不返回 cost,扩展后端超出范围
- 不重构 `quota_history` / `provider_token_history` 数据库
- 不新增后端 API
- 不动 proxy / 限额拦截逻辑
- 不动 quota-bar-row 本身(限额拦截 toggle 仍在原位)
- 不改 range 选项(沿用 `5h / 1h / 7d`,所有 provider 都合理)
- 不做多条 provider 叠加对比
- 不做曲线导出 / CSV 下载
- 不在图表里画「限额拦截」阈值参考线

## 12. 验收

- [ ] 每个 provider 卡片都有 📊 按钮
- [ ] 点击 📊 → modal 标题固定为"用量统计 - {provider.name}"(无 5h/周限额字样)
- [ ] 从 quota-bar-row 点击进入时 → 标题同上,但 quota-history 查询使用对应 window
- [ ] 非 MiniMax:3 条 token 线,无使用率轴,footer 仅汇总
- [ ] MiniMax 有 quota:4 条线 + 当前% 虚线 + "每 1% ≈ X token"
- [ ] MiniMax quota 未轮询成功:退化为通用视图
- [ ] 新 provider 冷启动:画布占位"暂无用量记录"
- [ ] modal 打开中发起请求 → token 曲线立刻刷新
- [ ] modal 打开中每 10s → MiniMax 使用率虚线自动更新
- [ ] 关闭 modal → echarts 释放,ResizeObserver 断开
- [ ] 已有 quota-bar-row 点击行为保留,打开合并后 modal
- [ ] 后端测试无新增失败
