package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"uipath-adapter/internal/auth"
	pb "uipath-adapter/proto"
)

// storageSourceID 是本 adapter 在主框架 KV 存储里的命名空间。
const storageSourceID = "uipath"

// authStateKey 生成 (email, orgID) 维度的存储 key。一个邮箱可对应多个 org。
func authStateKey(email, orgID string) string {
	return fmt.Sprintf("auth/%s/%s", email, orgID)
}

// storedAuthState 是 adapter 自己定义的存储载荷格式（框架不解析）。
type storedAuthState struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	UpstreamURL  string `json:"upstream_url"`
}

// UiPathAccountSource 实现 AccountSourceService
type UiPathAccountSource struct {
	pb.UnimplementedAccountSourceServiceServer

	baseAccounts []*BaseAccount
	sessions     []*SessionAccount
	mu           sync.RWMutex

	storageClient pb.StorageServiceClient

	// Web session 复用：一次密码登录（AuthenticateWeb）后缓存的 web token + 全部 org。
	// SupplyAccounts 策略2 据此对每个 org 调 BootstrapOrgToken，避免反复密码登录触发反爬。
	// 为 nil 时回退到逐账号全量 Authenticate（向后兼容）。
	webAuth    *auth.UiPathAuth
	webEmail   string
	webOrgs    []auth.OrgInfo
	orgCursor  int

	// proxyURL：框架在注册响应/心跳中下发的出站代理。登录/续期全程走此代理规避反爬。
	// 由 SetProxy 热更新；NewUiPathAuth 的两处回退路径据此构造带代理的 auth manager。
	proxyURL string

	// webLoginPending：注册完成到 SetWebSession 之间的登录窗口标记。
	// 置位期间 bootstrapSession 不走全量 Authenticate 回退（避免框架 5s 水位 ticker
	// 在 web session 就绪前触发密码登录撞反爬），返回空让下个 tick 重试。
	webLoginPending bool
}

// BaseAccount 基础账号（用于创建多个 session）
type BaseAccount struct {
	Email    string
	Password string
	OrgID    string
	TenantID string
}

// SessionAccount 会话账号
type SessionAccount struct {
	ID           string
	Email        string
	Password     string
	OrgID        string
	TenantID     string
	AccessToken  string
	RefreshToken string
	UpstreamURL  string
	ExpiresAt    time.Time
	Active       bool
}

// NewUiPathAccountSource 创建账号源
func NewUiPathAccountSource(storageClient pb.StorageServiceClient) *UiPathAccountSource {
	return &UiPathAccountSource{
		baseAccounts:  []*BaseAccount{},
		sessions:      []*SessionAccount{},
		storageClient: storageClient,
	}
}

// AddBaseAccount 添加基础账号
func (s *UiPathAccountSource) AddBaseAccount(email, password, orgID, tenantID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.baseAccounts = append(s.baseAccounts, &BaseAccount{
		Email:    email,
		Password: password,
		OrgID:    orgID,
		TenantID: tenantID,
	})

	log.Printf("📋 Added base account: %s (org=%s, tenant=%s)", email, orgID, tenantID)
}

// SetWebSession 注入已建立的 web session（main.go 在启动时 1 次密码登录后调用）。
// 之后 SupplyAccounts 对每个 org 复用该 session 调 BootstrapOrgToken，不再重复登录。
func (s *UiPathAccountSource) SetWebSession(mgr *auth.UiPathAuth, email string, orgs []auth.OrgInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.webAuth = mgr
	s.webEmail = email
	s.webOrgs = orgs
	s.orgCursor = 0
	log.Printf("🌐 Web session injected: %d orgs available for %s", len(orgs), email)
}

// SetProxy 热更新出站代理（注册响应/心跳下发）。同时对已存活的 web session 调 SetProxy，
// 使后续 BootstrapOrgToken/续期立即走新代理。空 url = 切回直连。
func (s *UiPathAccountSource) SetProxy(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proxyURL == url {
		return
	}
	s.proxyURL = url
	if s.webAuth != nil {
		s.webAuth.SetProxy(url)
	}
	log.Printf("🔌 Outbound proxy updated: %q", url)
}

// SetWebLoginPending 设置登录窗口标记（main.go 在注册后、登录完成前置位 true，登录后置 false）。
func (s *UiPathAccountSource) SetWebLoginPending(pending bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.webLoginPending = pending
}

// ListAccounts 列出所有活跃会话
func (s *UiPathAccountSource) ListAccounts(ctx context.Context, req *pb.ListAccountsRequest) (*pb.ListAccountsResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var accounts []*pb.AccountInfo
	for _, sess := range s.sessions {
		if sess.Active && time.Now().Before(sess.ExpiresAt) {
			accounts = append(accounts, &pb.AccountInfo{
				AccountId: sess.ID,
				Credentials: map[string]string{
					"access_token": sess.AccessToken,
					"upstream_url": sess.UpstreamURL,
				},
				Metadata: map[string]string{
					"email":     sess.Email,
					"org_id":    sess.OrgID,
					"tenant_id": sess.TenantID,
				},
				ExpiresAt: sess.ExpiresAt.Unix(),
				Quota: &pb.AccountQuota{
					MonthlyLimit: 1000000,
					Used:         0,
					RpmLimit:     60,
				},
			})
		}
	}

	log.Printf("📋 Listed %d active accounts", len(accounts))
	return &pb.ListAccountsResponse{Accounts: accounts}, nil
}

// RefreshAccount 刷新账号 token
func (s *UiPathAccountSource) RefreshAccount(ctx context.Context, req *pb.RefreshAccountRequest) (*pb.RefreshAccountResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var sess *SessionAccount
	for _, s := range s.sessions {
		if s.ID == req.AccountId {
			sess = s
			break
		}
	}

	if sess == nil {
		return &pb.RefreshAccountResponse{Success: false}, nil
	}

	log.Printf("🔄 Refreshing account %s (%s)", sess.ID, sess.Email)

	// 续期同样走框架下发的代理（持有 mu，可直接读 s.proxyURL）。
	authMgr, err := auth.NewUiPathAuth(sess.Email, sess.Password, s.proxyURL)
	if err != nil {
		return &pb.RefreshAccountResponse{Success: false}, err
	}

	// 优先用 refresh_token 续期（快、不依赖密码/MFA）；失败则回退到全量 4 阶段 OAuth
	var tokens *auth.TokenResponse
	if sess.RefreshToken != "" {
		tokens, err = authMgr.RefreshToken(ctx, sess.RefreshToken)
		if err != nil {
			log.Printf("⚠️  refresh_token 续期失败，回退全量认证: %s", truncateForLog(err.Error(), 200))
			tokens = nil
		}
	}
	if tokens == nil {
		tokens, err = authMgr.Authenticate(ctx)
	}
	if err != nil || tokens == nil {
		log.Printf("❌ Failed to refresh %s: %s", sess.Email, truncateForLog(err.Error(), 200))
		sess.Active = false
		return &pb.RefreshAccountResponse{Success: false}, err
	}

	sess.AccessToken = tokens.AccessToken
	if tokens.RefreshToken != "" {
		sess.RefreshToken = tokens.RefreshToken
	}
	if tokens.UpstreamURL != "" {
		sess.UpstreamURL = tokens.UpstreamURL
	}
	if tokens.ExpiresIn > 0 {
		sess.ExpiresAt = time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second)
	}
	sess.Active = true

	// 保存到数据库
	if s.storageClient != nil {
		s.saveToStorage(ctx, sess)
	}

	log.Printf("✅ Refreshed account %s", sess.ID)

	return &pb.RefreshAccountResponse{
		Success: true,
		NewCredentials: map[string]string{
			"access_token": sess.AccessToken,
			"upstream_url": sess.UpstreamURL,
		},
		ExpiresAt: sess.ExpiresAt.Unix(),
	}, nil
}

// HealthCheck 健康检查
func (s *UiPathAccountSource) HealthCheck(ctx context.Context, req *pb.AccountHealthCheckRequest) (*pb.AccountHealthCheckResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var sess *SessionAccount
	for _, s := range s.sessions {
		if s.ID == req.AccountId {
			sess = s
			break
		}
	}

	if sess == nil {
		return &pb.AccountHealthCheckResponse{
			Status:  "not_found",
			Details: "Account not found",
		}, nil
	}

	if !sess.Active {
		return &pb.AccountHealthCheckResponse{
			Status:  "inactive",
			Details: "Account marked as inactive",
		}, nil
	}

	if time.Now().After(sess.ExpiresAt) {
		return &pb.AccountHealthCheckResponse{
			Status:  "expired",
			Details: fmt.Sprintf("Token expired at %s", sess.ExpiresAt),
		}, nil
	}

	return &pb.AccountHealthCheckResponse{
		Status:  "healthy",
		Details: fmt.Sprintf("Expires in %s", time.Until(sess.ExpiresAt).Round(time.Minute)),
	}, nil
}

// SupplyAccounts 补充账号
func (s *UiPathAccountSource) SupplyAccounts(ctx context.Context, req *pb.SupplyAccountsRequest) (*pb.SupplyAccountsResponse, error) {
	log.Printf("📥 Supply request: count=%d, reason=%s", req.Count, req.Reason)

	s.mu.Lock()
	defer s.mu.Unlock()

	var supplied []*pb.AccountInfo

	// 策略1: 尝试从数据库恢复已存在的会话
	if s.storageClient != nil {
		for _, base := range s.baseAccounts {
			if len(supplied) >= int(req.Count) {
				break
			}

			getResp, err := s.storageClient.Get(ctx, &pb.GetRequest{
				SourceId: storageSourceID,
				Key:      authStateKey(base.Email, base.OrgID),
			})

			if err == nil && getResp.Found {
				// 不要恢复已过期的 token：否则客户端拿到的是一个立刻失效的账号
				if getResp.ExpiresAt > 0 && time.Unix(getResp.ExpiresAt, 0).Before(time.Now()) {
					log.Printf("⏭️  Stored token for %s/%s already expired, skipping restore", base.Email, base.OrgID)
					continue
				}
				var state storedAuthState
				if err := json.Unmarshal(getResp.Value, &state); err != nil {
					log.Printf("⚠️  Failed to parse stored token for %s/%s: %v", base.Email, base.OrgID, err)
					continue
				}
				sess := &SessionAccount{
					ID:           fmt.Sprintf("%s-%d", base.Email, time.Now().Unix()),
					Email:        base.Email,
					Password:     base.Password,
					OrgID:        base.OrgID,
					TenantID:     base.TenantID,
					AccessToken:  state.AccessToken,
					RefreshToken: state.RefreshToken,
					UpstreamURL:  state.UpstreamURL,
					ExpiresAt:    time.Unix(getResp.ExpiresAt, 0),
					Active:       true,
				}

				s.sessions = append(s.sessions, sess)
				supplied = append(supplied, sess.toProto())
				log.Printf("🔄 Restored session from storage: %s", sess.ID)
			}
		}
	}

	// 策略2: 创建新会话
	for len(supplied) < int(req.Count) {
		tokens, sessEmail, sessOrgID, sessTenantID, err := s.bootstrapSession(ctx)
		if err != nil {
			// 认证失败就停止本轮补号——不要立即重试同一个账号（会形成紧密重试循环狂打 UiPath，
			// 曾一次跑出上万次尝试、把日志撑到 GB 级并触发上游限流）。水位监控下个 tick 自然重试，相当于退避。
			log.Printf("❌ bootstrap stopped: %s", truncateForLog(err.Error(), 200))
			break
		}

		sess := &SessionAccount{
			ID:           fmt.Sprintf("%s-%d", sessEmail, time.Now().Unix()),
			Email:        sessEmail,
			Password:     "",
			OrgID:        sessOrgID,
			TenantID:     sessTenantID,
			AccessToken:  tokens.AccessToken,
			RefreshToken: tokens.RefreshToken,
			UpstreamURL:  tokens.UpstreamURL,
			ExpiresAt:    time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second),
			Active:       true,
		}

		s.sessions = append(s.sessions, sess)
		supplied = append(supplied, sess.toProto())

		// 保存到数据库
		if s.storageClient != nil {
			s.saveToStorage(ctx, sess)
		}

		log.Printf("✅ Created session %s (org=%s)", sess.ID, sessOrgID)
	}

	log.Printf("✅ Supplied %d accounts (requested %d)", len(supplied), req.Count)

	return &pb.SupplyAccountsResponse{
		SuppliedCount: int32(len(supplied)),
		Accounts:      supplied,
		Message:       fmt.Sprintf("Created %d new sessions", len(supplied)),
	}, nil
}

// Helper methods

func (s *UiPathAccountSource) selectBaseAccount() *BaseAccount {
	if len(s.baseAccounts) == 0 {
		return nil
	}
	// 简单轮询
	return s.baseAccounts[len(s.sessions)%len(s.baseAccounts)]
}

// bootstrapSession 创建一个新 session。
// 优先路径：复用已建立的 web session，对下一个 org 调 BootstrapOrgToken（无密码登录）。
// 回退路径：无 web session 时，用单个 base account 跑全量 Authenticate（向后兼容）。
// 调用者持有 s.mu。
func (s *UiPathAccountSource) bootstrapSession(ctx context.Context) (*auth.TokenResponse, string, string, string, error) {
	// 优先：web session 复用
	if s.webAuth != nil && len(s.webOrgs) > 0 {
		org := s.webOrgs[s.orgCursor%len(s.webOrgs)]
		s.orgCursor++
		log.Printf("🆕 Bootstrapping org %s (%s) via cached web session", org.Name, org.GlobalID)
		tokens, err := s.webAuth.BootstrapOrgToken(ctx, org)
		if err != nil {
			return nil, "", "", "", fmt.Errorf("bootstrap org %s: %w", org.Name, err)
		}
		tenantID := "DefaultTenant"
		if base := s.baseAccountForOrg(org.GlobalID); base != nil && base.TenantID != "" {
			tenantID = base.TenantID
		}
		return tokens, s.webEmail, org.GlobalID, tenantID, nil
	}

	// 登录窗口保护：注册后到 web session 就绪前，不走全量密码登录回退，
	// 否则框架 5s 水位 ticker 会在 web session 建立前触发密码登录撞反爬。
	// 返回空错误让本轮补号无果，下个 tick web session 通常已就绪。
	if s.webLoginPending {
		return nil, "", "", "", fmt.Errorf("web login pending, defer fallback authenticate")
	}

	// 回退：全量 Authenticate（单账号）
	base := s.selectBaseAccount()
	if base == nil {
		return nil, "", "", "", fmt.Errorf("no base accounts available")
	}
	log.Printf("🆕 Full authenticate for %s (no web session)", base.Email)
	authMgr, err := auth.NewUiPathAuth(base.Email, base.Password, s.proxyURL)
	if err != nil {
		return nil, "", "", "", err
	}
	tokens, err := authMgr.Authenticate(ctx)
	if err != nil {
		return nil, "", "", "", err
	}
	return tokens, base.Email, base.OrgID, base.TenantID, nil
}

// baseAccountForOrg 找出匹配某 orgID 的 base account（用于取 tenant 等附加信息）。
func (s *UiPathAccountSource) baseAccountForOrg(orgID string) *BaseAccount {
	for _, b := range s.baseAccounts {
		if b.OrgID == orgID {
			return b
		}
	}
	return nil
}

// truncateForLog 截断过长的错误字符串，避免 auth 失败时把整段 URL/HTML 刷进日志（曾撑爆 /tmp）。
func truncateForLog(s string, n int) string {
	if len(s) > n {
		return s[:n] + "...(truncated)"
	}
	return s
}

func (s *SessionAccount) toProto() *pb.AccountInfo {
	return &pb.AccountInfo{
		AccountId: s.ID,
		Credentials: map[string]string{
			"access_token": s.AccessToken,
			"upstream_url": s.UpstreamURL,
		},
		Metadata: map[string]string{
			"email":     s.Email,
			"org_id":    s.OrgID,
			"tenant_id": s.TenantID,
		},
		ExpiresAt: s.ExpiresAt.Unix(),
		Quota: &pb.AccountQuota{
			MonthlyLimit: 1000000,
			Used:         0,
			RpmLimit:     60,
		},
	}
}

func (s *UiPathAccountSource) saveToStorage(ctx context.Context, sess *SessionAccount) {
	state := storedAuthState{
		AccessToken:  sess.AccessToken,
		RefreshToken: sess.RefreshToken,
		UpstreamURL:  sess.UpstreamURL,
	}
	raw, err := json.Marshal(state)
	if err != nil {
		log.Printf("⚠️  Failed to marshal auth state: %v", err)
		return
	}
	_, err = s.storageClient.Put(ctx, &pb.PutRequest{
		SourceId:  storageSourceID,
		Key:       authStateKey(sess.Email, sess.OrgID),
		Value:     raw,
		ExpiresAt: sess.ExpiresAt.Unix(),
	})

	if err != nil {
		log.Printf("⚠️  Failed to save to storage: %v", err)
	}
}
