# 模型映射通配符 (`*`) 支持 — 设计文档

日期: 2026-06-30
状态: 待用户复核

## 背景

当前 SwitchAI 在「API 服务器密钥」编辑页提供 **模型映射** 表，每个 server key
通过显式 `user_model → (provider, provider_model)` 映射决定请求落到哪个上游。
路由查询走精确 SQL：

```go
// config/config.go:917
err := db.QueryRow(
    "SELECT ... FROM model_mappings WHERE server_key_id = ? AND user_model = ?",
    keyID, userModel,
)
```

这导致配置体验非常琐碎：每接入一个新模型都要手动加一条规则；想做"把
`claude-*` 全部转给某个 provider 的快速模型"或"未识别的模型统一 fallback"
就只能 N 条规则平铺。

用户希望：

1. 在 `user_model` 字段支持 `*` 通配符，覆盖一组模型
2. 同一 key 多条规则时按**配置顺序**匹配，第一个命中胜出
3. 不破坏现有精确规则的语义

---

## 一、设计目标

- 通配风格：仅支持 `*`（glob 风格，`/` 不被 `*` 匹配）
- 优先级：**配置顺序优先**（按 mappings 切片顺序，第一个命中胜出）
- 存储零迁移：`user_model` 原样存 `TEXT`，含 `*` 合法
- DB 索引保留：精确查询仍走 SQL `WHERE user_model = ?` 索引 O(1)
- 严格性：禁止非法 pattern、禁止同 key 重复覆盖

非目标：
- 不支持 `?` `[abc]` 等其他 glob 元字符
- 不支持正则表达式
- 不支持按通配策略动态生成的 `/v1/models` 列表

---

## 二、数据模型

### 2.1 Schema（不变）

```sql
CREATE TABLE IF NOT EXISTS model_mappings (
    id TEXT PRIMARY KEY,
    server_key_id TEXT NOT NULL,
    user_model TEXT NOT NULL,           -- 新语义: 精确字面量 OR glob pattern
    provider_id TEXT NOT NULL,
    provider_model TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(server_key_id, user_model), -- 现有约束
    FOREIGN KEY (server_key_id) REFERENCES server_keys(id) ON DELETE CASCADE
);
```

`user_model` 现在接受三类值：
- **精确**：`"gpt-4"`、`"claude-sonnet-4-5"`（与现状一致）
- **前缀/中段/后缀通配**：`"claude-*"`、`"gpt-*-turbo"`、`"*-4-5"`
- **全匹配**：`"*"`（作为 fallback）

### 2.2 Go 结构体（不变）

`config.ModelMapping.UserModel` 字段保持 `string`。无新增字段。

---

## 三、匹配算法

### 3.1 核心改动：`GetMappingForRouting`

`config/config.go:912` 当前实现改为两步：

```
GetMappingForRouting(keyID, userModel):

  // Step 1: 精确匹配（保留 SQL 索引）
  err := db.QueryRow(
    "SELECT ... FROM model_mappings WHERE server_key_id = ? AND user_model = ?",
    keyID, userModel,
  )
  if 命中:
    走 provider 校验，返回 mapping + provider

  // Step 2: 顺序扫描含通配的 mapping
  mappings := c.ServerKeys[idx].Mappings  // 内存切片，按配置顺序
  for m in mappings:
    if !strings.Contains(m.UserModel, "*"): continue
    matched, _ := path.Match(m.UserModel, userModel)
    if matched:
      走 provider 校验，返回 mapping + provider

  // Step 3: 未命中
  return nil, nil, ErrModelNotAllowed
```

**顺序保证**：`mappings` 切片由 `AddMapping` append 维护
（[config/config.go:959](config/config.go#L959)），等价于 DB 插入顺序，
等价于 UI 上用户配置的顺序。

**性能**：单 key mappings 一般 < 100 条；Step 2 是 O(N) 内存扫描，
最坏情况每个请求多遍历一次切片，可接受。

### 3.2 路径分隔符语义

使用 Go 标准库 `path.Match`，其语义：
- `*` 不匹配 `/`
- `?` 不匹配 `/`
- 方括号 `[abc]` 不匹配 `/`

我们只使用 `*`，等价于：
> `*` 匹配任意不含 `/` 的字符序列（包括空序列）。

这与当前模型名格式天然兼容（模型名不含 `/`），同时避免 `*` 滥用覆盖任意
路径式字符串。

### 3.3 优先级示例

key K 的 mappings（按顺序）：
```
1. gpt-4        → P-A 的 gpt-4-standard
2. gpt-*        → P-B 的 gpt-4-turbo
3. *            → P-C 的 fallback-model
```

| 请求 | 命中 | 结果 |
|---|---|---|
| `gpt-4` | #1 | P-A 的 gpt-4-standard |
| `gpt-4-turbo` | #2 | P-B 的 gpt-4-turbo |
| `claude-opus` | #3 | P-C 的 fallback-model |

**责任在用户**：若把顺序配成 `[*, gpt-4, gpt-*]`（不推荐），
`gpt-4` 请求会被 `*` 抢先匹配。这与「路由表叠加」心智模型一致。

### 3.4 provider 校验不变

匹配命中后走现有 `c.Providers[i].IsActive` 检查与 provider 缺失检测，
错误语义保持：
- 命中但 provider 不存在 → 500 `configured provider missing`
- 命中但 `IsActive=false` → 403 `model 'X' not supported (provider inactive)`
- 完全未匹配 → 403 `model 'X' not allowed for this key`

---

## 四、校验规则

### 4.1 Pattern 合法性校验

新加函数 `ValidateUserModelPattern(pattern string) error`：

| 输入 | 结果 |
|---|---|
| `"gpt-4"` | OK |
| `"*"` | OK（fallback） |
| `"claude-*"` | OK |
| `"*-4-5"` | OK |
| `"gpt-*-turbo"` | OK |
| `"claude-**"` | 400 `连续 '*' 不允许` |
| `"*foo*bar*"` | 400 `连续 '*' 不允许` |
| `"gpt-?4"` | 400 `不支持字符 '?'` |
| `"gpt-[abc]"` | 400 `不支持字符 '['` |
| `""` | 400 `required field missing`（已存在） |

实现：
1. 空字符串 → 报错（已存在）
2. `strings.Contains(pattern, "**")` → 拒绝
3. 用字符白名单扫描：`[a-zA-Z0-9._\-+/*]` 之外的字节 → 拒绝
   - 白名单覆盖：精确字符、`*`、`.`、`_`、`-`、`+`、`/`
   - 与现状模型名实际格式匹配（参考 Anthropic / OpenAI 命名）

### 4.2 同 key 冲突检测

新加函数 `DetectMappingConflict(keyID, candidate, excludeMappingID string) error`：

- 若 candidate 与已存在项都**不含 `*`** → 无需运行冲突扫描；DB `UNIQUE(server_key_id, user_model)` 已拦截精确重名
- 否则（含 `*` 之一出现）：扫描同 key 的所有 mappings，逐个测双向覆盖
  - candidate 可命中对方（candidate 更"窄"或同宽） → 报错 `pattern conflicts with existing mapping "<existing user_model>"`
  - 对方可命中 candidate（candidate 更"宽"或同宽） → 报错同上
- `excludeMappingID` 在 UpdateMapping 时传当前 mapping ID，跳过自身避免自冲突

**冲突矩阵**：

| 已有 | 新加 | 结果 |
|---|---|---|
| `gpt-4` | `gpt-*` | 409 冲突（双向覆盖） |
| `claude-*` | `claude-sonnet-*` | 409 冲突（已有规则覆盖新规则） |
| `gpt-*` 和 `*` | `*-4-5` | 409 冲突（`*` 覆盖新规则） |
| `gpt-4` | `gpt-4-turbo` | OK（互不覆盖） |
| `claude-*` | `gpt-*` | OK（互不覆盖） |

### 4.3 触发点

- `AddMapping`：INSERT 前先 `ValidateUserModelPattern`，再 `DetectMappingConflict(keyID, candidate, "")`
- `UpdateMapping`：同上，但 `excludeMappingID` 传当前 mapping ID，避免自我冲突

### 4.4 错误响应汇总

| 场景 | HTTP | 错误消息 |
|---|---|---|
| Pattern 非法（含 `**` 或非法字符） | 400 | `invalid pattern: <原因>` |
| 同 key 冲突 | 409 | `pattern conflicts with existing mapping "<existing user_model>"` |
| 精确重名（DB UNIQUE） | 409 | `duplicate user model name` |
| 字段为空 | 400 | `required field missing`（已存在） |
| provider 找不到 | 400 | `provider not found`（已存在） |

---

## 五、`/v1/models` 列表

### 5.1 `buildModelsListResponse` 改动

`proxy/proxy.go:50-82` 当前实现逐个把 `m.UserModel` 加入 `seen`。改为：

```go
for _, m := range mappings {
    if m.UserModel == "" {
        continue
    }
    if strings.Contains(m.UserModel, "*") {
        continue   // 跳过通配模式 —— 客户端不能把 "*" 当 id 调
    }
    seen[m.UserModel] = struct{}{}
}
```

### 5.2 测试下拉

`web/static/index.html` 的 `populateTestModelDropdown` 同样跳过含 `*` 的 mapping —
fallback 规则没有可测试的具体 provider_model 候选。

### 5.3 代理路径不变

`resolveRouteTarget` 返回 `(provider, providerModel)`，`providerModel`
是精确字符串（DB 字段不含 `*`），`requestBody["model"] = providerModel`
替换逻辑无须改动。

---

## 六、UI 改动

### 6.1 提示文案

`web/static/index.html:456` 区块说明：

```
原: 为每个用户侧模型名指定目标 provider 和实际模型名。
新: 为每个用户侧模型名指定目标 provider 和实际模型名。
    支持 * 通配（如 claude-* 匹配所有 claude- 开头的模型，
    单独 * 作为兜底）。匹配按配置顺序，第一个命中胜出。
```

### 6.2 通配角标

`addMappingRow`（`index.html:1015-1129`）渲染行时，若 `user_model`
字符串含 `*`，在 input 旁加小角标（`<span class="badge">通配</span>`），
帮助用户在表格里一眼区分普通规则和通配规则。

### 6.3 不变更

- 不变更行布局
- 不变更 provider / provider_model 下拉
- 不变更保存按钮、错误提示路径

---

## 七、API 契约

CRUD 端点契约不变：

| 端点 | body 字段 |
|---|---|
| `POST /api/server-keys/:id/mappings` | `{user_model, provider_id, provider_model}` |
| `PUT /api/server-keys/:id/mappings/:mapping_id` | 同上 |
| `DELETE /api/server-keys/:id/mappings/:mapping_id` | — |

仅错误码新增（pattern 非法 400、冲突 409）。客户端错误提示路径已存在。

---

## 八、测试计划（TDD 顺序）

1. `config.TestValidateUserModelPattern`
   - 合法：`*`、`claude-*`、`*-4-5`、`gpt-*-turbo`、`gpt-4`
   - 非法：`**`、`*foo*bar*`、`?`、`[abc]`、空串
2. `config.TestDetectMappingConflict`
   - 双向覆盖矩阵
3. `config.TestGetMappingForRouting_Wildcard`
   - 顺序优先 / fallback / 精确优先 / 多匹配顺序
4. `config.TestGetMappingForRouting_Pattern`
   - 各 glob 案例命中与未命中
5. `proxy.TestBuildModelsListResponse_SkipsWildcards`
   - 含 `*` 项不出现在列表
6. `proxy.TestResolveRouteTarget_Wildcard`
   - 端到端：路由命中通配规则
7. `web.TestAddKeyMapping_PatternValidation`
   - 400 / 409 错误响应
8. `config.TestMigrationBackcompat`
   - 已有数据无 `*` 仍走精确路径

---

## 九、迁移与部署

### DB 迁移

**无**。现有 schema 兼容；已有 mappings 原样保留，新规则按需添加。

### 前端兼容

替换 `web/static/index.html` 即可。无破坏性 API 变化。

### 后端重启

替换二进制后重启进程。旧 mappings 即时生效（无需重新配置）。

### 回退方案

- 删除含 `*` 的 mapping 或重命名为精确值
- 无需回滚代码（feature 不影响精确路径）

---

## 十、影响范围

修改文件：

| 文件 | 改动 |
|---|---|
| `config/config.go` | `GetMappingForRouting` 改两步；新增 `ValidateUserModelPattern`、`DetectMappingConflict`；`AddMapping`/`UpdateMapping` 增校验 |
| `config/config_test.go` | 上述测试 |
| `proxy/proxy.go` | `buildModelsListResponse` 跳过 `*` 项 |
| `proxy/proxy_test.go` | `/v1/models` 跳过 + 端到端 |
| `web/static/index.html` | 区块说明文案 + 通配角标 + 测试下拉过滤 |

未修改文件：`logger/`、`service/`、`update/`、`appdata/`、`main.go`、`history/`、
`stats/`、构建脚本。

---

## 十一、未决问题

无 — 全部确认完毕。