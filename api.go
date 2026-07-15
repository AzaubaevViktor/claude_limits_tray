package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// APIError is an HTTP/transport failure talking to the OAuth API.
type APIError struct {
	Msg        string
	Status     int    // 0 for transport/credential errors
	RetryAfter int    // seconds, from the retry-after header
	ResetAt    string // ISO/epoch, from a ratelimit-reset header
}

func (e *APIError) Error() string { return e.Msg }

const (
	apiBase           = "https://api.anthropic.com"
	keychain          = "Claude Code-credentials"
	userAgent         = "claude-cli/2.1.187 (external, cli)"
	oauthBeta         = "oauth-2025-04-20"
	claudeOAuthClient = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeTokenURL    = "https://console.anthropic.com/v1/oauth/token"
	codexBase         = "https://chatgpt.com/backend-api"
	codexAuthService  = "Codex Auth"
	codexUserAgent    = "codex-cli"
	codexAccessEnvVar = "CODEX_ACCESS_TOKEN"
	codexHomeEnvVar   = "CODEX_HOME"
	codexOAuthClient  = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexTokenURL     = "https://auth.openai.com/oauth/token"
	metaMark          = "\n__META__"
)

type Client struct {
	mu           sync.Mutex
	cachedToken  string
	tokenExp     time.Time
	refreshToken string
	refreshDirty bool
}

// token returns a usable access token. The keychain is the source of truth —
// Claude Code keeps it fresh while you work, so when its token is still valid we
// use it directly and never interfere. Only when the keychain token is also
// expired (Claude Code idle, e.g. overnight) do we mint a fresh one ourselves
// from the refresh token, the same way Claude Code would.
func (c *Client) token() (string, error) {
	c.mu.Lock()
	if c.cachedToken != "" && (c.tokenExp.IsZero() || time.Now().Before(c.tokenExp)) {
		tok := c.cachedToken
		c.mu.Unlock()
		return tok, nil
	}
	c.mu.Unlock()

	if access, refresh, exp, ok := c.readKeychain(); ok {
		if exp.IsZero() || time.Now().Before(exp) {
			c.storeTokens(access, refresh, exp) // adopt Claude Code's fresh token
			return access, nil
		}
		// Keychain token is expired too. Keep a refresh token we already hold —
		// it may be a fresher rotation than the keychain's.
		c.mu.Lock()
		if c.refreshToken == "" {
			c.refreshToken = refresh
		}
		rt := c.refreshToken
		c.mu.Unlock()
		if rt != "" {
			if tok, err := c.refresh(rt); err == nil {
				return tok, nil
			} else {
				log.Printf("claude token refresh failed: %v", err)
			}
		}
		// Refresh failed too — hand back the expired token so the 401 surfaces the
		// "reopen Claude Code" message.
		c.storeTokens(access, refresh, exp)
		return access, nil
	}
	// No keychain creds — last resort, a refresh token from persisted state.
	c.mu.Lock()
	rt := c.refreshToken
	c.mu.Unlock()
	if rt != "" {
		if tok, err := c.refresh(rt); err == nil {
			return tok, nil
		}
	}
	return "", &APIError{Msg: "No credentials — sign in to Claude Code"}
}

func (c *Client) readKeychain() (access, refresh string, exp time.Time, ok bool) {
	out, err := exec.Command("security", "find-generic-password",
		"-s", keychain, "-w").Output()
	if err != nil {
		return "", "", time.Time{}, false
	}
	a, r, ms := claudeCredsFromJSON(out)
	if a == "" {
		return "", "", time.Time{}, false
	}
	if ms > 0 {
		exp = time.UnixMilli(ms).Add(-time.Minute) // a minute of slack
	}
	return a, r, exp, true
}

func claudeCredsFromJSON(raw []byte) (access, refresh string, expiresMs int64) {
	var d map[string]json.RawMessage
	if json.Unmarshal(raw, &d) != nil {
		return "", "", 0
	}
	obj := d
	if inner, ok := d["claudeAiOauth"]; ok {
		var m map[string]json.RawMessage
		if json.Unmarshal(inner, &m) == nil {
			obj = m
		}
	}
	_ = json.Unmarshal(obj["accessToken"], &access)
	_ = json.Unmarshal(obj["refreshToken"], &refresh)
	_ = json.Unmarshal(obj["expiresAt"], &expiresMs)
	return access, refresh, expiresMs
}

// refresh exchanges the refresh token for a new access token via Anthropic's
// OAuth endpoint — the same flow Claude Code uses. The refresh token goes on
// stdin, never argv, so it can't leak into the process list.
func (c *Client) refresh(rt string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": rt,
		"client_id":     claudeOAuthClient,
	})
	log.Printf("claude token refresh")
	cmd := exec.Command("curl", "-sS", "--max-time", "15", "-X", "POST",
		"-H", "Content-Type: application/json",
		"-w", metaMark+"%{http_code}",
		"--data-binary", "@-", claudeTokenURL)
	cmd.Stdin = bytes.NewReader(body)
	out, err := cmd.Output()
	if err != nil {
		return "", &APIError{Msg: "Offline — can't refresh Claude token"}
	}
	s := string(out)
	idx := strings.LastIndex(s, metaMark)
	if idx < 0 {
		return "", &APIError{Msg: "Malformed Claude refresh response"}
	}
	status, _ := strconv.Atoi(strings.TrimSpace(s[idx+len(metaMark):]))
	if status >= 400 {
		return "", &APIError{Msg: fmt.Sprintf("Claude refresh HTTP %d", status), Status: status}
	}
	var d struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if json.Unmarshal([]byte(s[:idx]), &d) != nil || d.AccessToken == "" {
		return "", &APIError{Msg: "Malformed Claude refresh JSON"}
	}
	var exp time.Time
	if d.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(d.ExpiresIn)*time.Second - time.Minute)
	}
	c.storeTokens(d.AccessToken, d.RefreshToken, exp)
	return d.AccessToken, nil
}

func (c *Client) storeTokens(access, refresh string, exp time.Time) {
	c.mu.Lock()
	c.cachedToken = access
	c.tokenExp = exp
	if refresh != "" && refresh != c.refreshToken {
		c.refreshToken = refresh
		c.refreshDirty = true
	}
	c.mu.Unlock()
}

// invalidateAccess drops only the rejected access token (keeping the refresh
// token), so a concurrent "use current login" cannot have its newly adopted
// token cleared by an older request finishing with 401/403.
func (c *Client) invalidateAccess(rejected string) {
	c.mu.Lock()
	if c.cachedToken == rejected {
		c.cachedToken = ""
		c.tokenExp = time.Time{}
	}
	c.mu.Unlock()
}

// UseCurrentAuth forcibly adopts the credentials currently stored by Claude
// Code. Unlike token(), this intentionally bypasses a still-valid cached token:
// it is the explicit account-switch path used by the menu action.
func (c *Client) UseCurrentAuth() error {
	access, refresh, exp, ok := c.readKeychain()
	if !ok {
		return &APIError{Msg: "No credentials — sign in to Claude Code"}
	}
	c.adoptKeychainTokens(access, refresh, exp)
	log.Printf("claude credentials reloaded from keychain")
	return nil
}

// adoptKeychainTokens replaces the complete token pair. In particular, an
// absent refresh token clears a stale persisted token from another account.
func (c *Client) adoptKeychainTokens(access, refresh string, exp time.Time) {
	c.mu.Lock()
	c.cachedToken = access
	c.tokenExp = exp
	if refresh != c.refreshToken {
		c.refreshToken = refresh
		c.refreshDirty = true
	}
	c.mu.Unlock()
}

func (c *Client) LoadRefresh(rt string) {
	rt = strings.TrimSpace(rt)
	if rt == "" {
		return
	}
	c.mu.Lock()
	if c.refreshToken == "" {
		c.refreshToken = rt
	}
	c.mu.Unlock()
}

func (c *Client) RefreshToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refreshToken
}

func (c *Client) TakeRefreshDirty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.refreshDirty
	c.refreshDirty = false
	return d
}

func (c *Client) get(path string) ([]byte, error) {
	tok, err := c.token()
	if err != nil {
		log.Printf("GET %s -> %v", path, err)
		return nil, err
	}
	body, err := c.do(path, tok)
	// A rejected token usually just means it expired — drop it and try once more,
	// which re-reads the keychain (Claude Code may have refreshed it) or mints a
	// fresh one from the refresh token.
	if ae, ok := err.(*APIError); ok && (ae.Status == 401 || ae.Status == 403) {
		c.invalidateAccess(tok)
		if tok, err2 := c.token(); err2 == nil {
			body, err = c.do(path, tok)
		}
	}
	return body, err
}

func (c *Client) do(path, tok string) ([]byte, error) {
	wfmt := metaMark + "%{http_code}|%header{retry-after}" +
		"|%header{anthropic-ratelimit-unified-reset}"
	// Log the request — never the Authorization header / token.
	log.Printf("GET %s", path)
	start := time.Now()
	out, err := exec.Command("curl", "-sS", "--max-time", "15",
		"-H", "Authorization: Bearer "+tok,
		"-H", "anthropic-beta: "+oauthBeta,
		"-H", "User-Agent: "+userAgent,
		"-H", "Accept: application/json",
		"-w", wfmt, apiBase+path).Output()
	took := time.Since(start).Round(time.Millisecond)
	if err != nil {
		log.Printf("GET %s -> transport error in %s: %v", path, took, err)
		return nil, &APIError{Msg: "Offline — can't reach Claude API"}
	}
	s := string(out)
	idx := strings.LastIndex(s, metaMark)
	if idx < 0 {
		log.Printf("GET %s -> malformed response in %s: %q", path, took, s)
		return nil, &APIError{Msg: "Malformed curl response"}
	}
	body := s[:idx]
	fields := strings.Split(s[idx+len(metaMark):], "|")
	status, convErr := strconv.Atoi(strings.TrimSpace(fields[0]))
	if convErr != nil {
		log.Printf("GET %s -> malformed status in %s: %q", path, took, s)
		return nil, &APIError{Msg: "Malformed curl response"}
	}
	if status >= 400 {
		// Dump the body only on an error — it carries the server's reason.
		log.Printf("GET %s -> %d in %s (%d bytes): %s", path, status, took, len(body), body)
		e := &APIError{Msg: fmt.Sprintf("HTTP %d", status), Status: status}
		if len(fields) > 1 {
			if ra, err := strconv.ParseFloat(strings.TrimSpace(fields[1]), 64); err == nil {
				e.RetryAfter = int(ra)
			}
		}
		if len(fields) > 2 {
			e.ResetAt = strings.TrimSpace(fields[2])
		}
		return nil, e
	}
	log.Printf("GET %s -> %d in %s (%d bytes)", path, status, took, len(body))
	return []byte(body), nil
}

// --- response types ---------------------------------------------------------

type Window struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

type ScopedLimit struct {
	Kind     string  `json:"kind"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resets_at"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

type Usage struct {
	FiveHour Window        `json:"five_hour"`
	SevenDay Window        `json:"seven_day"`
	Limits   []ScopedLimit `json:"limits"`
}

type Profile struct {
	Account struct {
		FullName    string `json:"full_name"`
		DisplayName string `json:"display_name"`
		Email       string `json:"email"`
	} `json:"account"`
	Organization struct {
		RateLimitTier    string `json:"rate_limit_tier"`
		OrganizationType string `json:"organization_type"`
	} `json:"organization"`
}

// Usage returns the raw bytes (for persistence) and the parsed struct.
func (c *Client) Usage() ([]byte, *Usage, error) {
	raw, err := c.get("/api/oauth/usage")
	if err != nil {
		return nil, nil, err
	}
	var u Usage
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, nil, &APIError{Msg: "Malformed usage JSON"}
	}
	return raw, &u, nil
}

func (c *Client) Profile() (*Profile, error) {
	raw, err := c.get("/api/oauth/profile")
	if err != nil {
		return nil, err
	}
	var p Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func parseUsage(raw []byte) *Usage {
	if len(raw) == 0 {
		return nil
	}
	var u Usage
	if json.Unmarshal(raw, &u) != nil {
		return nil
	}
	return &u
}

// --- Codex usage API --------------------------------------------------------

type CodexClient struct {
	CodexHome    string
	mu           sync.Mutex
	cachedToken  string    // current access token (in memory)
	tokenExp     time.Time // when cachedToken expires (zero = unknown)
	refreshToken string    // long-lived; lets us mint access tokens without the keychain
	refreshDirty bool      // refreshToken changed since the last persist
	accountLabel string    // display-only account hint learned from the access token
	accountDirty bool      // accountLabel changed since the last persist
}

type CodexWindow struct {
	UsedPercent   float64
	WindowSeconds int64
	ResetsAt      int64
}

type CodexCredits struct {
	HasCredits bool    `json:"has_credits"`
	Unlimited  bool    `json:"unlimited"`
	Balance    *string `json:"balance"`
}

type CodexLimit struct {
	ID        string
	Name      string
	Primary   *CodexWindow
	Secondary *CodexWindow
	Credits   *CodexCredits
}

type CodexUsage struct {
	PlanType     string
	ReachedType  string
	ResetCredits int
	Limits       []CodexLimit
}

// token returns a usable access token. It prefers a still-valid cached token,
// then mints a fresh one from the refresh token (no keychain, no password), and
// only falls back to reading the keychain/auth.json/env when there's no working
// refresh token to bootstrap from.
func (c *CodexClient) token() (string, error) {
	c.mu.Lock()
	if c.cachedToken != "" && (c.tokenExp.IsZero() || time.Now().Before(c.tokenExp)) {
		tok := c.cachedToken
		c.mu.Unlock()
		return tok, nil
	}
	rt := c.refreshToken
	c.mu.Unlock()

	if rt != "" {
		tok, err := c.refresh(rt)
		if err == nil {
			return tok, nil
		}
		log.Printf("codex token refresh failed: %v", err)
		if ae, ok := err.(*APIError); ok && ae.Status >= 400 && ae.Status < 500 {
			// The refresh token was rejected (e.g. you signed out and back in) —
			// re-read the keychain to bootstrap a new one.
			return c.bootstrap()
		}
		// Transient failure (offline / 5xx / malformed): keep the refresh token
		// and surface the error so we retry later, rather than prompting.
		return "", err
	}
	return c.bootstrap()
}

type codexBootstrapCreds struct {
	access  string
	refresh string
	source  string
}

// bootstrapCreds reads credentials the slow way — env var, auth.json, then
// keychain (which is the one that can prompt).
func (c *CodexClient) bootstrapCreds() (*codexBootstrapCreds, error) {
	if tok := strings.TrimSpace(os.Getenv(codexAccessEnvVar)); tok != "" {
		return &codexBootstrapCreds{access: tok, source: codexAccessEnvVar}, nil
	}
	home, err := c.codexHome()
	if err != nil {
		return nil, err
	}
	if access, refresh, err := codexTokensFromFile(filepath.Join(home, "auth.json")); err == nil && access != "" {
		return &codexBootstrapCreds{
			access:  access,
			refresh: refresh,
			source:  filepath.Join(home, "auth.json"),
		}, nil
	}
	key, err := codexStoreKey(home)
	if err != nil {
		return nil, err
	}
	out, err := exec.Command("security", "find-generic-password",
		"-s", codexAuthService, "-a", key, "-w").Output()
	if err == nil {
		if access, refresh, err := codexTokensFromJSON(out); err == nil && access != "" {
			return &codexBootstrapCreds{
				access:  access,
				refresh: refresh,
				source:  "keychain " + codexAuthService,
			}, nil
		}
	}
	return nil, &APIError{Msg: "No Codex ChatGPT credentials — run codex login"}
}

// bootstrap reads current Codex credentials and caches them so the next poll can
// refresh silently instead of hitting the keychain again.
func (c *CodexClient) bootstrap() (string, error) {
	creds, err := c.bootstrapCreds()
	if err != nil {
		return "", err
	}
	c.storeBootstrapTokens(creds.access, creds.refresh)
	return creds.access, nil
}

// UseCurrentAuth replaces the persisted Codex token pair with the currently
// configured Codex login (env/auth.json/keychain), returning a display label for
// the account it switched to.
func (c *CodexClient) UseCurrentAuth() (string, error) {
	creds, err := c.bootstrapCreds()
	if err != nil {
		return "", err
	}
	account := codexAccountFromJWT(creds.access)
	log.Printf("codex account switched from %s: %s", creds.source, orElse(account, "unknown"))
	c.storeBootstrapTokens(creds.access, creds.refresh)
	return account, nil
}

// refresh exchanges the refresh token for a new access token via OpenAI's OAuth
// endpoint — the same flow the Codex CLI uses. The refresh token is sent on
// stdin, never argv, so it can't leak into the process list.
func (c *CodexClient) refresh(rt string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"client_id":     codexOAuthClient,
		"grant_type":    "refresh_token",
		"refresh_token": rt,
		"scope":         "openid profile email",
	})
	log.Printf("codex token refresh")
	cmd := exec.Command("curl", "-sS", "--max-time", "15", "-X", "POST",
		"-H", "Content-Type: application/json",
		"-w", metaMark+"%{http_code}",
		"--data-binary", "@-", codexTokenURL)
	cmd.Stdin = bytes.NewReader(body)
	out, err := cmd.Output()
	if err != nil {
		return "", &APIError{Msg: "Offline — can't refresh Codex token"}
	}
	s := string(out)
	idx := strings.LastIndex(s, metaMark)
	if idx < 0 {
		return "", &APIError{Msg: "Malformed Codex refresh response"}
	}
	status, _ := strconv.Atoi(strings.TrimSpace(s[idx+len(metaMark):]))
	if status >= 400 {
		return "", &APIError{Msg: fmt.Sprintf("Codex refresh HTTP %d", status), Status: status}
	}
	var d struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if json.Unmarshal([]byte(s[:idx]), &d) != nil || d.AccessToken == "" {
		return "", &APIError{Msg: "Malformed Codex refresh JSON"}
	}
	c.storeTokens(d.AccessToken, d.RefreshToken, d.ExpiresIn)
	return d.AccessToken, nil
}

// storeTokens caches the access token (with an expiry derived from its JWT, or
// expires_in, minus a minute of slack) and records a rotated refresh token.
func (c *CodexClient) storeTokens(access, refresh string, expiresIn int) {
	c.mu.Lock()
	c.cachedToken = access
	switch {
	case !jwtExp(access).IsZero():
		c.tokenExp = jwtExp(access).Add(-time.Minute)
	case expiresIn > 0:
		c.tokenExp = time.Now().Add(time.Duration(expiresIn)*time.Second - time.Minute)
	default:
		c.tokenExp = time.Time{}
	}
	if refresh != "" && refresh != c.refreshToken {
		c.refreshToken = refresh
		c.refreshDirty = true
	}
	if label := codexAccountFromJWT(access); label != "" && label != c.accountLabel {
		c.accountLabel = label
		c.accountDirty = true
		log.Printf("codex account: %s", label)
	}
	c.mu.Unlock()
}

func (c *CodexClient) storeBootstrapTokens(access, refresh string) {
	c.storeTokens(access, refresh, 0)
	if refresh != "" {
		return
	}
	c.mu.Lock()
	if c.refreshToken != "" {
		c.refreshToken = ""
		c.refreshDirty = true
	}
	c.mu.Unlock()
}

// invalidateAccess drops only the access token (keeping the refresh token), so a
// rejected token triggers a refresh on the next call rather than a keychain read.
func (c *CodexClient) invalidateAccess() {
	c.mu.Lock()
	c.cachedToken = ""
	c.tokenExp = time.Time{}
	c.mu.Unlock()
}

// LoadRefresh seeds the refresh token from persisted state (without marking it
// dirty); it won't clobber a token already learned this run.
func (c *CodexClient) LoadRefresh(rt string) {
	rt = strings.TrimSpace(rt)
	if rt == "" {
		return
	}
	c.mu.Lock()
	if c.refreshToken == "" {
		c.refreshToken = rt
	}
	c.mu.Unlock()
}

// LoadAccount seeds the display account from persisted state (without marking
// it dirty); a later access token can replace it with fresher data.
func (c *CodexClient) LoadAccount(label string) {
	label = strings.TrimSpace(label)
	if label == "" {
		return
	}
	c.mu.Lock()
	if c.accountLabel == "" {
		c.accountLabel = label
	}
	c.mu.Unlock()
}

// RefreshToken returns the current refresh token for persistence.
func (c *CodexClient) RefreshToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refreshToken
}

// AccountLabel returns the last display-only account hint learned for Codex.
func (c *CodexClient) AccountLabel() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.accountLabel
}

// TakeRefreshDirty reports whether the refresh token rotated since the last call
// and clears the flag, so the caller can persist it promptly.
func (c *CodexClient) TakeRefreshDirty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.refreshDirty
	c.refreshDirty = false
	return d
}

// TakeAccountDirty reports whether the display account changed since the last
// call and clears the flag, so the caller can persist it promptly.
func (c *CodexClient) TakeAccountDirty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.accountDirty
	c.accountDirty = false
	return d
}

// jwtExp extracts the exp claim from a JWT access token, or zero if absent.
func jwtExp(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var d struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &d) != nil || d.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(d.Exp, 0)
}

func jwtPayload(token string) map[string]json.RawMessage {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var d map[string]json.RawMessage
	if json.Unmarshal(payload, &d) != nil {
		return nil
	}
	return d
}

func codexAccountFromJWT(token string) string {
	d := jwtPayload(token)
	if len(d) == 0 {
		return ""
	}
	var email, name string
	for _, key := range []string{"email", "https://api.openai.com/email"} {
		_ = json.Unmarshal(d[key], &email)
		if strings.TrimSpace(email) != "" {
			break
		}
	}
	for _, key := range []string{"name", "given_name", "https://api.openai.com/name"} {
		_ = json.Unmarshal(d[key], &name)
		if strings.TrimSpace(name) != "" {
			break
		}
	}
	if email == "" {
		email = stringFromNestedClaim(d, "https://api.openai.com/profile", "email")
	}
	if name == "" {
		name = stringFromNestedClaim(d, "https://api.openai.com/profile", "name")
	}
	email = strings.TrimSpace(email)
	name = strings.TrimSpace(name)
	switch {
	case name != "" && email != "":
		return name + "  ·  " + email
	case email != "":
		return email
	case name != "":
		return name
	default:
		var sub string
		_ = json.Unmarshal(d["sub"], &sub)
		return strings.TrimSpace(sub)
	}
}

func stringFromNestedClaim(d map[string]json.RawMessage, key, field string) string {
	var nested map[string]json.RawMessage
	if json.Unmarshal(d[key], &nested) != nil {
		return ""
	}
	var out string
	_ = json.Unmarshal(nested[field], &out)
	return out
}

func (c *CodexClient) codexHome() (string, error) {
	if c.CodexHome != "" {
		return canonicalPath(c.CodexHome), nil
	}
	if home := strings.TrimSpace(os.Getenv(codexHomeEnvVar)); home != "" {
		return canonicalPath(home), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", &APIError{Msg: "No home directory for Codex credentials"}
	}
	return canonicalPath(filepath.Join(home, ".codex")), nil
}

func canonicalPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

func codexStoreKey(codexHome string) (string, error) {
	if codexHome == "" {
		return "", &APIError{Msg: "No Codex home for keyring lookup"}
	}
	sum := sha256.Sum256([]byte(canonicalPath(codexHome)))
	return fmt.Sprintf("cli|%x", sum)[:20], nil
}

func codexTokensFromFile(path string) (string, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	return codexTokensFromJSON(raw)
}

func codexTokensFromJSON(raw []byte) (string, string, error) {
	var d struct {
		AuthMode     string `json:"auth_mode"`
		OpenAIAPIKey string `json:"OPENAI_API_KEY"`
		Tokens       *struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return "", "", err
	}
	if d.Tokens != nil && strings.TrimSpace(d.Tokens.AccessToken) != "" {
		return strings.TrimSpace(d.Tokens.AccessToken),
			strings.TrimSpace(d.Tokens.RefreshToken), nil
	}
	if d.OpenAIAPIKey != "" || d.AuthMode == "apiKey" || d.AuthMode == "api_key" {
		return "", "", &APIError{Msg: "Codex uses API-key auth — ChatGPT login is required for limits"}
	}
	return "", "", &APIError{Msg: "No Codex ChatGPT access token"}
}

func (c *CodexClient) Usage() ([]byte, *CodexUsage, error) {
	raw, err := c.get("/wham/usage")
	if err != nil {
		return nil, nil, err
	}
	u, err := parseCodexUsage(raw)
	if err != nil {
		return nil, nil, err
	}
	return raw, u, nil
}

func (c *CodexClient) get(path string) ([]byte, error) {
	tok, err := c.token()
	if err != nil {
		log.Printf("GET codex %s -> %v", path, err)
		return nil, err
	}
	body, err := c.do(path, tok)
	// A rejected token usually just means ours expired — drop it and try once
	// more, which mints a fresh one from the refresh token (or, failing that,
	// re-reads the keychain).
	if ae, ok := err.(*APIError); ok && (ae.Status == 401 || ae.Status == 403) {
		c.invalidateAccess()
		if tok, err2 := c.token(); err2 == nil {
			body, err = c.do(path, tok)
		}
	}
	return body, err
}

func (c *CodexClient) do(path, tok string) ([]byte, error) {
	wfmt := metaMark + "%{http_code}|%header{retry-after}"
	log.Printf("GET codex %s", path)
	start := time.Now()
	out, err := exec.Command("curl", "-sS", "--max-time", "15",
		"-H", "Authorization: Bearer "+tok,
		"-H", "User-Agent: "+codexUserAgent,
		"-H", "Accept: application/json",
		"-w", wfmt, codexBase+path).Output()
	took := time.Since(start).Round(time.Millisecond)
	if err != nil {
		log.Printf("GET codex %s -> transport error in %s: %v", path, took, err)
		return nil, &APIError{Msg: "Offline — can't reach Codex API"}
	}
	s := string(out)
	idx := strings.LastIndex(s, metaMark)
	if idx < 0 {
		log.Printf("GET codex %s -> malformed response in %s: %q", path, took, s)
		return nil, &APIError{Msg: "Malformed Codex curl response"}
	}
	body := s[:idx]
	fields := strings.Split(s[idx+len(metaMark):], "|")
	status, convErr := strconv.Atoi(strings.TrimSpace(fields[0]))
	if convErr != nil {
		log.Printf("GET codex %s -> malformed status in %s: %q", path, took, s)
		return nil, &APIError{Msg: "Malformed Codex curl response"}
	}
	if status >= 400 {
		// Dump the body only on an error — it carries the server's reason.
		log.Printf("GET codex %s -> %d in %s (%d bytes): %s", path, status, took, len(body), body)
		e := &APIError{Msg: fmt.Sprintf("HTTP %d", status), Status: status}
		if len(fields) > 1 {
			if ra, err := strconv.ParseFloat(strings.TrimSpace(fields[1]), 64); err == nil {
				e.RetryAfter = int(ra)
			}
		}
		return nil, e
	}
	log.Printf("GET codex %s -> %d in %s (%d bytes)", path, status, took, len(body))
	return []byte(body), nil
}

type codexUsagePayload struct {
	PlanType              string                 `json:"plan_type"`
	RateLimit             *codexRateLimitDetails `json:"rate_limit"`
	Credits               *CodexCredits          `json:"credits"`
	AdditionalRateLimits  []codexAdditionalLimit `json:"additional_rate_limits"`
	RateLimitResetCredits *struct {
		AvailableCount int `json:"available_count"`
	} `json:"rate_limit_reset_credits"`
	RateLimitReachedType *struct {
		Type string `json:"type"`
	} `json:"rate_limit_reached_type"`
}

type codexAdditionalLimit struct {
	LimitName      string                 `json:"limit_name"`
	MeteredFeature string                 `json:"metered_feature"`
	RateLimit      *codexRateLimitDetails `json:"rate_limit"`
}

type codexRateLimitDetails struct {
	PrimaryWindow   *codexWindowSnapshot `json:"primary_window"`
	SecondaryWindow *codexWindowSnapshot `json:"secondary_window"`
}

type codexWindowSnapshot struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

func parseCodexUsage(raw []byte) (*CodexUsage, error) {
	var p codexUsagePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &APIError{Msg: "Malformed Codex usage JSON"}
	}
	u := &CodexUsage{
		PlanType: planOrUnknown(p.PlanType),
		Limits: []CodexLimit{{
			ID:        "codex",
			Primary:   codexWindowFrom(p.RateLimit, true),
			Secondary: codexWindowFrom(p.RateLimit, false),
			Credits:   p.Credits,
		}},
	}
	if p.RateLimitReachedType != nil {
		u.ReachedType = p.RateLimitReachedType.Type
	}
	if p.RateLimitResetCredits != nil {
		u.ResetCredits = p.RateLimitResetCredits.AvailableCount
	}
	for _, l := range p.AdditionalRateLimits {
		id := l.MeteredFeature
		if id == "" {
			id = l.LimitName
		}
		u.Limits = append(u.Limits, CodexLimit{
			ID:        id,
			Name:      l.LimitName,
			Primary:   codexWindowFrom(l.RateLimit, true),
			Secondary: codexWindowFrom(l.RateLimit, false),
		})
	}
	return u, nil
}

func parseCodexUsageCached(raw []byte) *CodexUsage {
	if len(raw) == 0 {
		return nil
	}
	u, err := parseCodexUsage(raw)
	if err != nil {
		return nil
	}
	return u
}

func codexWindowFrom(details *codexRateLimitDetails, primary bool) *CodexWindow {
	if details == nil {
		return nil
	}
	w := details.SecondaryWindow
	if primary {
		w = details.PrimaryWindow
	}
	if w == nil {
		return nil
	}
	return &CodexWindow{
		UsedPercent:   w.UsedPercent,
		WindowSeconds: w.LimitWindowSeconds,
		ResetsAt:      w.ResetAt,
	}
}

func planOrUnknown(plan string) string {
	if plan == "" {
		return "unknown"
	}
	return plan
}
