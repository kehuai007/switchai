# 编辑 API 密钥支持自定义（保持固定格式） — 设计文档

日期: 2026-06-26
状态: 待用户复核

## 背景

SwitchAI 网关对外暴露的客户端密钥叫 `ServerKey`，目前由后端随机生成，前端无法修改。随机生成逻辑位于：

- `web/web.go:294-322`（创建时自动生成）
- `web/web.go:482-520`（`/server-keys/generate` 端点）

格式固定为 `sk-` 前缀 + 16 位 `[a-zA-Z0-9]`，共 19 字符。

**两处阻断编辑**：

1. **服务端**：`config/config.go:589` 的 `UpdateServerKey` 用 `c.ServerKeys[i].Key` 强制覆盖，把传入的 `key` 字段直接丢弃
2. **前端**：`web/static/index.html:391-413` 的编辑弹窗用 `<span id="editKeyValueDisplay">` 只读展示密钥，根本没有 `<input>`

用户希望能在编辑弹窗里手动改 key，但要维持固定格式（`sk-` 前缀 + 16 位 `[a-zA-Z0-9]`）。本设计只动**编辑路径**，创建路径保持原行为（仍自动生成）。

## 设计目标

- 允许用户在编辑 API 密钥弹窗里自定义 key 值
- 保持格式约束：`sk-` 前缀 + 16 位 `[a-zA-Z0-9]`
- key 与其他 ServerKey 重复时拒绝并报错
- 不改创建路径
- 不动 DB schema
- 不修随机生成的 modulo 偏置（属于方案 B 范围）

---

## 一、接口契约（`PUT /server-keys` 行为变更）

端点已存在（`web/web.go:75-85`）。请求体新增透传 `key`：

```json
{
  "id": "...",
  "key": "sk-AbCdEfGh12345678",
  "remark": "...",
  "is_enabled": true,
  "order": 0,
  ...
}
```

**响应**：

| 场景 | HTTP | 说明 |
|---|---|---|
| 格式合法 + 唯一 + ID 存在 | 200 | 更新后的密钥列表（沿用现有） |
| key 格式错 | 400 | 后端 message 直显 |
| key 与其他 ServerKey 重复 | 409 | "密钥已被其他密钥使用" |
| key 为空字符串 | 200 | 视为未改，保持旧值 |
| key 等于当前旧值 | 200 | 唯一性检查跳过自己 |
| ID 不存在 | 404 | 沿用现有 |

---

## 二、后端改动（Go）

### 2.1 新增校验函数 `config/config.go`

紧贴 `ServerKey` struct 定义后：

```go
func validateServerKeyFormat(key string) error {
    const prefix = "sk-"
    const bodyLen = 16
    if !strings.HasPrefix(key, prefix) {
        return fmt.Errorf("密钥必须以 %q 开头", prefix)
    }
    body := key[len(prefix):]
    if len(body) != bodyLen {
        return fmt.Errorf("密钥主体长度必须为 %d", bodyLen)
    }
    for _, r := range body {
        if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
            return fmt.Errorf("密钥主体只能包含字母和数字")
        }
    }
    return nil
}
```

需确认 `config/config.go` 已 import `strings`、`fmt`、`errors`（若是新增 sentinel 会用到 `errors.New`）。实施时打开文件头部核查，不够则补 import。

### 2.2 修改 `UpdateServerKey`（`config/config.go:589`）

把强制覆盖那一行替换为按规则接受 key：

```go
func (c *Config) UpdateServerKey(key ServerKey) error {
    for i, k := range c.ServerKeys {
        if k.ID != key.ID {
            continue
        }
        if key.Key == "" {
            key.Key = k.Key
        } else {
            if err := validateServerKeyFormat(key.Key); err != nil {
                return err
            }
            for _, other := range c.ServerKeys {
                if other.ID != key.ID && other.Key == key.Key {
                    return fmt.Errorf("密钥已被其他密钥使用")
                }
            }
        }
        c.ServerKeys[i] = key
        return c.saveServerKeys()
    }
    return fmt.Errorf("server key not found")
}
```

**设计决策**：`key.Key == ""` 作为"未传"的信号。优点：与现有 JSON 反序列化兼容，不需要给 `ServerKey` 加指针字段。

### 2.3 sentinel 错误 + handler 改动（`web/web.go` 的 `updateServerKey`）

为避免字符串匹配脆弱，新增 sentinel 错误：

```go
// config/config.go
var ErrServerKeyDuplicate = errors.New("密钥已被其他密钥使用")
```

`UpdateServerKey` 返回 `ErrServerKeyDuplicate` 而非裸字符串。

handler 用 `errors.Is` 判定 409：

```go
if err != nil {
    if errors.Is(err, config.ErrServerKeyDuplicate) {
        c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
        return
    }
    c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
    return
}
```

**实施时需先验证**：现有 `updateServerKey` handler 在解析 JSON 后是否 `delete(body, "key")`。如果有必须去掉（否则前端传的 key 永远到不了 service 层，方案直接失效）。如无则 handler 主体不动，只加上面的错误码映射。

### 2.4 不在范围内

- `addServerKey` 创建路径保持不变
- DB schema 不加 CHECK 约束（应用层校验足够）
- 不修 `GenerateServerKey` 的 modulo 偏置
- `ValidateServerKey`（请求鉴权用）保持不变

---

## 三、前端改动（`web/static/index.html`）

### 3.1 编辑弹窗密钥字段（`index.html:391-413`）

替换 `<span id="editKeyValueDisplay">` 为：

```html
<label>密钥</label>
<div style="display:flex; gap:8px; align-items:center;">
  <input id="editKeyInput" type="text" style="font-family:monospace; flex:1;"
         placeholder="sk- + 16 位字母数字" />
  <button type="button" onclick="regenerateEditKey()">重新生成</button>
  <button type="button" onclick="toggleEditKeyVisibility()">显示/隐藏</button>
</div>
<div id="editKeyHint" style="font-size:12px; color:#888;">
  格式：sk- + 16 位 [a-zA-Z0-9]，共 19 字符
</div>
```

### 3.2 打开弹窗的 JS 逻辑

找到打开 `#editKeyModal` 的 JS 代码，改为：

```js
document.getElementById('editKeyInput').value = k.key;
document.getElementById('editKeyValueDisplay')?.remove();
```

### 3.3 新增 `regenerateEditKey()`

```js
async function regenerateEditKey() {
    const r = await fetch('/server-keys/generate', { method: 'POST' });
    const j = await r.json();
    if (j.key) document.getElementById('editKeyInput').value = j.key;
}
```

### 3.4 保存逻辑

找到 `updateServerKey` JS 函数，把 `key` 加进请求体：

```js
body.key = document.getElementById('editKeyInput').value.trim();
```

### 3.5 错误处理

- 409：toast "该密钥已被其他密钥使用"，不清空输入框
- 400：toast 显示后端返回的具体错误
- 成功：保留现有刷新列表 + 关弹窗行为

### 3.6 显示/隐藏按钮适配

切换 input 的 `type` 在 `text` 和 `password` 之间（替代原 `<span>` 的文本切换）。

### 3.7 不在范围内

- `renderServerKeys` 的 `'•'.repeat(k.key.length)` 掩码逻辑保持不变
- 列表里"复制"按钮保持不变
- 创建流程（`renderServerKeys` 中的"新增"分支）保持不变

---

## 四、错误处理 + 边界

### 4.1 边界验证

1. **大小写敏感**：`sk-abcdef0123456789` 与 `sk-ABCDEF0123456789` 视为不同
2. **Unicode 干扰**：用户粘贴带零宽字符/全角字符的 key → 字符集校验会失败，返回 400
3. **前后空格**：前端 `.trim()` 处理；服务端不重复 trim
4. **SQLite 写入**：新值字符集已被 `[a-zA-Z0-9]` 限制，无注入风险
5. **重新生成按钮 + 编辑混合**：用户点"重新生成"后 value 被覆盖；再手改也 OK
6. **并发编辑**：两窗口同时改同一密钥不同值 → 后到者覆盖先到者，与现有 `c.ServerKeys[i] = key` 行为一致

### 4.2 测试要点

**后端单测**（`config/config_test.go` 或新文件）：

- `validateServerKeyFormat` 表驱动测试：
  - 合法：`sk-AbCdEfGh12345678`
  - 前缀错：`xx-AbCdEfGh12345678`
  - 长度短：`sk-AbCdEfGh1234567`（15 位）
  - 长度长：`sk-AbCdEfGh123456789`（17 位）
  - 含 `-`：`sk-AbCdEfGh12345-78`
  - 含中文：`sk-AbCdEfGh中文1234`
  - 空串：`""`
- `UpdateServerKey`：
  - 相同 ID 改合法新值 → 成功
  - 相同 ID 改回自己 → 跳过唯一性检查，成功
  - 新 key 与他人重复 → 返回 "已被其他密钥使用" 错误
  - 新 key 格式错 → 返回校验错误
  - ID 不存在 → 返回 "not found"

**端到端**（`web/web_test.go` 或手动）：

- POST `/server-keys` → PUT `/server-keys` 带自定义 key → GET 验证持久化 → PUT 重复 key 拿 409

**前端手动测试**（浏览器）：

1. 编辑某密钥 → 输入合法新 key → 保存 → 刷新页面值正确
2. 编辑某密钥 → 输入与他人相同 key → 保存 → 看到 409 toast
3. 编辑某密钥 → 输入 `sk-` + 15 位 → 保存 → 看到 400 + 长度提示
4. 编辑某密钥 → 输入 `xx-abcdef...` → 保存 → 看到 400 + 前缀提示
5. 编辑某密钥 → 点"重新生成" → 输入框值变化 → 保存 → 成功
6. 编辑某密钥 → 不动输入框 → 保存 → 值未变

### 4.3 回归重点

- 现有"列表里展示/复制 key"逻辑未动
- 现有 `addServerKey` 创建路径未动
- 现有 `ValidateServerKey`（请求鉴权用）继续工作 — 它的输入就是 `ServerKey.Key`，格式校验不会影响命中逻辑

---

## 五、改动文件清单

| 文件 | 改动类型 | 大致行数 |
|---|---|---|
| `config/config.go` | 新增函数 + 修改 `UpdateServerKey` | +30 / -2 |
| `web/web.go` | 确认 handler 透传 key + 错误码映射 | +5 / -1 |
| `web/static/index.html` | 编辑弹窗 HTML + JS 改动 | +30 / -10 |

无 DB schema 变更，无新依赖。