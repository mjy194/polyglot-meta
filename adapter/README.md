# UiPath Adapter

UiPath Adapter 是 Polyglot 的第一个真实后端适配器，连接 UiPath Cloud AI 服务。

## 🎯 功能

- ✅ gRPC 服务器（实现 AdapterService 接口）
- ⏳ OAuth 2.0 PKCE 认证（未实现）
- ⏳ UiPath Cloud API 客户端（未实现）
- ⏳ Universal ↔ UiPath 格式转换（未实现）

## 🏗️ 项目结构

```
uipath_adapter/
├── cmd/adapter/           # 主程序
├── internal/
│   ├── server/           # gRPC 服务器 ✅
│   ├── auth/             # OAuth 认证 ⏳
│   ├── client/           # UiPath API 客户端 ⏳
│   ├── converter/        # 格式转换 ⏳
│   └── config/           # 配置管理 ⏳
├── proto/                # gRPC 协议定义 ✅
└── configs/              # 配置文件
```

## 🚀 快速开始

### 编译

```bash
go build -o uipath-adapter cmd/adapter/main.go
```

### 运行

```bash
./uipath-adapter
```

默认监听端口：`50051`

### 使用环境变量

```bash
PORT=50052 ./uipath-adapter
```

## 🧪 测试

### 使用 Core 的 E2E 测试

```bash
# Terminal 1: 启动 UiPath Adapter
./uipath-adapter

# Terminal 2: 运行 Core 的 E2E 测试
cd ../polyglot/src/srv
go run cmd/e2e-test/main.go
```

## 📋 当前状态

### Phase 1: 基础框架 ✅

- ✅ 项目结构
- ✅ Go module 初始化
- ✅ Proto 代码生成
- ✅ gRPC 服务器骨架
- ✅ 可编译运行

### Phase 2: 认证模块 ⏳

- ⏳ OAuth Manager
- ⏳ Token 缓存
- ⏳ 组织续费处理

### Phase 3: API 客户端 ⏳

- ⏳ UiPath Client
- ⏳ 流式处理
- ⏳ 错误处理

### Phase 4: 转换器 ⏳

- ⏳ Universal → UiPath
- ⏳ UiPath → Universal

### Phase 5: 集成测试 ⏳

- ⏳ 端到端测试
- ⏳ 真实 API 测试

## 📊 API 端点

实现的 gRPC 方法：

- ✅ `GetMetadata` - 返回 Adapter 元数据
- ✅ `HealthCheck` - 健康检查
- ✅ `ProcessRequest` - 处理请求（当前返回 Mock）
- ✅ `CancelRequest` - 取消请求

## 🔧 配置

配置文件：`configs/config.yaml`（待创建）

```yaml
server:
  port: 50051

uipath:
  base_url: "https://cloud.uipath.com/api"
  client_id: "${UIPATH_CLIENT_ID}"
  client_secret: "${UIPATH_CLIENT_SECRET}"
```

## 📚 文档

- [设计文档](./DESIGN.md) - 详细的架构设计

## 🤝 贡献

这是 Polyglot 项目的一部分。

## 📄 License

MIT
