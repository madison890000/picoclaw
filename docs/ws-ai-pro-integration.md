# ws_ai_pro Channel - Electron Integration Guide

PicoClaw 通过 `ws_ai_pro` channel 与 Electron 客户端对接，使用 WebSocket 通讯。
本文档描述协议格式、JWT 透传认证机制，以及与后端 API 代理的对接方式。

---

## Architecture

```
Electron App                  PicoClaw                    API Proxy (ws-ai-pro-backend)       OpenAI
    │                            │                                │                              │
    │── WebSocket ──────────────▶│                                │                              │
    │   ClientMessage{token}     │                                │                              │
    │                            │── HTTP POST ─────────────────▶│                              │
    │                            │   Authorization: Bearer <jwt>  │                              │
    │                            │                                │── Authorization: Bearer <sk>─▶│
    │                            │                                │   (proxy injects real key)    │
    │◀── chunk/done ────────────│◀── SSE / JSON ────────────────│◀─────────────────────────────│
```

**关键设计**：PicoClaw 不保存任何 API Key，也不负责 token 的获取和刷新。
Electron 客户端在每条消息中携带最新的 JWT token，PicoClaw 原样透传到上游 API 代理。

---

## WebSocket Protocol

### Connection

```
ws://<picoclaw-host>:<port>/ws
```

默认端口由 `config.json` 中 `ws_ai_pro.port` 配置。

### ClientMessage (Electron → PicoClaw)

```json
{
  "type": "chat",
  "id": "req-001",
  "session": "session-abc",
  "content": "Hello, help me translate this",
  "no_tools": false,
  "token": "eyJhbGciOiJIUzI1NiIs..."
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `type` | string | yes | `"chat"` or `"ping"` |
| `id` | string | recommended | Request ID, echoed back for correlation |
| `session` | string | recommended | Chat session ID, maps to PicoClaw session history |
| `content` | string | yes (chat) | User message content |
| `no_tools` | bool | no | If true, disable tool calls (pure LLM response) |
| `token` | string | yes (jwt) | Bearer token from Electron (JWT from backend) |

### ServerMessage (PicoClaw → Electron)

```json
{ "type": "chunk", "id": "req-001", "content": "Hello" }
{ "type": "done",  "id": "req-001" }
```

| Type | Description |
|------|-------------|
| `chunk` | Streaming LLM token fragment |
| `done` | Response complete |
| `reply` | Full response (non-streaming or broadcast) |
| `pong` | Response to `ping` |
| `error` | Error occurred, see `error` field |

### Keep-Alive

```json
// Client sends:
{ "type": "ping", "id": "p1" }

// Server responds:
{ "type": "pong", "id": "p1" }
```

---

## JWT Token Flow

### Overview

1. Electron 通过后端 API (`POST /auth/login`) 获取 `access_token`
2. Electron 负责 token 刷新 (`POST /auth/refresh`)
3. 每次发送 chat 消息时，Electron 将最新 token 放入 `ClientMessage.token` 字段
4. PicoClaw 将 token 注入 request context，透传到上游 API 请求的 `Authorization: Bearer` header
5. API 代理验证 JWT 并注入真正的 OpenAI API Key

### Token Lifecycle (Electron Side)

```
Login ──▶ access_token (15min TTL)
              │
              ▼
         Send to PicoClaw via WebSocket
              │
              ▼ (approaching expiry)
         POST /auth/refresh ──▶ new access_token
              │
              ▼
         Next message carries new token
```

Electron 端需要实现：
- 登录获取初始 token
- 定时或按需刷新 token（建议在过期前 2 分钟刷新）
- 每条 WebSocket 消息携带最新 token

### Error Handling

当 token 过期或无效时，API 代理会返回 `401`，PicoClaw 会将错误透传回 Electron：

```json
{ "type": "error", "id": "req-001", "error": "auth: auth_method=jwt but no bearer token in context" }
```

或来自上游代理的错误：

```json
{ "type": "error", "id": "req-001", "error": "API request failed: Status: 401" }
```

Electron 收到 401 类错误后应触发 token 刷新并重试。

---

## PicoClaw Configuration

### config.json

```json
{
  "ws_ai_pro": {
    "enabled": true,
    "host": "127.0.0.1",
    "port": 8765
  },
  "model_list": [
    {
      "model": "openai/gpt-5.4",
      "auth_method": "jwt",
      "api_base": "https://api.whatsappaipro.com/v1"
    }
  ]
}
```

### model_list Fields

| Field | Required | Description |
|-------|----------|-------------|
| `model` | yes | `openai/<model-name>` format |
| `auth_method` | yes | Must be `"jwt"` for proxy mode |
| `api_base` | yes | API proxy URL (your backend's AI gateway base) |
| `proxy` | no | HTTP proxy for outbound requests |
| `max_tokens_field` | no | Override max tokens field name |
| `request_timeout` | no | Request timeout in seconds |

> **Important**: `auth_method: "jwt"` 模式下不需要配置 `api_key`。
> Token 由 Electron 客户端在每条消息中动态提供。

---

## Data Flow (Internal)

PicoClaw 内部数据流：

```
ClientMessage.token
    │
    ▼
ws_ai_pro handler
    │  stores in metadata["bearer_token"]
    ▼
InboundMessage.Metadata
    │
    ▼
agent/loop.go processMessage()
    │  injects via openaicompat.WithBearerToken(ctx, token)
    ▼
context.Context
    │
    ▼
provider.Chat(ctx, ...)
    │
    ▼
openai_compat setAuthHeader(req)
    │  reads BearerTokenFromContext(req.Context())
    ▼
HTTP Request: Authorization: Bearer <jwt>
    │
    ▼
API Proxy (ws-ai-pro-backend)
```

---

## Backend API Proxy Endpoints

PicoClaw 的 `api_base` 指向后端代理，以下是 PicoClaw 会调用的端点：

### POST /v1/chat/completions (OpenAI-compatible proxy)

后端新增的 OpenAI 兼容反向代理端点。PicoClaw 发送标准 OpenAI 格式请求，
后端原样转发到 OpenAI 并原样返回响应（不做格式转换）。

```json
{
  "model": "gpt-5.4",
  "messages": [{"role": "user", "content": "Hello"}],
  "stream": true
}
```

Header:
```
Authorization: Bearer <jwt-from-electron>
Content-Type: application/json
```

后端处理流程：
1. JWT 验证（从 `Authorization` header 提取，复用 auth middleware）
2. 速率限制（复用 chat rate: 30/s, burst 10）
3. 配额检查（月度 token 配额）
4. 审计日志（feature="proxy"）
5. 替换 Authorization header 为真正的 OpenAI API Key（`OPENAI_API_KEY`）
6. 透传请求到 `OPENAI_BASE_URL/chat/completions`
7. 原样返回 OpenAI 响应（支持 JSON 和 SSE streaming）

> **注意**: 这个端点与后端已有的 `/v1/ai/chat` 不同。
> `/v1/ai/chat` 做格式转换（自定义 SSE 事件），而 `/v1/chat/completions` 是纯透传。

### POST /v1/ai/chat (原有端点)

原有的 AI Gateway 端点，使用自定义 SSE 事件格式（`stream_start`/`delta`/`stream_end`）。
Electron 前端直接调用此端点；PicoClaw 走 `/v1/chat/completions` 透传端点。

---

## Electron Integration Checklist

- [ ] 实现登录流程 (`POST /auth/login`) 获取 access_token
- [ ] 实现 token 刷新机制 (`POST /auth/refresh`)，建议过期前 2 分钟刷新
- [ ] WebSocket 连接到 PicoClaw (`ws://localhost:8765/ws`)
- [ ] 每条 chat 消息携带 `token` 字段
- [ ] 处理 `chunk` / `done` / `error` 消息类型
- [ ] 收到 401 错误时触发 token 刷新并重试
- [ ] 实现 `ping` / `pong` 心跳保活

### Electron Example (TypeScript)

```typescript
interface ClientMessage {
  type: 'chat' | 'ping';
  id?: string;
  session?: string;
  content?: string;
  no_tools?: boolean;
  token?: string;
}

interface ServerMessage {
  type: 'chunk' | 'done' | 'reply' | 'pong' | 'error';
  id?: string;
  content?: string;
  error?: string;
}

// Send a chat message with JWT token
function sendChat(ws: WebSocket, content: string, token: string) {
  const msg: ClientMessage = {
    type: 'chat',
    id: crypto.randomUUID(),
    session: currentSessionId,
    content,
    token,  // JWT from backend
  };
  ws.send(JSON.stringify(msg));
}
```
