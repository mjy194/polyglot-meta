package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"

	"uipath-adapter/internal/debug"
)

// UiPathAuth UiPath 认证管理器
type UiPathAuth struct {
	email       string
	password    string
	httpClient  *http.Client
	cloudOrigin string
	proxyURL    string // 出站代理；空 = 直连。登录/续期全程走此代理以规避反爬。

	// Phase 1 web session — 由 AuthenticateWeb 建立，供 BootstrapOrgToken 复用。
	// 复用 httpClient.Jar（session cookies）是"1 次登录拿 N 个 org token"的关键。
	webTokens *TokenResponse
	webReady  bool
}

// OrgInfo 表示账号可见的一个组织（Phase 2 返回，Phase 3+4 针对单个 org 拿 token）。
type OrgInfo struct {
	Name     string
	GlobalID string
}

// TokenResponse Token 响应
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	UpstreamURL  string `json:"-"` // 内部字段：AutoPilot API 的 base URL
}

// NewUiPathAuth 创建认证管理器。proxyURL 非空时，登录/续期的全部出站请求走该代理
// （scheme://[user:pass@]host:port），以规避 UiPath 登录端点的反爬限制。
func NewUiPathAuth(email, password, proxyURL string) (*UiPathAuth, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	return &UiPathAuth{
		email:       email,
		password:    password,
		cloudOrigin: "https://cloud.uipath.com",
		proxyURL:    proxyURL,
		httpClient: &http.Client{
			Jar:       jar,
			Timeout:   60 * time.Second,
			Transport: newAuthTransport(proxyURL),
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse // 不自动跟随重定向
			},
		},
	}, nil
}

// newAuthTransport 构造登录用 transport：强制 IPv4（规避 IPv6 连通性问题），
// proxyURL 非空时叠加出站代理。proxyURL 解析失败时回退为无代理（仅记录），不影响 IPv4 强制。
func newAuthTransport(proxyURL string) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr)
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			tr.Proxy = http.ProxyURL(u)
		} else {
			debug.Printf("⚠️  invalid proxy_url %q, auth runs direct: %v\n", proxyURL, err)
		}
	}
	return tr
}

// SetProxy 热更新出站代理：重建 transport（保留强制 IPv4），但保留现有 Jar，
// 使已建立的 web session（cookies）继续有效。空 proxyURL = 切回直连。
func (a *UiPathAuth) SetProxy(proxyURL string) {
	if a.proxyURL == proxyURL {
		return
	}
	a.proxyURL = proxyURL
	a.httpClient.Transport = newAuthTransport(proxyURL)
}

const accountOrigin = "https://account.uipath.com"

// portalClientID 是 Phase 3 portal token（cloud.uipath.com/identity_/connect/token）的 client_id，
// RefreshToken 续期也用同一个 client_id。
const portalClientID = "1119a927-10ab-4543-bd1a-ad6bfbbc27f4"

// Authenticate 执行完整认证流程（向后兼容：Phase 1+2 后取第一个 org 跑 Phase 3+4）。
// 多 org 场景请改用 AuthenticateWeb + 逐个 BootstrapOrgToken。
func (a *UiPathAuth) Authenticate(ctx context.Context) (*TokenResponse, error) {
	debug.Println("🔐 Starting UiPath authentication (full 4 phases, first org)...")

	orgs, err := a.AuthenticateWeb(ctx)
	if err != nil {
		return nil, err
	}
	if len(orgs) == 0 {
		return nil, fmt.Errorf("phase2: no orgs found")
	}
	debug.Printf("✅ Using first org: name=%s, globalId=%s\n", orgs[0].Name, orgs[0].GlobalID)
	return a.BootstrapOrgToken(ctx, orgs[0])
}

// AuthenticateWeb 执行 Phase 1（密码登录）+ Phase 2（列出全部 org）。
// 成功后 web session 状态缓存在 receiver 上，后续可对任意 org 调 BootstrapOrgToken，
// 而无需再次密码登录（这是避免触发反爬的关键）。
// 返回账号可见的全部 org 列表。
func (a *UiPathAuth) AuthenticateWeb(ctx context.Context) ([]OrgInfo, error) {
	debug.Println("\n=== Phase 1: Web OAuth PKCE ===")
	webTokens, err := a.webPasswordAuth(ctx)
	if err != nil {
		return nil, fmt.Errorf("phase1 web auth: %w", err)
	}
	a.webTokens = webTokens
	a.webReady = true
	debug.Printf("✅ Phase 1 done: web token len=%d, id_token len=%d\n",
		len(webTokens.AccessToken), len(webTokens.IDToken))

	debug.Println("\n=== Phase 2: Fetch User Orgs ===")
	orgs, err := a.fetchUserOrgs(ctx, webTokens.AccessToken, webTokens.IDToken)
	if err != nil {
		return nil, fmt.Errorf("phase2 fetch orgs: %w", err)
	}
	debug.Printf("✅ Phase 2 done: %d orgs visible\n", len(orgs))
	for i, o := range orgs {
		debug.Printf("   [%d] name=%s, globalId=%s\n", i, o.Name, o.GlobalID)
	}
	return orgs, nil
}

// BootstrapOrgToken 对单个 org 执行 Phase 3（portal token）+ Phase 4（发现 upstream URL）。
// 必须先调用 AuthenticateWeb 建立 web session。复用 Phase 1 的 session cookies，
// 因此对 N 个 org 只产生 1 次密码登录。
func (a *UiPathAuth) BootstrapOrgToken(ctx context.Context, org OrgInfo) (*TokenResponse, error) {
	if !a.webReady || a.webTokens == nil {
		return nil, fmt.Errorf("web session not established; call AuthenticateWeb first")
	}
	debug.Printf("\n=== Phase 3+4: Bootstrap org name=%s globalId=%s ===\n", org.Name, org.GlobalID)

	portalTokens, err := a.bootstrapPortal1119Token(ctx, org.GlobalID, a.webTokens.IDToken)
	if err != nil {
		return nil, fmt.Errorf("phase3 bootstrap 1119: %w", err)
	}
	debug.Printf("✅ Phase 3 done: portal token len=%d\n", len(portalTokens.AccessToken))

	upstreamURL, err := a.discoverAutopilotURL(ctx, org.Name, org.GlobalID, portalTokens.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("phase4 discover URL: %w", err)
	}
	debug.Printf("✅ Phase 4 done: URL=%s\n", upstreamURL)

	portalTokens.UpstreamURL = upstreamURL
	return portalTokens, nil
}

// WebTokens 返回 Phase 1 拿到的 web token（含 id_token），供需要复用 web session 的场景。
// 未建立 web session 时返回 nil。
func (a *UiPathAuth) WebTokens() *TokenResponse {
	if !a.webReady {
		return nil
	}
	return a.webTokens
}

// webPasswordAuth 完整的 Web OAuth PKCE + WS-Federation 流程
// 完全对齐 Rust 参考实现 auth/flow.rs
func (a *UiPathAuth) webPasswordAuth(ctx context.Context) (*TokenResponse, error) {
	// PKCE 参数
	codeVerifier := generateCodeVerifier()
	codeChallenge := generateCodeChallenge(codeVerifier)
	authorizeState := generateRandomString(32)
	authorizeNonce := generateRandomString(32)

	webClientID := "2yt9HdF45O006H9qdPcP9as5cdGbnCWs"
	redirectURI := fmt.Sprintf("%s/portal_/authCallback", a.cloudOrigin)
	webAudience := "https://uipath.eu.auth0.com/api/v2/"
	webScope := "openid profile email read:current_user update:current_user_metadata"

	// ============ Step 1: 发起 OAuth authorize ============
	debug.Println("📤 Step 1: Starting OAuth authorization...")
	authorizeURL := fmt.Sprintf("%s/authorize", accountOrigin)

	req, err := http.NewRequestWithContext(ctx, "GET", authorizeURL, nil)
	if err != nil {
		return nil, fmt.Errorf("step1 create request: %w", err)
	}

	// 完整参数（对齐 Rust 实现）
	q := req.URL.Query()
	q.Add("audience", webAudience)
	q.Add("scope", webScope)
	q.Add("client_id", webClientID)
	q.Add("redirect_uri", redirectURI)
	q.Add("type", "login")
	q.Add("ecommerceRedirect", "false")
	q.Add("redirectPath", "")
	q.Add("subscription_plan", "")
	q.Add("service_redirect_uri", "")
	q.Add("retryUrl", "/portal_/enterprisesso")
	q.Add("product_name", "UiPath Automation Cloud")
	q.Add("company_code", "B2B_CP")
	q.Add("platform_name", "UiPath Platform")
	q.Add("cloudrpa_signup_subdomain", "/portal_")
	q.Add("register_endpoint", "/register")
	q.Add("use_local_registration", "false")
	q.Add("response_type", "code")
	q.Add("response_mode", "query")
	q.Add("state", authorizeState)
	q.Add("nonce", authorizeNonce)
	q.Add("code_challenge", codeChallenge)
	q.Add("code_challenge_method", "S256")
	q.Add("auth0Client", "eyJuYW1lIjoiYXV0aDAta")
	req.URL.RawQuery = q.Encode()

	resp1, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("step1 request: %w", err)
	}
	resp1.Body.Close()

	if resp1.StatusCode < 300 || resp1.StatusCode >= 400 {
		return nil, fmt.Errorf("step1 expected redirect, got %d", resp1.StatusCode)
	}

	loginURL := resp1.Header.Get("Location")
	if loginURL == "" {
		return nil, fmt.Errorf("step1: no login redirect")
	}
	if !strings.HasPrefix(loginURL, "http") {
		loginURL = accountOrigin + loginURL
	}
	debug.Printf("   Login URL: %s...\n", loginURL[:min(80, len(loginURL))])

	// ============ Step 2: 访问登录页，提取 CSRF（可选）============
	debug.Println("📤 Step 2: Loading login page...")
	req2, err := http.NewRequestWithContext(ctx, "GET", loginURL, nil)
	if err != nil {
		return nil, fmt.Errorf("step2 create request: %w", err)
	}
	req2.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req2.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req2.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp2, err := a.httpClient.Do(req2)
	if err != nil {
		return nil, fmt.Errorf("step2 request: %w", err)
	}
	loginHTML, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()

	if !isSuccess(resp2.StatusCode) {
		return nil, fmt.Errorf("step2: login page returned %d", resp2.StatusCode)
	}

	// CSRF 是可选的（与 Rust 对齐：unwrap_or_default）
	csrf := extractHiddenInput(string(loginHTML), "_csrf")
	debug.Printf("   CSRF: %q (may be empty)\n", csrf)

	// 从最终 URL 获取 state 和 nonce
	loginFinalURL := resp2.Request.URL.String()
	loginURLParsed, _ := url.Parse(loginFinalURL)
	postState := loginURLParsed.Query().Get("state")
	if postState == "" {
		postState = authorizeState
	}
	postNonce := loginURLParsed.Query().Get("nonce")
	if postNonce == "" {
		postNonce = authorizeNonce
	}

	// ============ Step 3: 提交用户名密码到 /usernamepassword/login ============
	debug.Println("📤 Step 3: Submitting credentials...")
	loginPayload := map[string]interface{}{
		"client_id":    webClientID,
		"redirect_uri": redirectURI,
		"tenant":       "uipath",
		"response_type": "token id_token",
		"scope":        webScope,
		"audience":     webAudience,
		"_csrf":        csrf,
		"state":        postState,
		"_intstate":    "deprecated",
		"nonce":        postNonce,
		"username":     a.email,
		"password":     a.password,
		"connection":   "Username-Password-Authentication",
	}

	payloadBytes, _ := json.Marshal(loginPayload)
	req3, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/usernamepassword/login", accountOrigin),
		strings.NewReader(string(payloadBytes)))
	if err != nil {
		return nil, fmt.Errorf("step3 create request: %w", err)
	}
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("Accept", "*/*")
	req3.Header.Set("Origin", accountOrigin)
	req3.Header.Set("Referer", loginFinalURL)
	req3.Header.Set("Sec-Fetch-Dest", "empty")
	req3.Header.Set("Sec-Fetch-Mode", "cors")
	req3.Header.Set("Sec-Fetch-Site", "same-origin")
	req3.Header.Set("auth0-client", "eyJuYW1lIjoiYXV0aDAuanMtdWxwIiwidmVyc2lvbiI6IjkuMjkuMCJ9")

	resp3, err := a.httpClient.Do(req3)
	if err != nil {
		return nil, fmt.Errorf("step3 request: %w", err)
	}
	upLoginHTML, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()

	if !isSuccess(resp3.StatusCode) {
		return nil, fmt.Errorf("step3: /usernamepassword/login returned %d - %s",
			resp3.StatusCode, string(upLoginHTML))
	}

	debug.Printf("   Credential response: %d bytes\n", len(upLoginHTML))
	// DEBUG: Print first 800 bytes of response
	snippet3 := string(upLoginHTML)
	if len(snippet3) > 800 {
		snippet3 = snippet3[:800]
	}
	debug.Printf("   Step3 response snippet:\n%s\n---\n", snippet3)

	// ============ Step 4: 提取 WS-Federation 字段，POST 到 /login/callback ============
	debug.Println("📤 Step 4: Processing WS-Federation callback...")
	upLoginHTMLStr := string(upLoginHTML)
	waField := extractHiddenInput(upLoginHTMLStr, "wa")
	if waField == "" {
		waField = "wsignin1.0"
	}
	wresultField := extractHiddenInput(upLoginHTMLStr, "wresult")
	wctxField := extractHiddenInput(upLoginHTMLStr, "wctx")

	debug.Printf("   wa=%q, wresult_len=%d, wctx_len=%d\n",
		waField, len(wresultField), len(wctxField))
	// DEBUG: Print wresult first 300 chars
	if len(wresultField) > 0 {
		wr := wresultField
		if len(wr) > 300 {
			wr = wr[:300]
		}
		debug.Printf("   wresult: %s\n", wr)
	}

	if strings.TrimSpace(wresultField) == "" || strings.TrimSpace(wctxField) == "" {
		// 打印响应片段供调试
		snippet := upLoginHTMLStr
		if len(snippet) > 500 {
			snippet = snippet[:500]
		}
		return nil, fmt.Errorf("step4: WS-Federation fields missing. Response: %s", snippet)
	}

	callbackData := url.Values{}
	callbackData.Set("wa", waField)
	callbackData.Set("wresult", wresultField)
	callbackData.Set("wctx", wctxField)

	req4, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/login/callback", accountOrigin),
		strings.NewReader(callbackData.Encode()))
	if err != nil {
		return nil, fmt.Errorf("step4 create request: %w", err)
	}
	req4.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req4.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req4.Header.Set("Origin", accountOrigin)
	req4.Header.Set("Referer", loginFinalURL)
	req4.Header.Set("Sec-Fetch-Dest", "document")
	req4.Header.Set("Sec-Fetch-Mode", "navigate")
	req4.Header.Set("Sec-Fetch-Site", "same-origin")

	resp4, err := a.httpClient.Do(req4)
	if err != nil {
		return nil, fmt.Errorf("step4 request: %w", err)
	}
	resp4.Body.Close()

	resumeURL := resp4.Header.Get("Location")
	if resumeURL == "" {
		return nil, fmt.Errorf("step4: /login/callback returned %d with no redirect", resp4.StatusCode)
	}
	if !strings.HasPrefix(resumeURL, "http") {
		resumeURL = accountOrigin + resumeURL
	}
	debug.Printf("   Resume URL (full): %s\n", resumeURL)
	debug.Printf("   wctxField: %s\n", wctxField)

	// ============ Step 5: 跟随 resume URL ============
	debug.Println("📤 Step 5: Following resume URL...")
	req5, err := http.NewRequestWithContext(ctx, "GET", resumeURL, nil)
	if err != nil {
		return nil, fmt.Errorf("step5 create request: %w", err)
	}
	req5.Header.Set("Referer", loginFinalURL)
	req5.Header.Set("Sec-Fetch-Dest", "document")
	req5.Header.Set("Sec-Fetch-Mode", "navigate")
	req5.Header.Set("Sec-Fetch-Site", "same-origin")

	resp5, err := a.httpClient.Do(req5)
	if err != nil {
		return nil, fmt.Errorf("step5 request: %w", err)
	}
	resp5.Body.Close()

	authCallbackURL := resp5.Header.Get("Location")
	if authCallbackURL == "" {
		return nil, fmt.Errorf("step5: resume returned %d with no authCallback redirect", resp5.StatusCode)
	}
	if !strings.HasPrefix(authCallbackURL, "http") {
		authCallbackURL = a.cloudOrigin + authCallbackURL
	}
	debug.Printf("   authCallback URL: %s...\n", authCallbackURL[:min(80, len(authCallbackURL))])

	// ============ Step 6: 访问 authCallback，获取 authorization code ============
	debug.Println("📤 Step 6: Getting authorization code...")
	req6, err := http.NewRequestWithContext(ctx, "GET", authCallbackURL, nil)
	if err != nil {
		return nil, fmt.Errorf("step6 create request: %w", err)
	}
	req6.Header.Set("Referer", loginFinalURL)
	req6.Header.Set("Sec-Fetch-Dest", "document")
	req6.Header.Set("Sec-Fetch-Mode", "navigate")
	req6.Header.Set("Sec-Fetch-Site", "same-site")

	resp6, err := a.httpClient.Do(req6)
	if err != nil {
		return nil, fmt.Errorf("step6 request: %w", err)
	}
	resp6.Body.Close()

	// 从 authCallback URL 提取 authorization code
	parsedCallback, err := url.Parse(authCallbackURL)
	if err != nil {
		return nil, fmt.Errorf("step6: parse authCallback URL: %w", err)
	}
	authCode := parsedCallback.Query().Get("code")
	if authCode == "" {
		return nil, fmt.Errorf("step6: no authorization code in URL: %s", authCallbackURL)
	}
	debug.Printf("✅ Got authorization code: %s...\n", authCode[:min(20, len(authCode))])

	// ============ Step 7: 用 code 交换 access token ============
	debug.Println("📤 Step 7: Exchanging code for access token...")
	tokenPayload := map[string]interface{}{
		"redirect_uri":  redirectURI,
		"client_id":     webClientID,
		"code_verifier": codeVerifier,
		"grant_type":    "authorization_code",
		"code":          authCode,
	}
	tokenPayloadBytes, _ := json.Marshal(tokenPayload)

	req7, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/oauth/token", accountOrigin),
		strings.NewReader(string(tokenPayloadBytes)))
	if err != nil {
		return nil, fmt.Errorf("step7 create request: %w", err)
	}
	req7.Header.Set("Content-Type", "application/json")
	req7.Header.Set("Accept", "*/*")
	req7.Header.Set("Origin", a.cloudOrigin)
	req7.Header.Set("Referer", fmt.Sprintf("%s/", a.cloudOrigin))
	req7.Header.Set("Sec-Fetch-Dest", "empty")
	req7.Header.Set("Sec-Fetch-Mode", "cors")
	req7.Header.Set("Sec-Fetch-Site", "same-site")

	resp7, err := a.httpClient.Do(req7)
	if err != nil {
		return nil, fmt.Errorf("step7 request: %w", err)
	}
	defer resp7.Body.Close()

	if !isSuccess(resp7.StatusCode) {
		body, _ := io.ReadAll(resp7.Body)
		return nil, fmt.Errorf("step7: /oauth/token returned %d - %s", resp7.StatusCode, string(body))
	}

	var tokens TokenResponse
	if err := json.NewDecoder(resp7.Body).Decode(&tokens); err != nil {
		return nil, fmt.Errorf("step7: decode tokens: %w", err)
	}

	if strings.TrimSpace(tokens.AccessToken) == "" {
		return nil, fmt.Errorf("step7: access_token is empty in response")
	}

	debug.Printf("✅ Authentication successful! Token length: %d\n", len(tokens.AccessToken))
	return &tokens, nil
}

// fetchUserOrgs Phase 2: 获取用户的组织列表，返回全部 org（不再只取第一个）。
// 对齐 Rust fetch_web_user_orgs: GET /portal_/api/identity/UserOrgs/UserOrgsLocalByToken
func (a *UiPathAuth) fetchUserOrgs(ctx context.Context, accessToken, idToken string) ([]OrgInfo, error) {
	doRequest := func(token, tokenType string) ([]byte, int, error) {
		reqURL := fmt.Sprintf("%s/portal_/api/identity/UserOrgs/UserOrgsLocalByToken", a.cloudOrigin)
		req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
		if err != nil {
			return nil, 0, err
		}
		q := req.URL.Query()
		q.Add("includeNonAcceptedInvites", "true")
		req.URL.RawQuery = q.Encode()
		req.Header.Set("Authorization", tokenType+" "+token)
		req.Header.Set("Accept", "application/json")

		resp, err := a.httpClient.Do(req)
		if err != nil {
			return nil, 0, fmt.Errorf("fetch orgs request: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return body, resp.StatusCode, nil
	}

	// 先用 access_token 试
	body, status, err := doRequest(accessToken, "Bearer")
	if err != nil {
		return nil, err
	}

	// 若返回 "missing email claim"，用 id_token fallback
	if !isSuccess(status) || strings.Contains(string(body), "missing email claim") {
		debug.Println("   fetchUserOrgs: access_token failed, retrying with id_token...")
		body, status, err = doRequest(idToken, "Bearer")
		if err != nil {
			return nil, err
		}
	}

	if !isSuccess(status) {
		return nil, fmt.Errorf("fetch orgs: status %d body=%s", status, string(body[:min(300, len(body))]))
	}

	// 响应是 []UiPathUserOrg，字段: name, globalId
	var orgs []struct {
		Name     string `json:"name"`
		GlobalID string `json:"globalId"`
	}
	if err := json.Unmarshal(body, &orgs); err != nil {
		// 尝试解析 {"value": [...]} 格式
		var wrapper struct {
			Value []struct {
				Name     string `json:"name"`
				GlobalID string `json:"globalId"`
			} `json:"value"`
		}
		if err2 := json.Unmarshal(body, &wrapper); err2 == nil && len(wrapper.Value) > 0 {
			orgs = wrapper.Value
		} else {
			return nil, fmt.Errorf("parse orgs: %w (body=%s)", err, string(body[:min(300, len(body))]))
		}
	}
	if len(orgs) == 0 {
		return nil, fmt.Errorf("no orgs found (body=%s)", string(body[:min(300, len(body))]))
	}

	// 打印所有 orgs
	debug.Printf("   All orgs (%d total):\n", len(orgs))
	for i, o := range orgs {
		debug.Printf("     [%d] name=%s, globalId=%s\n", i, o.Name, o.GlobalID)
	}

	result := make([]OrgInfo, 0, len(orgs))
	for _, o := range orgs {
		result = append(result, OrgInfo{Name: o.Name, GlobalID: o.GlobalID})
	}
	return result, nil
}

// bootstrapPortal1119Token Phase 3: 第二次 PKCE，复用 session cookies，换取 portal service token
// client_id=1119a927-10ab-4543-bd1a-ad6bfbbc27f4，对齐 Rust bootstrap_portal_1119_token
// 关键：使用允许自动跟随重定向的客户端（携带 Phase 1 的 session cookies），
// 这样重定向链会把 session 带到正确的 signin-oidc 表单或直接到 loginsuccess
func (a *UiPathAuth) bootstrapPortal1119Token(ctx context.Context, orgID, idTokenFallback string) (*TokenResponse, error) {
	portalRedirectURI := fmt.Sprintf("%s/portal_/loginsuccess", a.cloudOrigin)
	portalScope := "openid profile email offline_access"

	codeVerifier := generateCodeVerifier()
	codeChallenge := generateCodeChallenge(codeVerifier)
	state := generateRandomString(32)
	nonce := generateRandomString(32)

	// 创建允许自动跟随重定向的客户端（共享 Phase 1 的 session cookies）
	// Rust 默认跟随所有重定向，session cookie 携带，最终到达 signin-oidc form 或 loginsuccess
	redirectClient := &http.Client{
		Jar:       a.httpClient.Jar,       // 共享 session cookies（Phase 1 设置的）
		Transport: a.httpClient.Transport, // 同样的 IPv4 transport
		Timeout:   60 * time.Second,
		// 不设置 CheckRedirect = 默认允许最多10次重定向
	}

	// Step A: 发起第二次 authorize（自动跟随所有重定向，带着 session cookies）
	// 关键：Phase 3 用 cloud.uipath.com/identity_/connect/authorize，不是 account.uipath.com/authorize
	debug.Println("   Step A: 2nd PKCE authorize (auto-following redirects with session cookies)...")
	authorizeURL := fmt.Sprintf("%s/identity_/connect/authorize", a.cloudOrigin)
	req, err := http.NewRequestWithContext(ctx, "GET", authorizeURL, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	q.Add("client_id", portalClientID)
	q.Add("redirect_uri", portalRedirectURI)
	q.Add("scope", portalScope)
	q.Add("response_type", "code")
	q.Add("response_mode", "query")
	q.Add("state", state)
	q.Add("nonce", nonce)
	q.Add("code_challenge", codeChallenge)
	q.Add("code_challenge_method", "S256")
	q.Add("acr_values", "tenant:"+orgID)
	req.URL.RawQuery = q.Encode()
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	respA, err := redirectClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("step A authorize: %w", err)
	}
	bodyA, _ := io.ReadAll(respA.Body)
	respA.Body.Close()

	// 跟随重定向后的最终 URL
	finalURLStr := respA.Request.URL.String()
	debug.Printf("   Step A final URL: %s\n", finalURLStr[:min(150, len(finalURLStr))])
	debug.Printf("   Step A status: %d html_len=%d\n", respA.StatusCode, len(bodyA))

	// 情况1: 直接落在 loginsuccess（用户已授权 1119 client，无需再次表单提交）
	if strings.Contains(finalURLStr, "/portal_/loginsuccess") {
		parsedURL, _ := url.Parse(finalURLStr)
		directCode := parsedURL.Query().Get("code")
		if directCode != "" {
			debug.Printf("   ✅ Got auth code directly from loginsuccess: %s...\n", directCode[:min(20, len(directCode))])
			return a.exchangePortalCode(ctx, directCode, codeVerifier, portalClientID, portalRedirectURI)
		}
	}

	// 情况2: 需要提交 id_token form（Rust 标准流程）
	htmlA := string(bodyA)
	debug.Printf("   Step A HTML preview:\n%s\n---\n", htmlA[:min(1500, len(htmlA))])

	formAction := extractFormAction(htmlA)
	idToken := extractHiddenInput(htmlA, "id_token")
	formState := extractHiddenInput(htmlA, "state")

	if idToken == "" {
		idToken = idTokenFallback // Phase 1 的 id_token 作为 fallback
		debug.Println("   Using id_token fallback from Phase 1")
	}

	if formAction == "" {
		// 页面没有 form（错误页面等），直接用 Phase 1 的 id_token 尝试 portal signin
		formAction = fmt.Sprintf("%s/portal_/oidc/signin-oidc", a.cloudOrigin)
		debug.Printf("   form_action empty, using fallback: %s\n", formAction)
	} else if !strings.HasPrefix(formAction, "http") {
		if strings.Contains(formAction, "portal_") || strings.Contains(formAction, "identity_") {
			formAction = a.cloudOrigin + formAction
		} else {
			formAction = accountOrigin + formAction
		}
	}

	debug.Printf("   form_action=%s\n", formAction[:min(100, len(formAction))])
	debug.Printf("   id_token len=%d, state len=%d\n", len(idToken), len(formState))

	// Step C: POST signin-oidc
	debug.Println("   Step C: POST signin-oidc...")
	postData := url.Values{}
	postData.Set("id_token", idToken)
	if formState != "" {
		postData.Set("state", formState)
	}

	reqC, err := http.NewRequestWithContext(ctx, "POST", formAction,
		strings.NewReader(postData.Encode()))
	if err != nil {
		return nil, err
	}
	reqC.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqC.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")

	respC, err := a.httpClient.Do(reqC)
	if err != nil {
		return nil, fmt.Errorf("step C post signin-oidc: %w", err)
	}
	bodyC, _ := io.ReadAll(respC.Body)
	respC.Body.Close()
	locC := respC.Header.Get("Location")
	debug.Printf("   Step C: status=%d body_len=%d location=%s\n", respC.StatusCode, len(bodyC), locC[:min(100, len(locC))])

	// Step D: 跟随最多8次重定向，找 /portal_/loginsuccess?code=
	debug.Println("   Step D: Following redirects to loginsuccess...")
	currentURL := locC
	if currentURL != "" && !strings.HasPrefix(currentURL, "http") {
		currentURL = a.cloudOrigin + currentURL
	}
	if currentURL == "" {
		// Step C 没有返回 redirect，检查 body 是否有跳转
		currentURL = respC.Request.URL.String()
	}

	var authCode string
	for i := 0; i < 8; i++ {
		if currentURL == "" {
			break
		}
		debug.Printf("   redirect[%d]: %s\n", i, currentURL[:min(120, len(currentURL))])

		if strings.Contains(currentURL, "/portal_/loginsuccess") {
			parsedURL, _ := url.Parse(currentURL)
			authCode = parsedURL.Query().Get("code")
			if authCode != "" {
				debug.Printf("   ✅ Found auth code: %s...\n", authCode[:min(20, len(authCode))])
				break
			}
		}
		if strings.Contains(currentURL, "code=") {
			parsedURL, _ := url.Parse(currentURL)
			if c := parsedURL.Query().Get("code"); c != "" {
				authCode = c
				debug.Printf("   ✅ Found auth code in URL: %s...\n", authCode[:min(20, len(authCode))])
				break
			}
		}

		reqD, err := http.NewRequestWithContext(ctx, "GET", currentURL, nil)
		if err != nil {
			break
		}
		reqD.Header.Set("User-Agent", "Mozilla/5.0")

		respD, err := a.httpClient.Do(reqD)
		if err != nil {
			break
		}
		io.ReadAll(respD.Body)
		respD.Body.Close()

		nextLoc := respD.Header.Get("Location")
		if nextLoc == "" {
			break
		}
		if !strings.HasPrefix(nextLoc, "http") {
			if strings.HasPrefix(currentURL, "https://cloud.uipath.com") {
				nextLoc = a.cloudOrigin + nextLoc
			} else {
				nextLoc = accountOrigin + nextLoc
			}
		}
		currentURL = nextLoc

		if respD.StatusCode < 300 || respD.StatusCode >= 400 {
			break
		}
	}

	if authCode == "" {
		return nil, fmt.Errorf("step D: auth code not found after 8 redirects (last URL: %s)", currentURL[:min(150, len(currentURL))])
	}

	return a.exchangePortalCode(ctx, authCode, codeVerifier, portalClientID, portalRedirectURI)
}

// exchangePortalCode 用 authorization code 换取 portal access_token (Step E)
func (a *UiPathAuth) exchangePortalCode(ctx context.Context, authCode, codeVerifier, clientID, redirectURI string) (*TokenResponse, error) {
	debug.Println("   Step E: Exchanging code for portal token...")
	tokenEndpoint := fmt.Sprintf("%s/identity_/connect/token", a.cloudOrigin)
	tokenData := url.Values{}
	tokenData.Set("grant_type", "authorization_code")
	tokenData.Set("code", authCode)
	tokenData.Set("redirect_uri", redirectURI)
	tokenData.Set("client_id", clientID)
	tokenData.Set("code_verifier", codeVerifier)

	req, err := http.NewRequestWithContext(ctx, "POST", tokenEndpoint,
		strings.NewReader(tokenData.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("step E token exchange: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !isSuccess(resp.StatusCode) {
		return nil, fmt.Errorf("step E: status %d body=%s", resp.StatusCode, string(body[:min(300, len(body))]))
	}

	var tokens TokenResponse
	if err := json.Unmarshal(body, &tokens); err != nil {
		return nil, fmt.Errorf("step E parse token: %w", err)
	}
	if tokens.AccessToken == "" {
		return nil, fmt.Errorf("step E: empty access_token in response: %s", string(body[:min(200, len(body))]))
	}
	debug.Printf("   ✅ Portal token obtained: len=%d\n", len(tokens.AccessToken))
	return &tokens, nil
}

// RefreshToken 用 portal refresh_token 续期（与 exchangePortalCode 同一 endpoint、同一 client_id，
// 只是 grant_type 改为 refresh_token）。失败时返回错误，由调用方回退到全量 Authenticate。
// 返回的 TokenResponse 不含 UpstreamURL（保持不变）。
func (a *UiPathAuth) RefreshToken(ctx context.Context, refreshToken string) (*TokenResponse, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("empty refresh_token")
	}
	tokenEndpoint := fmt.Sprintf("%s/identity_/connect/token", a.cloudOrigin)
	data := url.Values{}
	data.Set("grant_type", "refresh_token")
	data.Set("client_id", portalClientID)
	data.Set("refresh_token", refreshToken)

	req, err := http.NewRequestWithContext(ctx, "POST", tokenEndpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh token request: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !isSuccess(resp.StatusCode) {
		return nil, fmt.Errorf("refresh token: status %d body=%s", resp.StatusCode, string(body[:min(300, len(body))]))
	}

	var tokens TokenResponse
	if err := json.Unmarshal(body, &tokens); err != nil {
		return nil, fmt.Errorf("refresh token parse: %w", err)
	}
	if tokens.AccessToken == "" {
		return nil, fmt.Errorf("refresh token: empty access_token in response: %s", string(body[:min(200, len(body))]))
	}
	debug.Printf("   ✅ Refreshed portal token: len=%d\n", len(tokens.AccessToken))
	return &tokens, nil
}

// discoverAutopilotURL Phase 4: 发现 AutoPilot 服务 URL
// orgName = logical name (e.g. "pgc5446e3865"), orgID = UUID
func (a *UiPathAuth) discoverAutopilotURL(ctx context.Context, orgName, orgID, accessToken string) (string, error) {
	bearer := "Bearer " + accessToken

	// Step 4B: Get tenant list with service instances (using orgName in path)
	debug.Printf("   Step 4B: GET tenants for org=%s\n", orgName)
	tenantsURL := fmt.Sprintf("%s/%s/organization_/api/organization/%s/tenants?serviceType=orchestrator&includeTenantServices=true",
		a.cloudOrigin, orgName, orgName)
	req, err := http.NewRequestWithContext(ctx, "GET", tenantsURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", bearer)
	req.Header.Set("Accept", "application/json")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("tenants request: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	tenantName := "DefaultTenant"
	if isSuccess(resp.StatusCode) {
		// Step 4C: Parse tenants and find autopilotstudio service
		var tenants []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			TenantServiceInstances []struct {
				ServiceType string `json:"serviceType"`
				URL         string `json:"url"`
				FriendlyURL string `json:"friendlyUrl"`
			} `json:"tenantServiceInstances"`
		}
		if err := json.Unmarshal(body, &tenants); err == nil {
			debug.Printf("   Found %d tenants\n", len(tenants))
			for _, tenant := range tenants {
				debug.Printf("   Tenant: %s (%d services)\n", tenant.Name, len(tenant.TenantServiceInstances))
				for _, svc := range tenant.TenantServiceInstances {
					if strings.EqualFold(svc.ServiceType, "autopilotstudio") {
						friendly := strings.TrimRight(strings.TrimSpace(svc.FriendlyURL), "/")
						if friendly == "" {
							friendly = strings.TrimRight(strings.TrimSpace(svc.URL), "/")
						}
						if friendly != "" {
							url := friendly + "/autopilot-everywhere"
							debug.Printf("   ✅ Found AutoPilot URL from service: %s\n", url)
							return url, nil
						}
					}
				}
			}
			if len(tenants) > 0 {
				tenantName = tenants[0].Name
			}
		}
	} else {
		debug.Printf("   tenants API status %d (non-fatal), using default tenant\n", resp.StatusCode)
	}

	// Fallback: construct URL from orgName + tenant name
	fallbackURL := fmt.Sprintf("%s/%s/%s/autopilotstudio_/autopilot-everywhere", a.cloudOrigin, orgName, tenantName)
	debug.Printf("   Using constructed AutoPilot URL: %s\n", fallbackURL)
	return fallbackURL, nil
}

// extractFormAction 从 HTML 中提取 <form action="..."> 的值
func extractFormAction(htmlStr string) string {
	re := regexp.MustCompile(`(?i)<form[^>]*\saction=["']([^"']+)["']`)
	if m := re.FindStringSubmatch(htmlStr); len(m) > 1 {
		return html.UnescapeString(m[1])
	}
	return ""
}

func isSuccess(code int) bool {
	return code >= 200 && code < 300
}

func extractHiddenInput(htmlStr, name string) string {
	// 使用 (?s) 标志让 . 匹配换行符，处理多行 input 标签
	// 尝试 name 在 value 之前
	re1 := regexp.MustCompile(fmt.Sprintf(`(?s)<input[^>]*name="%s"[^>]*value="([^"]*)"`, name))
	if matches := re1.FindStringSubmatch(htmlStr); len(matches) > 1 {
		return html.UnescapeString(matches[1])
	}
	// 尝试 value 在 name 之前
	re2 := regexp.MustCompile(fmt.Sprintf(`(?s)<input[^>]*value="([^"]*)"[^>]*name="%s"`, name))
	if matches := re2.FindStringSubmatch(htmlStr); len(matches) > 1 {
		return html.UnescapeString(matches[1])
	}
	// 尝试单引号
	re3 := regexp.MustCompile(fmt.Sprintf(`(?s)<input[^>]*name='%s'[^>]*value='([^']*)'`, name))
	if matches := re3.FindStringSubmatch(htmlStr); len(matches) > 1 {
		return html.UnescapeString(matches[1])
	}
	return ""
}

func generateCodeVerifier() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func generateCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func generateRandomString(length int) string {
	b := make([]byte, length)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)[:length]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
