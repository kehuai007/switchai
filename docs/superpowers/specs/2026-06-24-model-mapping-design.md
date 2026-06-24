# 模型映射 + 多 Provider 激活 — 设计文档

日期: 2026-06-24
状态: 待用户复核

## 背景

当前 SwitchAI 的路由模型是"所有 server key 共享同一个 active provider"，provider 只能有一个为激活态，且 `Provider.Model` 是单值字段。用户希望：

1. provider 能声明其支持的多个模型（用 `;` 分割）
2. 每个 server key 可以独立配置"用户侧模型名 → (目标 provider, 目标 provider_model)"映射表
3. 多个 provider 可同时处于激活态
4. 多浏览器标签的 UI 状态需要 WSS 实时同步

## 设计目标

- 解耦 server key 与 provider：每个 key 通过显式映射决定请求落到哪个 provider
- 严格模式：key 没有为请求的模型声明映射 → 直接拒绝，避免静默回退导致配额/账单混淆
- 向后兼容 `Provider.Model` 单值语法
- 浏览器多标签配置实时同步

---

## 一、数据模型

### 1.1 Provider

```go
type Provider struct {
    ID             string `json:"id"`
    Name           string `json:"name"`
    BaseURL        string `json:"base_url"`
    APIKey         string `json:"api_key"`
    Model          string `json:"model"`            // 语义扩展: 单值 或 "X;Y;Z"
    IsActive       bool   `json:"is_active"`        // 允许多个 true
    CreatedAt      string `json:"created_at"`
    Order          int    `json:"order"`
    IsOpenAIFormat bool   `json:"is_openai_format"`
}
```

`Model` 字段保持 string，但新增解析函数：

```go
func (p *Provider) GetSupportedModels() []string {
    if p.Model == "" {
        return nil
    }
    parts := strings.Split(p.Model, ";")
    out := make([]string, 0, len(parts))
    for _, s := range parts {
        s = strings.TrimSpace(s)
        if s != "" {
            out = append(out, s)
        }
    }
    return out
}
```

兼容：
- `"claude-sonnet-4-5"` → `["claude-sonnet-4-5"]`
- `"X;Y;Z"` → `["X","Y","Z"]`
- `""` 或 `"X;;Y"` → 过滤空字符串

### 1.2 ServerKey 新增 Mappings 字段

```go
type ModelMapping struct {
    ID           string `json:"id"`
    ServerKeyID  string `json:"server_key_id"`
    UserModel    string `json:"user_model"`     // 用户侧模型名（请求中的）
    ProviderID   string `json:"provider_id"`    // 目标 provider
    ProviderModel string `json:"provider_model"` // 该 provider 下的实际模型名
    CreatedAt    string `json:"created_at"`
}

type ServerKey struct {
    ID               string         `json:"id"`
    Key              string         `json:"key"`
    Remark           string         `json:"remark"`
    IsEnabled        bool           `json:"is_enabled"`
    CreatedAt        string         `json:"created_at"`
    Order            int            `json:"order"`
    DailyReqLimit    int            `json:"daily_req_limit"`
    TotalReqLimit    int            `json:"total_req_limit"`
    DailyCostLimit   float64        `json:"daily_cost_limit"`
    TotalCostLimit   float64        `json:"total_cost_limit"`
    Mappings         []ModelMapping `json:"mappings"` // 新增
}
```

### 1.3 数据库 Schema

新增独立表 `model_mappings`（不用 JSON 列）：

```sql
CREATE TABLE IF NOT EXISTS model_mappings (
    id TEXT PRIMARY KEY,
    server_key_id TEXT NOT NULL,
    user_model TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    provider_model TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(server_key_id, user_model),
    FOREIGN KEY (server_key_id) REFERENCES server_keys(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_model_mappings_key ON model_mappings(server_key_id);
```

约束：
- `UNIQUE(server_key_id, user_model)` 在 DB 层硬约束 1-to-1
- 删除 server_key 时 CASCADE 自动清 mappings
- 删除 provider 时不级联 — 残留 provider_id 在运行时由 `GetMappingForRouting` 检测，返回 500

### 1.4 删除的字段

- `Config.ActiveProvider`（单 ID 字段）：从 config 表中永久弃用
- `Config.SetActiveProvider()` / `Config.GetActiveProvider()` 的路由语义：删除（保留 `GetActiveProvider()` 函数但改为"返回第一个 IsActive=true 的 provider"，仅供 `testProvider` 使用）

---

## 二、路由逻辑

替换 [proxy/proxy.go:148](proxy/proxy.go#L148) 的 provider 选择。

```
请求到来 (Key=K, 解析出 UserModel=A)
  │
  ├─ 1. 鉴权与限额（同现状）
  │
  ├─ 2. 解析请求体 model 字段 → A
  │
  ├─ 3. 查 K 的 model_mappings，找 user_model = A 的行
  │     ├─ 不存在 → 403 {"error":"model 'A' not allowed for this key"}
  │     └─ 找到 mapping → (ProviderID=P1, ProviderModel=Y)
  │
  ├─ 4. 加载 Provider P1
  │     ├─ P1 不存在 → 500 {"error":"configured provider missing"}
  │     └─ P1.IsActive == false → 403 {"error":"model 'A' not supported (provider inactive)"}
  │
  ├─ 5. 用 P1 转发，request body 中 model 替换为 Y
  │     ├─ 格式转换：P1.IsOpenAIFormat 与 isIncomingOpenAIFormat 不一致 → 走现有转换
  │     └─ 走现有的 doRequestWithRetry
```

辅助函数（[config/config.go](config/config.go)）：

```go
func (c *Config) GetMappingForRouting(keyID, userModel string) (*ModelMapping, *Provider, error)
// 返回 (mapping, target_provider, error)；按映射找 + 校验 is_active
```

错误返回统一语义：

| 场景 | HTTP | 错误消息 |
|---|---|---|
| key 无映射匹配 `user_model` | 403 | `model 'X' not allowed for this key` |
| 映射存在但 `provider.IsActive=false` | 403 | `model 'X' not supported (provider inactive)` |
| 映射存在但 `provider_id` 在 DB 中找不到 | 500 | `configured provider missing` |

`requestBody["model"]` 替换点（[proxy/proxy.go:198-208](proxy/proxy.go#L198-L208)）：替换为通过 key→mapping 链路解析出的 `(provider, provider_model)`。

---

## 三、API 端点

### 3.1 新增 Provider 字段处理

- `POST /api/providers` / `PUT /api/providers/:id` — body 接受 `is_active` 字段（默认 true）
- `POST /api/providers/:id/activate` — 改造：语义改为**切换 `is_active`**（原来是单选）
- `GET /api/providers` — 返回所有 provider，每个含 `is_active` 字段（UI 显示激活徽章）

### 3.2 ServerKey 端点变更

| 端点 | 动作 |
|---|---|
| `GET /api/server-keys` | 改为返回 `mappings` 字段（嵌套在每个 key 里） |
| `POST /api/server-keys` | 创建 key，mappings 默认空 |
| `PUT /api/server-keys/:id` | 只更新 key 自己的字段，不动 mappings |
| **`POST /api/server-keys/:id/mappings`** | 新增 — 单条添加映射 |
| **`PUT /api/server-keys/:id/mappings/:mapping_id`** | 新增 — 更新单条映射 |
| **`DELETE /api/server-keys/:id/mappings/:mapping_id`** | 新增 — 删除单条映射 |
| `POST /api/server-keys/:id/test` | 改造 — body 加 `provider_id` + `provider_model` 字段 |

### 3.3 新增辅助函数

```go
// config/config.go
func (c *Config) LoadMappingsForKey(keyID string) []ModelMapping
func (c *Config) AddMapping(keyID string, m ModelMapping) (ModelMapping, error)
func (c *Config) UpdateMapping(keyID, mappingID string, m ModelMapping) error
func (c *Config) DeleteMapping(keyID, mappingID string) error
func (c *Config) GetMappingForRouting(keyID, userModel string) (*ModelMapping, *Provider, error)
```

### 3.4 配置层错误响应

| 场景 | HTTP | 错误消息 |
|---|---|---|
| `(key_id, user_model)` 已存在 | 409 | `duplicate user model name` |
| `provider_id` 在 DB 中不存在 | 400 | `provider not found` |
| `user_model` 或 `provider_model` 为空 | 400 | `required field missing` |
| 删除 provider 时仍有 mappings 引用 | 409 | `provider has X active mapping(s); delete them first` |
| 删除 server_key | — | CASCADE 自动清 mappings |

### 3.5 Provider Model 字段约束（运行时）

- `GetSupportedModels()` 返回 nil 时，UI 映射编辑里 provider_model 下拉为空
- 大小写敏感匹配（严格）

---

## 四、UI 改动

### 4.1 Provider 编辑弹窗（[index.html:273-311](web/static/index.html#L273-L311)）

| 现有 | 改为 |
|---|---|
| "模型名称" 单 input | input + 提示文字 "用 `;` 分割多模型，如 `claude-sonnet-4-5;claude-opus-4-1`" |
| 无激活开关 | 加 `IsActive` 复选框（默认 true） |
| 顶部"激活"按钮 | 保留 — 按钮文字在 `激活` ⇄ `取消激活` 间切换，点击调用 `POST /api/providers/:id/activate` 切换 is_active |
| Provider 列表显示 | 删 `activeProvider`（单 ID）后，原 `p.id === activeProvider ? 'active' : ''` 改为 `p.is_active ? 'active' : ''`；激活徽章文字 `激活` 显示在 `is_active=true` 的 provider 上 |

### 4.2 ServerKey 编辑弹窗（[index.html:330-391](web/static/index.html#L330-L391)）

新增 **"模型映射" 区块**：
- 表格行：`用户模型名 (input) | API服务商 (select, 数据源: /api/providers) | 服务商模型 (select, 选项来自选中 provider 的 GetSupportedModels()) | 删除按钮`
- "+" 按钮添加新行
- 已存在的映射：`PUT/DELETE /api/server-keys/:id/mappings/:mapping_id`
- 错误处理：保存时如果 `UNIQUE(server_key_id, user_model)` 冲突 → 后端 409 → UI 显示"用户模型名重复"

### 4.3 测试 API 连接区（[index.html:374-384](web/static/index.html#L374-L384)）

- 把现有 `provider_type` 下拉改为 `测试模型` 下拉
- 选项：遍历该 key 的所有 mappings，格式 `"{ProviderName}: {ProviderModel}"`
- 选中后请求体追加 `provider_id` + `provider_model` 字段
- **边界**：key 没有任何 mappings 时，下拉置灰显示"该 key 暂无可测试的映射"，测试按钮 disabled

### 4.4 多标签 WSS 同步

#### 后端：新增独立 WS 端点 `/api/ws/config`

```go
// web/ws_config.go (新文件)
type ConfigEvent struct {
    Type   string `json:"type"`   // "provider_added/updated/deleted/activated"
                               // "key_added/updated/deleted"
                               // "mapping_added/updated/deleted"
    ID     string `json:"id"`
    Source string `json:"source"` // 发起请求的客户端 token
}

var configBroadcaster = NewConfigBroadcaster()

// 在 web.go 的 CRUD handler 末尾调用
configBroadcaster.Broadcast(ConfigEvent{Type: "provider_updated", ID: id, Source: clientToken})
```

不在现有 `/api/ws`（stats）和 `/api/ws/history` 里加 type 字段 — 职责分离。

#### 客户端：WS 订阅

```js
const clientToken = crypto.randomUUID();
localStorage.setItem('client_token', clientToken);
const configWs = new WebSocket(`ws://${location.host}/api/ws/config`);
configWs.onmessage = (e) => {
    const ev = JSON.parse(e.data);
    if (ev.source === clientToken) return; // 自己触发的，跳过
    if (ev.type.startsWith('provider')) loadProviders();
    if (ev.type.startsWith('key') || ev.type.startsWith('mapping')) loadServerKeys();
};

fetch('/api/providers', {
    headers: { 'X-Client-Token': clientToken },
    ...
});
```

WebSocket 边界：
- 客户端断开 → 服务端清理连接，重连后立即 refresh
- 广播丢失事件可接受（数据在 DB，下次 refresh 拿到）

多标签并发：
- 两个 tab 同时编辑同一 key 的同一 `user_model`：DB UNIQUE 约束 → 后到的 409 → UI 显示错误
- `clientToken` 用 `crypto.randomUUID()` 存 localStorage，跨 tab 不共享 → 每个 tab 自己的回环天然过滤

---

## 五、Stats / History 归属

- `Provider.Name` 字段照旧填映射的目标 provider 名字
- `requestedModel` 保留为**映射后的实际模型名**（`provider_model`）— cost 计算用实际计费模型
- history 表新增 `UserModel` 字段记录**用户原始请求的模型名**（用于排查）
  - 需要 ALTER TABLE：`history` 表 schema 在 `history/history.go`，需同步更新

---

## 六、迁移与部署

### DB 迁移

- `CREATE TABLE IF NOT EXISTS model_mappings (...)` — 老 DB 自动建新表
- 删除 `config` 表里的 `active_provider` 行 — 永久弃用
- **不迁移老 key 的 mappings**：每个 key 必须重新绑定映射
  - 升级后老 key 的 mappings 为空 → 所有请求立即 403
  - UI 提示："检测到该 key 没有模型映射，请求将被拒绝。请添加至少一条映射。"
- 老 `Provider.Model` 单值不需要迁移 — 直接被 `GetSupportedModels()` 解析为 1 元素列表
- history 表新增 `user_model` 列：`ALTER TABLE history ADD COLUMN user_model TEXT`

### 前端兼容

- 老 index.html 访问 `/api/server-keys` 拿不到 `mappings` 字段 → 默认空数组 `[]`
- 新 index.html 加 `mappings` 表格区块

### 配置项默认值

- 新建 Provider：`IsActive=true`
- 新建 ServerKey：`Mappings=[]`

---

## 七、测试计划

实施时按 TDD 顺序：

1. `Provider.GetSupportedModels()` 单元测试（边界值）
2. `Config.AddMapping` / `GetMappingForRouting` 单元测试（DB 层）
3. Proxy 路由测试：
   - 无映射 → 403
   - 映射存在但 provider inactive → 403
   - 映射存在且 provider 存在+激活 → 成功
   - provider_id 找不到 → 500
4. CRUD 端点测试：
   - 重复 user_model → 409
   - 删除 provider 仍有 mappings 引用 → 409
   - 删除 server_key → CASCADE 验证
5. WebSocket 广播测试：
   - A 触发 → B 收到（clientToken 去重验证）
   - 自身 token 收到的事件被过滤

---

## 八、影响范围

修改文件：

- `config/config.go` — Provider.GetSupportedModels()、Mappings CRUD、GetMappingForRouting、删除 ActiveProvider 路由语义
- `proxy/proxy.go` — 替换 provider 选择逻辑
- `web/web.go` — 新增 mappings CRUD handler、新增 `/api/ws/config` handler、改造 activate handler、改造 testServerKey
- `web/ws_config.go` — 新建文件
- `web/static/index.html` — provider 弹窗 + server key 弹窗 + WSS 订阅
- `history/history.go` — schema 增 user_model 列，RequestRecord 增字段
- `stats/stats.go` — 记录 user_model

未修改文件：

- `logger/`、`service/`、`update/`、`appdata/`、`main.go`、构建脚本

---

## 九、未决问题

无 — 全部确认完毕。