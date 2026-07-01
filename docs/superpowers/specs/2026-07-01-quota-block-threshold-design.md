# 限额拦截阈值可配置

**日期**：2026-07-01
**状态**：设计稿，待评审

## 1. 背景

当前「限额拦截」开关启用后，[quota.IsBlocked](quota/quota.go#L82-L121) 以硬编码常量 `blockThreshold = 99.0`（见 [quota/quota.go:54](quota/quota.go#L54)）作为拦截线。这意味着用户只能选择「拦 / 不拦」，不能在 5h 或周限额快用完时提前拉一道更保守的线（如 90%、80%），也无法在不拦 / 全拦之间做精细控制——尤其在配额窗口末期，几分钟的延迟可能就足以打穿限额。

**关键不变量**：放行的请求会被正常转发并产生计费（输入/输出 token 都会被上游计入）。因此拦截阈值的设计意图是 **"提前切走以避免继续计费"**，而不是"等到满了才挡"。99% 的硬编码几乎等于"用尽才拦"，对保护预算不友好；本设计把阈值开放给用户调小，用更保守的线提前切走流量。

本设计为每个 provider 增加一个 1–100 的整数百分比阈值（默认 99，沿用现有行为），用户可实时下拉选择，写入后端并立即影响后续转发判断。

## 2. 目标 / 非目标

### 目标
- 每个 provider 独立的拦截阈值（1–100 整数百分比）
- 阈值持久化到 DB（重启后保留）
- UI 在现有「限额拦截」checkbox 旁新增 `<select>` 1–100，下拉选择即发送到后端（无 debounce）
- 默认值 99（与现有行为完全一致，存量 provider 无感升级）
- 运行时 `quota.IsBlocked` 用调用方传入的阈值（移除包级常量）

### 非目标
- 不做阈值模板 / 全局默认值
- 不做"按窗口分别设阈值"（5h 和周共用一个阈值；用户场景里"提前保护 5h 限额"和"提前保护周限额"通常一致，必要时可后续再加）
- 不在用量统计图表里画阈值参考线（与 [2026-07-01-generic-usage-stats-design.md](2026-07-01-generic-usage-stats-design.md) §11 "不在图表里画「限额拦截」阈值参考线" 一致）
- 不做阈值变更历史 / 审计日志
- 不动 proxy.go 已有的 403 错误体格式
- 不改 `quota_block_enabled` 的字段语义（toggle 与 threshold 是两个独立轴）

## 3. 架构

### 数据流

```
[Frontend <select class="quota-block-threshold">] 1..100
        │
        ▼ change 事件
fetch PUT /api/providers/:id/quota-block-threshold  {threshold: 95}
        │
        ▼
[web/web.go] setQuotaBlockThreshold
   ├─ 校验 1 ≤ threshold ≤ 100
   ├─ cfg.SetProviderQuotaBlockThreshold(id, threshold)  ──► SQLite
   └─ quota.SetBlockThreshold(id, threshold)               ──► 内存镜像
                                                                 │
                                                                 ▼
[proxy/proxy.go]   quota.IsBlocked(provider.ID, provider.QuotaBlockThreshold)
                                                                 │
                                                                 ▼
[quota/quota.go]   检查 5h 限额 → 检查周限额
                    任一窗口 UsedPercent >= threshold → 403
                    否则放行 → 正常转发 → stats 计数（推动下一次轮询的 UsedPercent 上涨）
```

**实时拦截链路**：
- `quota` 包每 10s 调一次 `/v1/token_plan/remains` 拿到当前 `UsedPercent`，写入内存快照（snapshot）
- 每次客户端请求到达 `proxy.go` 时，**实时**调 `IsBlocked` 读最新快照判断
- `IsBlocked` 只读不写——拦截是请求维度而非轮询维度
- 放行的请求被正常转发，`stats` 包会计数（input/output/total token），进而推动上游 `/v1/token_plan/remains` 的 UsedPercent 上涨
- 重置时间到达时，上游会把窗口的 `UsedPercent` 重置到 0（5h 或周），下一轮轮询拉到新值，`IsBlocked` 自动放行

### 检查顺序（重要）

`IsBlocked` 必须按 **5h → 周** 顺序检查（先 interval 后 weekly）：
- 任一窗口达到阈值就拦截，`BlockInfo.Window` 字段记录"是哪个窗口触发的拦截"
- 5h 通常先到顶、用户更关心 5h 提示，5h 优先返回更符合心智
- 当前代码遍历顺序 `{"interval", ...}, {"weekly", ...}` 已经正确，本设计仅约束"未来改动不得调换顺序"
```

### 持久化模型

沿用 `quota_block_enabled` 的迁移模式：
- 新增列 `quota_block_threshold INTEGER DEFAULT 99`
- `Provider` 结构体新增 `QuotaBlockThreshold int` 字段（与 `QuotaBlockEnabled bool` 同级）
- `Load()` SELECT 时读出该列；`AddProvider` / `UpdateProvider` 写入新列

### 内存模型

`quota` 包新增包级 `blockThresholds map[string]float64`（与 `blockEnabled` 并列），由 `SetBlockThreshold` 写入。`IsBlocked` 的入参保留 `providerID`，但 **threshold 由调用方传**（proxy.go 在拿到 `Provider` 时一起拿），quota 包内不查内存 map。

> **设计取舍**：阈值为什么不放在 quota 包的 map 里、跟 `blockEnabled` 一样查？
> 因为 `blockEnabled` 走 map 是历史决定的——它只存 toggle 的 bool，且前端只需按 provider id 查；但 `Provider.QuotaBlockThreshold` 已经随 `getProviders` 返回前端，前端不需要再调一个独立接口去取阈值。后端 `proxy.go` 调用 `IsBlocked` 时也已经能拿到完整 `Provider`，直接传 `provider.QuotaBlockThreshold` 最自然，省一次 map 查询、避免"内存 map 和 Provider 字段不一致"的潜在 bug。

## 4. 后端改动

### 4.1 `config/config.go`

**Schema 迁移**：
```go
// CREATE TABLE providers 中加
quota_block_threshold INTEGER DEFAULT 99

// ALTER TABLE 迁移
db.Exec("ALTER TABLE providers ADD COLUMN quota_block_threshold INTEGER DEFAULT 99")
```

**Provider 结构体加字段**：
```go
type Provider struct {
    // ... 现有字段 ...
    QuotaBlockEnabled   bool `json:"quota_block_enabled"`
    QuotaBlockThreshold int  `json:"quota_block_threshold"` // 1..100，默认 99
}
```

**Load() SELECT 改**：
```go
rows, err := db.Query("SELECT id, name, base_url, api_key, model, is_active, created_at, order_num, "+
    "COALESCE(is_openai_format, 0), COALESCE(quota_block_enabled, 0), "+
    "COALESCE(quota_block_threshold, 99) "+
    "FROM providers ORDER BY order_num")
```

`p.QuotaBlockThreshold = threshold` （[config/config.go:307-325](config/config.go#L307-L325) 同款 COALESCE 模式）

**save() / AddProvider / UpdateProvider**：新 INSERT/UPDATE 多一列 `quota_block_threshold`。
- `save()` 的 `INSERT OR REPLACE` 列表追加 `quota_block_threshold` 列
- `AddProvider` 的 INSERT 占位符追加 `?`，参数追加 `p.QuotaBlockThreshold`（默认 0 → DB DEFAULT 99）

**新增 setter**：
```go
// SetProviderQuotaBlockThreshold 更新某个 provider 的拦截阈值（1..100）。
//   - 同步更新内存中的 c.Providers[i].QuotaBlockThreshold（O(1)）；
//   - 同步写入 DB 的 quota_block_threshold 列。
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

> 注：与 `SetProviderQuotaBlockEnabled` 不同，阈值是 Provider 的"展示字段"（前端 `getProviders` 渲染 select 需要它），因此走 Provider 数组而不是独立 map。

### 4.2 `quota/quota.go`

**移除常量**：
```go
// 删除：
blockThreshold = 99.0
```

**改 `IsBlocked` 签名**：
```go
// 改前
func IsBlocked(providerID string) (bool, BlockInfo)

// 改后
func IsBlocked(providerID string, threshold float64) (bool, BlockInfo)
```

函数体内：
```go
if x.w.UsedPercent >= threshold {  // 不再用 blockThreshold
    return true, BlockInfo{...}
}
```

**新增 setter**：
```go
// SetBlockThreshold updates the in-memory threshold mirror for backward-compat
// callers that don't have a Provider struct in hand (e.g. legacy tests).
// The authoritative value lives on Provider.QuotaBlockThreshold.
func SetBlockThreshold(providerID string, threshold float64) {
    stateMu.Lock()
    defer stateMu.Unlock()
    blockThresholds[providerID] = threshold
}
```

> **取舍说明**：`SetBlockThreshold` 是冗余接口（proxy 实际上不会用它），但保留它有两个理由：
> 1. 与 `SetBlockEnabled` 对称，未来若做"全局配置"或"管理端 API"也能用；
> 2. 测试可以单独构造 quota 包内部状态而不依赖 Provider。
>
> 实际 `proxy.go` 调用 `IsBlocked` 时传 `provider.QuotaBlockThreshold`，不走 `blockThresholds` map。这是 YAGNI 的临界点：保留对称性代价是一个 map + 一个 setter 函数，去掉它能省 4 行代码。我倾向于保留，因为保持 API 对称未来收益更大。

**新增包级变量**：
```go
var (
    // ... 现有 ...
    blockThresholds = map[string]float64{} // providerID -> threshold mirror
)
```

### 4.3 `web/web.go`

**新增 handler + route**：

```go
// 注册路由（紧邻 quota-block-enabled）
api.PUT("/providers/:id/quota-block-threshold", setQuotaBlockThreshold)

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
    quota.SetBlockThreshold(id, float64(body.Threshold)) // mirror for legacy callers
    c.JSON(http.StatusOK, gin.H{"ok": true})
}
```

**`getProviders` 无需改逻辑**：因为新字段 `QuotaBlockThreshold` 在 Provider 结构体上，JSON 序列化自动带上。但若某个 Provider 的 `QuotaBlockThreshold == 0`（存量 DB），前端需要 fallback 99——见 §5 UI。

**`proxy/proxy.go` 调用处更新**（[proxy/proxy.go:274-276](proxy/proxy.go#L274-L276)）：
```go
// 改前
if blocked, info := quota.IsBlocked(provider.ID); blocked {

// 改后
threshold := provider.QuotaBlockThreshold
if threshold <= 0 { threshold = 99 } // 防御：DB 迁移前的老数据 / 测试数据
if blocked, info := quota.IsBlocked(provider.ID, float64(threshold)); blocked {
```

### 4.4 测试

**新增**：
- `config/config_test.go::TestSetProviderQuotaBlockThreshold_Persists` —— 类似 `TestSetProviderQuotaBlockEnabled_Persists`：写值 → 重启 Load → 验证 round-trip。
- `config/config_test.go::TestSetProviderQuotaBlockThreshold_OutOfRange` —— 测 setter 对越界值的行为（注：setter 不校验，校验在 web handler；setter 测试只测正常值）。
- `quota/quota_test.go::TestIsBlocked_ThresholdParam` —— 改 `IsBlocked` 签名后，旧测试都要补 `99.0` 参数。新增一个 case：toggle ON + interval 95 + threshold 90 → blocked；threshold 96 → not blocked。

**改**：
- 所有现有 `IsBlocked(id)` 调用点改为 `IsBlocked(id, 99.0)`（向后兼容的默认值）。
- `proxy.go` 集成测试（如有）需覆盖新签名。

## 5. 前端改动

**HTML**（[web/static/index.html:1732-1735](web/static/index.html#L1732-L1735)）：

改前：
```html
<label class="quota-block-toggle" data-pid="${p.id}">
    <input type="checkbox" class="quota-block-cb" ${p.quota_block_enabled ? 'checked' : ''} />
    限额拦截
</label>
```

改后：
```html
<label class="quota-block-toggle" data-pid="${p.id}">
    <input type="checkbox" class="quota-block-cb" ${p.quota_block_enabled ? 'checked' : ''} />
    限额拦截
    <select class="quota-block-threshold" data-pid="${p.id}">
        ${buildThresholdOptions(p.quota_block_threshold || 99)}
    </select>
    %
</label>
```

**`buildThresholdOptions` 工具函数**（在 index.html 顶部 utility 区）：
```js
function buildThresholdOptions(current) {
    // current 可能是 0（存量老 provider）或已设置的 1..100
    const selected = current || 99;
    let html = '';
    for (let i = 1; i <= 100; i++) {
        html += `<option value="${i}" ${i === selected ? 'selected' : ''}>${i}</option>`;
    }
    return html;
}
```

> **YAGNI 取舍**：是否提供"快捷档位"（如 [80, 90, 95, 99]）而不是 1–100 全量？——下拉是 native `<select>`，100 项性能无压力（浏览器处理），且用户场景就是"想精细控制"，全量枚举最直接。如果未来需要再加 group/preset 也不冲突。

**事件处理**（紧邻现有 `change` 委托，[index.html:1584-1600](web/static/index.html#L1584-L1600)）：

```js
document.addEventListener('change', (e) => {
    // 现有 checkbox 处理 ...
    const cb = e.target.closest('.quota-block-cb');
    if (cb) { /* ... */ }

    // 新增 select 处理
    const sel = e.target.closest('.quota-block-threshold');
    if (sel) {
        const pid = sel.getAttribute('data-pid');
        const threshold = parseInt(sel.value, 10);
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

**视觉布局**：
- `<select>` 默认宽度由浏览器决定；视觉上 checkbox → 文字 → select → `%`，横向排列
- select 宽度有限（默认约 60px），给 3 位数字也够
- 当 toggle OFF 时 select 不 disable（保持可调，阈值仍生效于下次 ON 之时）——与「threshold 是独立轴」语义一致

**CSS**（沿用现有 .quota-block-toggle 样式，无需新增）：

现有：
```css
.quota-block-toggle { display: block; font-size: 11px; margin-top: 6px; cursor: pointer; }
.quota-block-toggle input { margin-right: 3px; vertical-align: middle; }
```

新增（仅一个）：
```css
.quota-block-threshold { font-size: 11px; margin-left: 4px; padding: 0 2px; vertical-align: middle; }
```

## 6. 验收

**功能验收**：
- [ ] 新增 provider DB 行后，`quota_block_threshold` 默认 99
- [ ] UI 下拉默认值 99，存量为 0 时显示 99
- [ ] 改 select 到 95 → PUT 200 → DB 写入 95 → 内存 Provider.QuotaBlockThreshold == 95
- [ ] 阈值 95 + toggle ON + interval 96% → 403 拦截
- [ ] 阈值 95 + toggle ON + interval 94% → 不拦截
- [ ] 阈值边界 1 / 100：1 时"只要有任何使用就拦"，100 时"UsedPercent 必须 ≥ 100 才拦"（实际场景几乎总触发，因 UsedPercent clamp 在 100）
- [ ] Web handler 拒绝 threshold < 1 或 > 100，返回 400
- [ ] 重启服务后阈值保留

**回归验收**：
- [ ] toggle OFF 时不拦截（与现有行为一致）
- [ ] lazy reset：EndTime 过期窗口不参与判断（与现有行为一致）
- [ ] 用量统计 modal 仍正常打开，📊 按钮不被影响

**测试覆盖**：
- [ ] `TestSetProviderQuotaBlockThreshold_Persists`
- [ ] `TestIsBlocked_ThresholdParam`（新签名 + 不同阈值）
- [ ] 现有 IsBlocked 测试全部以 99.0 调用通过

## 7. 风险与决策记录

**决策 1：阈值走 Provider 字段 vs 走独立 map**
- 选 Provider 字段：因为 `IsBlocked` 的调用方（proxy.go）已经持有完整 Provider，再查 map 是冗余；前端 `getProviders` 也已返回完整 Provider。
- 代价：阈值"显示字段"和"持久化字段"耦合，未来若想拆分需要重构。
- 取舍：当前需求简单，Provider 字段更直接。

**决策 2：阈值 setter `SetBlockThreshold` 是否冗余**
- 决定保留：与 `SetBlockEnabled` 对称，测试方便。
- 代价：4 行额外代码 + 一个 map。
- 代价可控。

**决策 3：阈值是单值（5h + 周共用）vs 双值**
- 单值：实现简单、UI 紧凑；用户场景里"提前保护"通常对两个窗口同步。
- 双值：精细，但 UI 复杂（两个 select 挤在 quota-bar 之间）。
- 决定单值。需求里写"自定义 5h or 周限额"——"or"是用户视角的两个额度类型，不是要求两个独立阈值。如未来需要，再迭代为 `QuotaBlockThresholdInterval` / `QuotaBlockThresholdWeekly`。

**决策 4：是否 debounce**
- 不 debounce：native `<select>` 一次 change = 一次选择，没有"连续滑动"场景；后端一次 UPDATE 也廉价。
- 代价：几乎为 0。

**决策 5：阈值 100 的语义**
- 100 = "UsedPercent >= 100 即拦"。`UsedPercent` 上限通常由上游 clamp 在 100，所以 100 在实际场景"几乎总触发"，但语义上仍合法。
- 注意：阈值不是"达到 100 才拦"——恰恰相反，达到阈值即拦；放行会继续产生计费请求。100 表示"只要有任何使用就拦"，是最保守的档位。
- 保留 100 是为了"我有富余配额额度，但希望完全不消耗"这种边界场景；多数用户应该选 90/95/99。