# SwitchAI API 文档

## 项目概述

SwitchAI 是一个 API 代理网关，支持：
- 多密钥管理
- AI API 格式转换（OpenAI ↔ Anthropic/Claude）
- 请求统计与历史记录
- 2FA 安全认证

## 代理转发 API

客户端请求通过 `/v1/*` 路径，由网关转发至配置的外部提供商。

### 认证

```http
Authorization: Bearer <server_key>
```

### 端点

#### Chat Completions (OpenAI 格式)

```http
POST /v1/chat/completions
Content-Type: application/json

{
  "model": "claude-3-5-sonnet-20241022",
  "messages": [
    {"role": "user", "content": "Hello"}
  ],
  "max_tokens": 1024,
  "stream": false
}
```

#### Messages (Anthropic/Claude 格式)

```http
POST /v1/messages
Content-Type: application/json
anthropic-version: 2023-06-01

{
  "model": "claude-3-5-sonnet-20241022",
  "messages": [
    {"role": "user", "content": "Hello"}
  ],
  "max_tokens": 1024
}
```

### 响应格式

流式响应返回 SSE 格式：

```http
data: {"id":"...","type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"},"usage":{...}}

data: {"id":"...","type":"message_stop","usage":{"input_tokens":10,"output_tokens":20}}
```

## 管理后台 API

基础路径：`/api`

### 认证

除以下端点外，所有 API 均需登录认证：
- `POST /api/login`
- `POST /api/totp/setup`
- `POST /api/totp/verify`
- `GET /api/totp/status`

登录后通过 Cookie (`switchai_auth`) 维持会话。

---

### 2FA 管理

#### 获取 TOTP 状态

```http
GET /api/totp/status
```

响应：
```json
{
  "enabled": true,
  "has_secret": true
}
```

#### 设置 TOTP（首次）

```http
POST /api/totp/setup
```

响应：
```json
{
  "secret": "JBSWY3DPEHPK3PXP",
  "otpauth": "otpauth://totp/SwitchAI:admin?secret=...&issuer=SwitchAI"
}
```

#### 验证 TOTP 并启用

```http
POST /api/totp/verify
Content-Type: application/json

{
  "code": "123456"
}
```

响应（成功后自动登录）：
```json
{
  "message": "2FA绑定成功"
}
```

#### 登录

```http
POST /api/login
Content-Type: application/json

{
  "code": "123456"
}
```

#### 退出登录

```http
POST /api/logout
```

---

### 服务器密钥管理

#### 获取所有密钥

```http
GET /api/server-keys
```

响应：
```json
{
  "server_keys": [
    {
      "id": "uuid",
      "key": "sk-xxxxxxxxxxxx",
      "remark": "备注",
      "is_enabled": true,
      "created_at": "2024-01-01T00:00:00Z",
      "daily_req_limit": 1000,
      "total_req_limit": 0,
      "daily_cost_limit": 10.0,
      "total_cost_limit": 0
    }
  ]
}
```

#### 生成新密钥

```http
POST /api/server-keys/generate
```

响应：
```json
{
  "server_key": "sk-xxxxxxxxxxxx",
  "message": "密钥已生成"
}
```

#### 添加密钥

```http
POST /api/server-keys
Content-Type: application/json

{
  "remark": "备注",
  "daily_req_limit": 1000,
  "total_req_limit": 0,
  "daily_cost_limit": 10.0,
  "total_cost_limit": 0
}
```

#### 更新密钥

```http
PUT /api/server-keys/:id
Content-Type: application/json

{
  "remark": "新备注",
  "is_enabled": true,
  "daily_req_limit": 2000,
  "daily_cost_limit": 20.0
}
```

#### 删除密钥

```http
DELETE /api/server-keys/:id
```

#### 获取密钥统计

```http
GET /api/server-keys/:id/stats
```

响应：
```json
{
  "key_id": "uuid",
  "input_tokens": 1000,
  "output_tokens": 2000,
  "total_tokens": 3000,
  "ip_addresses": ["192.168.1.1"],
  "request_count": 50
}
```

#### 测试密钥

```http
POST /api/server-keys/:id/test
Content-Type: application/json

{
  "provider_type": "openai"
}
```

响应：
```json
{
  "success": true,
  "status": 200,
  "aiReply": "Hi!",
  "providerName": "MiniMax"
}
```

---

### 提供商管理

#### 获取所有提供商

```http
GET /api/providers
```

响应：
```json
{
  "providers": [
    {
      "id": "uuid",
      "name": "MiniMax",
      "base_url": "https://api.minimaxi.com",
      "api_key": "",
      "model": "abab6.5s-chat",
      "is_active": true,
      "is_openai_format": false
    }
  ],
  "active_provider": "uuid"
}
```

#### 添加提供商

```http
POST /api/providers
Content-Type: application/json

{
  "name": "MiniMax",
  "base_url": "https://api.minimaxi.com",
  "api_key": "your-api-key",
  "model": "abab6.5s-chat",
  "is_openai_format": false
}
```

#### 更新提供商

```http
PUT /api/providers/:id
Content-Type: application/json

{
  "name": "MiniMax",
  "base_url": "https://api.minimaxi.com",
  "api_key": "new-api-key",
  "model": "abab6.5s-chat",
  "is_active": true,
  "is_openai_format": false
}
```

#### 删除提供商

```http
DELETE /api/providers/:id
```

#### 激活提供商

```http
POST /api/providers/:id/activate
```

#### 测试提供商连接

```http
POST /api/providers/:id/test
```

响应：
```json
{
  "success": true,
  "status": 200,
  "message": "Connection successful",
  "response": "...",
  "aiReply": "Hi!"
}
```

---

### 统计信息

#### 获取总体统计

```http
GET /api/stats
```

响应：
```json
{
  "total_requests": 1000,
  "total_input_tokens": 50000,
  "total_output_tokens": 100000,
  "total_cost": 1.5,
  "active_clients": 5
}
```

#### 获取每日统计

```http
GET /api/stats/daily
```

响应：
```json
{
  "today": {
    "requests": 100,
    "input_tokens": 5000,
    "output_tokens": 10000,
    "cost": 0.15
  },
  "daily_history": [
    {
      "date": "2024-01-01",
      "requests": 100,
      "input_tokens": 5000,
      "output_tokens": 10000,
      "cost": 0.15
    }
  ]
}
```

#### 重置所有统计

```http
POST /api/stats/reset
```

#### 重置提供商统计

```http
POST /api/stats/reset/:provider_id
```

---

### 请求历史

#### 获取历史记录

```http
GET /api/history?page=1&page_size=20
```

响应：
```json
{
  "records": [
    {
      "id": "uuid",
      "timestamp": "2024-01-01T00:00:00Z",
      "method": "POST",
      "path": "/v1/messages",
      "client_ip": "192.168.1.1",
      "provider": "MiniMax",
      "model": "abab6.5s-chat",
      "status_code": 200,
      "duration": 1500,
      "input_tokens": 100,
      "output_tokens": 200,
      "cost": 0.003
    }
  ],
  "total": 100,
  "page": 1,
  "page_size": 20
}
```

#### 获取历史详情

```http
GET /api/history/:id
```

---

### WebSocket

#### 实时统计

```http
GET /api/ws
```

服务器主动推送统计数据更新。

#### 实时历史

```http
GET /api/ws/history
```

服务器主动推送新的请求记录。

---

## 外部 API 提供商配置

项目本身不硬编码外部 API 地址，而是通过管理后台配置提供商。常见的提供商配置示例：

### MiniMax

| 字段 | 值 |
|------|-----|
| Base URL | `https://api.minimaxi.com` |
| API Key | 您的 MiniMax API Key |
| 模型 | `abab6.5s-chat` |
| 格式 | Anthropic/Claude |

### Anthropic

| 字段 | 值 |
|------|-----|
| Base URL | `https://api.anthropic.com` |
| API Key | 您的 Anthropic API Key |
| 模型 | `claude-3-5-sonnet-20241022` |
| 格式 | Anthropic/Claude |

### OpenAI

| 字段 | 值 |
|------|-----|
| Base URL | `https://api.openai.com` |
| API Key | 您的 OpenAI API Key |
| 模型 | `gpt-4o` |
| 格式 | OpenAI |

---

## 错误码

| HTTP 状态码 | 说明 |
|-------------|------|
| 400 | 请求参数错误 |
| 401 | 未登录或认证失败 |
| 403 | 密钥被禁用或超出限额 |
| 404 | 资源不存在 |
| 429 | 请求过于频繁（外部 API） |
| 500 | 服务器内部错误 |
| 502 | 外部 API 请求失败 |
| 503 | 没有可用的提供商配置 |
