# SwitchAI 项目架构与逻辑分析

## 项目概述

SwitchAI 是一个本地 Claude API 聚合网关服务，支持多提供商管理、API格式转换、Token统计、请求历史记录等功能。

---

## 系统架构图

```
┌─────────────────────────────────────────────────────────────────────────┐
│                          SwitchAI Gateway                                │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  ┌──────────────┐     ┌──────────────┐     ┌──────────────────────┐    │
│  │   Web UI     │     │  HTTP API    │     │   Claude Proxy       │    │
│  │  (HTML/JS)  │     │  (Gin Routes)│     │   /v1/*             │    │
│  └──────┬───────┘     └──────┬───────┘     └──────────┬───────────┘    │
│         │                    │                         │                │
│         └────────────────────┼─────────────────────────┘                │
│                              │                                          │
│                    ┌─────────▼─────────┐                               │
│                    │   Gin Engine      │                               │
│                    │  (HTTP Router)   │                               │
│                    └─────────┬─────────┘                               │
│                              │                                          │
│         ┌────────────────────┼────────────────────┐                     │
│         │                    │                    │                     │
│  ┌──────▼──────┐    ┌───────▼──────┐   ┌───────▼───────┐           │
│  │  Auth       │    │  Middleware   │   │  Proxy        │           │
│  │  (2FA/TOTP) │    │  (Logger)     │   │  Handler      │           │
│  └─────────────┘    └──────────────┘   └───────┬───────┘           │
│                                                │                      │
└────────────────────────────────────────────────┼──────────────────────┘
                                                 │
                              ┌──────────────────┼──────────────────┐
                              │                  │                  │
                    ┌─────────▼─────────┐ ┌─────▼─────────┐ ┌──────▼──────┐
                    │  Provider 1       │ │  Provider 2   │ │  Provider N │
                    │  (Claude/OpenAI)  │ │  (Claude)     │ │  (OpenAI)   │
                    └───────────────────┘ └───────────────┘ └─────────────┘
```

---

## 核心模块分析

### 1. 配置管理 (`config/`)

**职责**：管理提供商配置、服务器密钥、2FA认证、会话令牌

**核心数据结构**：
```go
// Provider - API提供商配置
type Provider struct {
    ID             string  // UUID标识
    Name           string  // 显示名称
    BaseURL        string  // API基础地址
    APIKey         string  // API密钥
    Model          string  // 默认模型
    IsActive       bool    // 是否激活
    IsOpenAIFormat bool    // 是否OpenAI格式(否则为Anthropic格式)
}

// ServerKey - 服务器端密钥
type ServerKey struct {
    ID              string  // UUID标识
    Key             string  // sk-xxxx 格式密钥
    DailyReqLimit   int     // 每日请求限额(0=不限)
    TotalReqLimit   int     // 总请求限额
    DailyCostLimit  float64 // 每日费用限额
    TotalCostLimit  float64 // 总费用限额
}

// Config - 主配置
type Config struct {
    Providers       []Provider
    ServerKeys     []ServerKey
    TOTPSecret     string  // 2FA密钥
    SessionTokens  []SessionTokenEntry // 多端登录会话
}
```

**存储**：SQLite数据库 (`config.db`)

**关键逻辑**：
- 多会话令牌支持：允许多设备同时登录
- 格式匹配：根据请求格式(Anthropic/OpenAI)选择对应格式的提供商

---

### 2. 代理转发 (`proxy/`)

**职责**：接收Claude API请求、转发到配置的提供商、处理格式转换

**请求流程**：
```
1. 接收 /v1/* 请求
2. 验证 Authorization 头(服务器密钥)
3. 检查密钥限额(每日/总请求数、费用)
4. 检测请求格式(Anthropic/OpenAI)
5. 选择匹配的提供商
6. 必要时进行格式转换
7. 转发请求到目标提供商(带重试机制)
8. 处理响应(解压、格式转换)
9. 记录统计和历史
```

**格式转换**：
| 请求格式 | 提供商格式 | 转换方向 |
|---------|-----------|---------|
| Anthropic | OpenAI | Claude→OpenAI请求, OpenAI→Claude响应 |
| OpenAI | Anthropic | OpenAI→Claude请求, Claude→OpenAI响应 |

**重试机制**：
- 触发条件：500错误、429限流、529不可用
- 检查错误类型：`overloaded_error`、`rate_limit_error`
- 检查错误消息关键词：`try again`、`high traffic`、`overloaded`
- 重试次数：3次，递增延迟

---

### 3. 统计模块 (`stats/`)

**职责**：记录Token使用量、费用统计、密钥限额检查

**数据表**：
```sql
-- 详细使用记录
CREATE TABLE usage_records (
    provider_id, model, input_tokens, output_tokens,
    cost, duration_ms, timestamp, key_id, client_ip
)

-- 提供商维度统计
CREATE TABLE provider_stats (
    provider_id, input_tokens, output_tokens, total_cost
)

-- 密钥维度统计
CREATE TABLE key_stats (
    key_id, input_tokens, output_tokens, total_cost, ip_addresses
)

-- 密钥每日统计(用于限额检查)
CREATE TABLE key_daily_stats (
    key_id, date, request_count, total_cost
)
```

**限额检查逻辑**：
```go
func CheckKeyLimit(keyID, dailyReqLimit, totalReqLimit,
                   dailyCostLimit, totalCostLimit float64) (bool, string) {
    usage := GetKeyUsage(keyID)

    if dailyReqLimit > 0 && usage.DailyReqCount >= dailyReqLimit {
        return false, "每日请求次数限额已用尽"
    }
    if totalReqLimit > 0 && usage.TotalReqCount >= totalReqLimit {
        return false, "总请求次数限额已用尽"
    }
    if dailyCostLimit > 0 && usage.DailyCost >= dailyCostLimit {
        return false, "每日花费限额已用尽"
    }
    if totalCostLimit > 0 && usage.TotalCost >= totalCostLimit {
        return false, "总花费限额已用尽"
    }
    return true, ""
}
```

---

### 4. 历史记录 (`history/`)

**职责**：持久化存储API请求/响应历史

**特性**：
- SQLite存储 (`history.db`)
- 最多保留1000条记录(自动清理旧数据)
- 首页缓存20条记录，加速展示
- 通过WebSocket实时推送新记录

**数据结构**：
```go
type RequestRecord struct {
    ID, Timestamp, Method, Path
    ClientIP, KeyID, Provider, Model
    StatusCode, Duration
    RequestBody, ResponseBody  // 完整请求/响应体
    InputTokens, OutputTokens, TotalTokens, Cost
}
```

---

### 5. 日志系统 (`logger/`)

**职责**：文件日志记录，支持按日期轮转

**特性**：
- 双日志输出：info.log + error.log
- 自动按小时轮转：`info_2026-01-02_15.log`
- 保留3天自动清理
- 包含微秒级时间戳

---

### 6. Web界面 (`web/`)

**职责**：提供管理UI和REST API

**页面**：
| 路由 | 功能 |
|-----|------|
| `/` | 首页-提供商管理 |
| `/log.html` | Token统计 |
| `/history.html` | 请求历史 |

**API端点**：
```
认证相关:
POST /api/login         - 登录
POST /api/logout        - 登出
POST /api/totp/setup    - 设置2FA
POST /api/totp/verify   - 验证2FA

提供商管理:
GET/POST /api/providers
PUT/DELETE /api/providers/:id
POST /api/providers/:id/activate
POST /api/providers/:id/test

密钥管理:
GET/POST /api/server-keys
PUT/DELETE /api/server-keys/:id
POST /api/server-keys/generate
GET /api/server-keys/:id/stats

统计:
GET /api/stats
GET /api/stats/daily
POST /api/stats/reset

历史:
GET /api/history?page=1&page_size=20
GET /api/history/:id

WebSocket:
GET /api/ws        - 实时统计推送
GET /api/ws/history - 实时历史推送
```

**认证机制**：
- Cookie-based会话认证
- TOTP 2FA二次验证
- 多设备登录支持(独立session token)
- `-skip`参数跳过认证(内网部署)

---

### 7. 自动更新 (`update/`)

**职责**：检测并自动安装新版本

**流程**：
```
1. 服务启动时检测是否以服务模式运行
2. 如果是服务模式，启动AutoUpdater
3. 每小时检查GitHub Releases
4. 发现新版本时自动下载
5. 下载完成后替换二进制文件
6. 重启服务生效
```

---

### 8. 服务安装 (`service/`)

**职责**：将程序注册为系统服务

**Windows**：
- 安装路径：`C:\Program Files\SwitchAI`
- 使用`sc`命令管理
- 配置故障恢复：5秒后自动重启

**Linux**：
- 二进制路径：`/usr/local/bin/switchai`
- Systemd服务：`/etc/systemd/system/switchai.service`
- 自动启动：`systemctl enable`

---

## 数据流分析

### API请求完整流程

```
┌────────┐     ┌─────────┐     ┌─────────┐     ┌──────────┐     ┌────────────┐
│Client  │────▶│  Gin    │────▶│Auth MW  │────▶│Proxy     │────▶│Provider   │
│        │     │Router   │     │         │     │Handler   │     │(Claude/API)│
└────────┘     └─────────┘     └─────────┘     └────┬─────┘     └────────────┘
                                                     │
                          ┌──────────────────────────┼──────────────┐
                          │                          │              │
                          ▼                          ▼              ▼
                    ┌──────────┐              ┌──────────┐    ┌──────────┐
                    │  Stats   │              │  History │    │  Logger  │
                    │(SQLite)  │              │(SQLite)  │    │ (File)   │
                    └──────────┘              └──────────┘    └──────────┘
                          │
                          ▼
                    ┌──────────┐
                    │WebSocket │
                    │Broadcast │
                    └──────────┘
```

---

## 启动流程

```
main()
  │
  ├─ Parse CLI flags (-p, -install, -uninstall, -skip, -reset)
  │
  ├─ appdata.Init() ─── 创建 .switchai 数据目录
  │
  ├─ logger.Init() ─── 初始化日志系统
  │
  ├─ config.Init() ─── 初始化SQLite，加载配置
  │
  ├─ stats.Init() ─── 初始化统计数据库
  │
  ├─ history.Init() ── 初始化历史数据库
  │
  ├─ 创建Gin Engine
  │    │
  │    ├─ 恢复中间件
  │    ├─ 请求日志中间件
  │    ├─ CORS中间件
  │    │
  │    ├─ web.RegisterRoutes() ─── 管理界面
  │    └─ proxy.RegisterRoutes() ── /v1/* 代理
  │
  ├─ 如果服务模式：启动自动更新器
  │
  └─ 启动HTTP服务器，监听端口
```

---

## 数据持久化

**存储位置**：`./.switchai/` 目录

| 文件/目录 | 内容 |
|----------|------|
| `config.db` | 提供商配置、密钥、2FA、会话 |
| `stats.db` | 使用统计、每日统计 |
| `history.db` | 请求历史记录 |
| `logs/` | 应用日志(info/error) |

---

## 关键设计决策

### 1. 双格式支持
- 同时支持Anthropic和OpenAI API格式
- 自动检测请求格式，选择匹配的提供商
- 支持跨格式代理和响应转换

### 2. 密钥限额体系
- 四维限额：每日请求、总请求、每日费用、总费用
- 基于密钥的隔离统计
- 支持多提供商共享限额

### 3. 多端登录
- 独立的session token存储
- 单设备登出不影响其他设备
- 2FA保护

### 4. 透明代理
- 请求/响应格式自动转换
- 模型名称自动替换
- 流式响应完整支持

### 5. 实时推送
- WebSocket实时推送统计和历史
- 首页缓存加速加载
- 后台数据库持久化

---

## 依赖技术栈

| 组件 | 技术 |
|-----|------|
| Web框架 | Gin (github.com/gin-gonic/gin) |
| 数据库 | SQLite (modernc.org/sqlite) |
| WebSocket | gorilla/websocket |
| 2FA | pquerna/otp (TOTP) |
| UUID | google/uuid |
| 压缩 | brotli, gzip, deflate, zlib |

---

## 配置文件参考

**providers.json 示例**：
```json
{
  "providers": [
    {
      "id": "uuid",
      "name": "Provider Name",
      "base_url": "https://api.example.com",
      "api_key": "sk-xxxx",
      "model": "claude-sonnet-4-6",
      "is_openai_format": false,
      "is_active": true
    }
  ],
  "active_provider": "uuid"
}
```

---

## API调用示例

### 配置VSCode Claude Code

```json
"claudeCode.environmentVariables": [
    {"name": "ANTHROPIC_AUTH_TOKEN", "value": "any-value"},
    {"name": "ANTHROPIC_BASE_URL", "value": "http://localhost:7777"},
    {"name": "ANTHROPIC_MODEL", "value": "claude-sonnet-4-6"}
]
```

### 密钥测试

```bash
# 生成新密钥
curl -X POST http://localhost:7777/api/server-keys/generate

# 测试密钥(需先登录获取cookie)
curl -X POST http://localhost:7777/api/server-keys/{id}/test \
  -H "Cookie: switchai_auth=xxx" \
  -d '{"provider_type": "anthropic"}'
```

---

## 故障排查

| 问题 | 排查方向 |
|-----|---------|
| 401 Unauthorized | 检查服务器密钥是否正确，密钥是否启用 |
| 429 Rate Limit | 等待后重试，或检查密钥限额设置 |
| 格式错误 | 确认提供商格式设置正确 |
| WebSocket断开 | 检查网络连接，确认服务正常运行 |

---

*文档生成时间：2026-06-03*
