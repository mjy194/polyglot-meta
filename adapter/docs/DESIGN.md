# 🎯 UiPath Adapter 设计文档

## 📋 项目概述

**UiPath Adapter** 是 Polyglot 的第一个真实后端 Adapter，负责：
1. 接收 Core 的 Universal 格式请求
2. 认证到 UiPath Cloud
3. 转换并调用 UiPath API
4. 返回 Universal 格式响应

---

## 🏗️ 项目结构

```
uipath_adapter/
├── cmd/
│   └── adapter/
│       └── main.go              # 主程序入口
│
├── internal/
│   ├── auth/
│   │   ├── oauth.go             # OAuth 2.0 PKCE 认证
│   │   ├── token_cache.go       # Token 缓存
│   │   └── renew.go             # 组织续费处理
│   │
│   ├── client/
│   │   ├── uipath.go            # UiPath API 客户端
│   │   ├── stream.go            # 流式响应处理
│   │   └── errors.go            # 错误处理
│   │
│   ├── converter/
│   │   ├── universal_to_uipath.go   # Universal → UiPath
│   │   └── uipath_to_universal.go   # UiPath → Universal
│   │
│   ├── config/
│   │   └── config.go            # 配置管理
│   │
│   └── server/
│       └── grpc_server.go       # gRPC 服务器实现
│
├── proto/
│   └── adapter.proto            # gRPC 协议定义（从 Core 复制）
│
├── configs/
│   └── config.yaml              # 配置文件
│
├── go.mod
├── go.sum
└── README.md
```

---

## 🔑 核心组件

### 1. gRPC Server

**文件**: `internal/server/grpc_server.go`

**职责**：
- 实现 `AdapterService` 接口
- 接收 `UniversalRequest`
- 返回 `UniversalResponse` 流

**接口**：
```go
type UiPathAdapter struct {
    auth      *auth.Manager
    client    *client.UiPath
    converter *converter.Converter
    config    *config.Config
}

func (a *UiPathAdapter) GetMetadata(ctx, req) (*pb.AdapterMetadata, error)
func (a *UiPathAdapter) HealthCheck(ctx, req) (*pb.HealthCheckResponse, error)
func (a *UiPathAdapter) ProcessRequest(req, stream) error
func (a *UiPathAdapter) CancelRequest(ctx, req) (*pb.CancelRequestResponse, error)
```

---

### 2. OAuth Manager

**文件**: `internal/auth/oauth.go`

**职责**：
- OAuth 2.0 PKCE 认证流程
- Token 获取和刷新
- Token 缓存管理

**核心方法**：
```go
type Manager struct {
    clientID     string
    clientSecret string
    cache        *TokenCache
    db           *sql.DB
}

// GetAccessToken 获取有效的 access token
func (m *Manager) GetAccessToken() (string, error)

// Authenticate 执行完整的 OAuth 流程
func (m *Manager) Authenticate() (*Token, error)

// RefreshToken 刷新 token
func (m *Manager) RefreshToken(refreshToken string) (*Token, error)

// RenewOrganization 处理组织续费
func (m *Manager) RenewOrganization() error
```

**设计要点**：
- ✅ 从 Rust 版本移植逻辑
- ✅ 使用 SQLite 缓存 Token
- ✅ 自动刷新过期 Token
- ✅ 处理 `licensing_not_entitled` 错误

---

### 3. UiPath Client

**文件**: `internal/client/uipath.go`

**职责**：
- 调用 UiPath Cloud API
- 处理流式响应
- 错误重试

**UiPath API 格式**：
```go
// UiPath 请求格式
type UiPathRequest struct {
    Messages    []Message `json:"messages"`
    MaxTokens   int       `json:"maxTokens,omitempty"`
    Temperature float64   `json:"temperature,omitempty"`
    Stream      bool      `json:"stream,omitempty"`
}

type Message struct {
    Role    string `json:"role"`    // "user" | "assistant"
    Content string `json:"content"`
}
```

**核心方法**：
```go
type UiPath struct {
    baseURL    string
    httpClient *http.Client
    auth       *auth.Manager
}

// SendMessage 发送消息（非流式）
func (c *UiPath) SendMessage(ctx, req) (*Response, error)

// StreamMessage 发送消息（流式）
func (c *UiPath) StreamMessage(ctx, req) (<-chan Event, error)
```

---

### 4. Converter

**文件**: `internal/converter/universal_to_uipath.go`

**职责**：
- Universal → UiPath 格式转换
- UiPath → Universal 格式转换

**转换逻辑**：

#### Universal → UiPath

```go
func UniversalToUiPath(req *pb.UniversalRequest) (*UiPathRequest, error) {
    uipathReq := &UiPathRequest{
        MaxTokens:   int(req.Config.MaxTokens),
        Temperature: req.Config.Temperature,
        Stream:      req.Config.Stream,
    }
    
    // 转换消息
    for _, msg := range req.Messages {
        uipathMsg := Message{
            Role: convertRole(msg.Role),
        }
        
        // 提取文本内容
        for _, part := range msg.Content {
            if textPart := part.GetText(); textPart != nil {
                uipathMsg.Content += textPart.Text
            }
        }
        
        uipathReq.Messages = append(uipathReq.Messages, uipathMsg)
    }
    
    return uipathReq, nil
}

func convertRole(role pb.Message_Role) string {
    switch role {
    case pb.Message_USER:
        return "user"
    case pb.Message_ASSISTANT:
        return "assistant"
    default:
        return "user"
    }
}
```

#### UiPath → Universal

```go
func UiPathToUniversal(event *UiPathEvent) (*pb.UniversalResponse, error) {
    switch event.Type {
    case "content_block_delta":
        return &pb.UniversalResponse{
            Response: &pb.UniversalResponse_Chunk{
                Chunk: &pb.ContentChunk{
                    Text: event.Delta.Text,
                },
            },
        }, nil
        
    case "message_stop":
        return &pb.UniversalResponse{
            Response: &pb.UniversalResponse_Completion{
                Completion: &pb.CompletionInfo{
                    FinishReason: "stop",
                    InputTokens:  uint32(event.Usage.InputTokens),
                    OutputTokens: uint32(event.Usage.OutputTokens),
                },
            },
        }, nil
    }
    
    return nil, nil
}
```

---

## 🔄 完整请求流程

### 场景：处理 Universal 请求

```
1. Core 发送 gRPC 请求
   ProcessRequest(UniversalRequest)

2. UiPath Adapter 接收
   ProcessRequest(req, stream) {
   
3. 获取 Access Token
     token := auth.GetAccessToken()
     
4. 转换为 UiPath 格式
     uipathReq := converter.UniversalToUiPath(req)
     
5. 调用 UiPath API
     eventChan := client.StreamMessage(ctx, uipathReq)
     
6. 转换并流式返回
     for event := range eventChan {
         univResp := converter.UiPathToUniversal(event)
         stream.Send(univResp)
     }
   }
```

---

## 🔐 认证流程

### OAuth 2.0 PKCE

**完整流程**：

```
1. 检查缓存
   token := cache.Get()
   if token != nil && !token.IsExpired() {
       return token.AccessToken
   }

2. 尝试刷新
   if refreshToken := cache.GetRefreshToken(); refreshToken != "" {
       return RefreshToken(refreshToken)
   }

3. 重新认证
   a. 生成 code_verifier 和 code_challenge
      verifier := generateCodeVerifier()
      challenge := sha256(verifier)
   
   b. 获取授权码
      authURL := buildAuthURL(challenge)
      code := getUserAuthorization(authURL)
   
   c. 交换 access_token
      token := exchangeToken(code, verifier)
   
   d. 缓存 token
      cache.Set(token)
   
   return token.AccessToken
```

### Token 缓存结构

**SQLite Schema**：
```sql
CREATE TABLE tokens (
    id INTEGER PRIMARY KEY,
    access_token TEXT NOT NULL,
    refresh_token TEXT,
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);
```

---

## ⚠️ 错误处理

### 关键错误场景

#### 1. licensing_not_entitled

**错误**：组织需要续费

**处理**：
```go
if err.Code == "licensing_not_entitled" {
    // 触发续费
    if err := auth.RenewOrganization(); err != nil {
        return err
    }
    
    // 重试请求
    return client.SendMessage(ctx, req)
}
```

#### 2. Token 过期

**错误**：401 Unauthorized

**处理**：
```go
if err.StatusCode == 401 {
    // 刷新 token
    token, err := auth.RefreshToken()
    if err != nil {
        // 重新认证
        token, err = auth.Authenticate()
    }
    
    // 重试请求
    return client.SendMessage(ctx, req)
}
```

#### 3. 速率限制

**错误**：429 Too Many Requests

**处理**：
```go
if err.StatusCode == 429 {
    retryAfter := parseRetryAfter(err.Headers)
    time.Sleep(retryAfter)
    
    return client.SendMessage(ctx, req)
}
```

---

## 📊 配置文件

**文件**: `configs/config.yaml`

```yaml
server:
  port: 50051              # gRPC 端口
  
uipath:
  base_url: "https://cloud.uipath.com/api"
  client_id: "${UIPATH_CLIENT_ID}"
  client_secret: "${UIPATH_CLIENT_SECRET}"
  organization_id: "${UIPATH_ORG_ID}"
  
auth:
  db_path: "./data/tokens.db"
  token_refresh_buffer: 300  # 提前 5 分钟刷新
  
logging:
  level: "info"
  format: "json"
```

---

## 🧪 测试策略

### 单元测试

1. **Converter 测试**
   - Universal → UiPath 转换
   - UiPath → Universal 转换

2. **Auth 测试**
   - Token 缓存
   - Token 刷新逻辑

### 集成测试

3. **End-to-End 测试**
   - Core → UiPath Adapter → UiPath API
   - 真实认证流程
   - 流式响应

---

## 📈 性能考虑

### 优化点

1. **Token 缓存**
   - 避免每次请求都认证
   - SQLite 持久化

2. **连接池**
   - 复用 HTTP 连接
   - 减少握手开销

3. **并发控制**
   - 限制并发请求数
   - 避免触发速率限制

---

## 🎯 实施计划

### Phase 1: 基础框架（1 天）

1. ✅ 创建项目结构
2. ⏳ 初始化 Go module
3. ⏳ 生成 proto 代码
4. ⏳ 实现 gRPC 服务器骨架

### Phase 2: 认证模块（1 天）

5. ⏳ 实现 OAuth Manager
6. ⏳ 实现 Token 缓存
7. ⏳ 实现组织续费
8. ⏳ 单元测试

### Phase 3: API 客户端（1 天）

9. ⏳ 实现 UiPath Client
10. ⏳ 实现流式处理
11. ⏳ 实现错误处理
12. ⏳ 单元测试

### Phase 4: 转换器（0.5 天）

13. ⏳ 实现 Universal → UiPath
14. ⏳ 实现 UiPath → Universal
15. ⏳ 单元测试

### Phase 5: 集成测试（0.5 天）

16. ⏳ Core → Adapter 测试
17. ⏳ 真实 API 测试
18. ⏳ 性能测试

---

## 🔗 依赖关系

### Go 依赖

```go
require (
    google.golang.org/grpc v1.81.1
    google.golang.org/protobuf v1.36.11
    github.com/mattn/go-sqlite3 v1.14.x
    gopkg.in/yaml.v3 v3.0.x
)
```

### 外部依赖

- **UiPath Cloud API** - 后端 AI 服务
- **SQLite** - Token 缓存
- **OAuth 2.0** - 认证

---

## 💡 关键设计决策

### 1. 为什么独立仓库？

**原因**：
- ✅ 独立部署（可以在不同机器）
- ✅ 独立扩展（水平扩展 Adapter）
- ✅ 职责分离（Core 只管协议，Adapter 只管后端）

### 2. 为什么使用 SQLite？

**原因**：
- ✅ 轻量级（无需额外服务）
- ✅ 持久化（重启不丢失 Token）
- ✅ 够用（单 Adapter 不需要复杂数据库）

### 3. 为什么需要 Converter？

**原因**：
- ✅ UiPath API 格式与 Universal 不同
- ✅ 需要提取和重组数据
- ✅ 需要处理字段映射

---

## ✅ 设计完成

**已完成**：
- ✅ 项目结构设计
- ✅ 组件职责划分
- ✅ 认证流程设计
- ✅ 转换逻辑设计
- ✅ 错误处理策略
- ✅ 配置方案
- ✅ 测试策略
- ✅ 实施计划

**下一步**：
1. 初始化 Go module
2. 生成 proto 代码
3. 实现 gRPC 服务器骨架

---

**准备好开始实现了吗？** 🚀
