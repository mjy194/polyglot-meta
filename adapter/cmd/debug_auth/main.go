package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"time"
)

func main() {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	codeVerifier := generateCodeVerifier()
	codeChallenge := generateCodeChallenge(codeVerifier)
	state := generateRandomString(32)
	nonce := generateRandomString(32)

	webClientID := "2yt9HdF45O006H9qdPcP9as5cdGbnCWs"
	redirectURI := "https://cloud.uipath.com/portal_/authCallback"
	accountOrigin := "https://account.uipath.com"
	webAudience := "https://uipath.eu.auth0.com/api/v2/"
	webScope := "openid profile email read:current_user update:current_user_metadata"

	fmt.Println("=== Step 1: Starting authorization ===")
	authorizeURL := fmt.Sprintf("%s/authorize", accountOrigin)

	req, _ := http.NewRequest("GET", authorizeURL, nil)
	q := req.URL.Query()
	q.Add("audience", webAudience)
	q.Add("scope", webScope)
	q.Add("client_id", webClientID)
	q.Add("redirect_uri", redirectURI)
	q.Add("response_type", "code")
	q.Add("response_mode", "query")
	q.Add("state", state)
	q.Add("nonce", nonce)
	q.Add("code_challenge", codeChallenge)
	q.Add("code_challenge_method", "S256")
	q.Add("auth0Client", "eyJuYW1lIjoiYXV0aDAta")
	req.URL.RawQuery = q.Encode()

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("ERROR Step1: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	fmt.Printf("Step1 Status: %d\n", resp.StatusCode)
	fmt.Printf("Step1 Location: %s\n", resp.Header.Get("Location"))

	loginURL := resp.Header.Get("Location")
	if loginURL == "" {
		fmt.Println("ERROR: no redirect location")
		os.Exit(1)
	}
	if !strings.HasPrefix(loginURL, "http") {
		loginURL = accountOrigin + loginURL
	}

	fmt.Printf("\n=== Step 2: Loading login page: %s ===\n", loginURL[:min(80, len(loginURL))])

	loginReq, _ := http.NewRequest("GET", loginURL, nil)
	loginReq.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")
	loginResp, err := client.Do(loginReq)
	if err != nil {
		fmt.Printf("ERROR Step2: %v\n", err)
		os.Exit(1)
	}
	defer loginResp.Body.Close()

	fmt.Printf("Step2 Status: %d\n", loginResp.StatusCode)
	fmt.Printf("Step2 Final URL: %s\n", loginResp.Request.URL.String()[:min(100, len(loginResp.Request.URL.String()))])

	loginHTML, _ := io.ReadAll(loginResp.Body)
	htmlStr := string(loginHTML)
	fmt.Printf("Step2 HTML Length: %d bytes\n", len(htmlStr))

	// Save full HTML to file for inspection
	os.WriteFile("/tmp/login_page.html", loginHTML, 0644)
	fmt.Println("\n✅ Full HTML saved to /tmp/login_page.html")

	// Search for CSRF patterns
	fmt.Println("\n=== CSRF Token Search ===")

	patterns := []string{
		`_csrf`,
		`csrf`,
		`name="_csrf"`,
		`name='_csrf'`,
		`"_csrf":`,
		`window._csrf`,
		`var csrf`,
		`data-csrf`,
	}

	for _, pattern := range patterns {
		idx := strings.Index(htmlStr, pattern)
		if idx >= 0 {
			start := max(0, idx-50)
			end := min(len(htmlStr), idx+100)
			fmt.Printf("FOUND '%s' at pos %d:\n  ...%s...\n\n", pattern, idx, htmlStr[start:end])
		} else {
			fmt.Printf("NOT FOUND: '%s'\n", pattern)
		}
	}

	// Print first 2000 chars
	fmt.Printf("\n=== First 2000 chars of HTML ===\n%s\n", htmlStr[:min(2000, len(htmlStr))])
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

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
