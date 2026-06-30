# 当前进度（KV 重构 + OAuth 拆分完成）

## 已完成 ✅

### 1. 两仓库初始提交
- `new/polyglot`: `9e39f44`（119 文件）
- `new/uipath_adapter`: `7ff8def`（24 文件）

### 2. StorageService → KV 模式（主框架去 UiPath 化）✅
- proto（srv + adapter 双份）: SaveAuthState/LoadAuthState → Put/Get/Delete/List(source_id, key, bytes value, expires_at)
- srv 新增 `internal/data/kv.go`：KVStoreRecord + KVStoreRepository（gorm，表 kv_store，PK source_id+key）
- srv `internal/data/store.go`：Store 挂 kvStore 字段 + KVStore() accessor + AutoMigrate 注册 KVStoreRecord
- srv `internal/storage/uipath_storage.go` 重写：实现 Put/Get/Delete/List，不再懂 token 字段
- adapter `internal/server/account_source.go`：LoadAuthState→Get、SaveAuthState→Put，value=JSON{access_token,refresh_token,upstream_url}，key=auth/<email>/<orgID>，source_id="uipath"
- 测试：srv storage 测试重写为 KV（Put/Get/Delete/List），全绿

### 3. OAuth 方案 1（一次登录拿 N orgs）✅
- `internal/auth/oauth.go`：
  - 拆分 Authenticate() → AuthenticateWeb()(Phase 1+2，返回 []OrgInfo) + BootstrapOrgToken(org)(Phase 3+4)
  - UiPathAuth 加 webTokens/webReady 字段缓存 Phase 1 session
  - fetchUserOrgs 改成返回全部 org（不再只 orgs[0]）
  - 保留 Authenticate() 向后兼容（= AuthenticateWeb + BootstrapOrgToken(orgs[0])）
- `internal/server/account_source.go`：
  - 加 webAuth/webOrgs/orgCursor 字段 + SetWebSession()
  - SupplyAccounts 策略2 改 bootstrapSession()：优先复用 web session 调 BootstrapOrgToken（round-robin orgs），无 web session 回退全量 Authenticate
- `cmd/adapter/main.go`：启动 1 次 AuthenticateWeb → 为每个 org AddBaseAccount + SetWebSession

### 4. dev.sh + .env + frontend stdin EIO 修复 ✅（早先）

## 待做

### Task #7: 端到端联调（待 UiPath 账号冷却）
账号 abc@uruz.top 因之前 5s 低水位重试触发 429 反爬，需冷却。
**冷却前不要 ./dev.sh start**。冷却后验证：
- adapter.log: `Web login ok: N orgs` + `Bootstrapping org ...`
- 不应再出现 `/usernamepassword/login` 429
- test_anthropic.sh 跑通真实请求

### 后续：文档对齐 / Web 优化

## 关键收益
- 反爬根治：1 次密码登录拿 N org，token 续期走 RefreshToken，长期零登录
- 框架零业务知识：Storage 是纯 KV，接新 adapter 不改 proto
