package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
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
	Refresh    bool   // came from the token-refresh endpoint, not the usage API
}

func (e *APIError) Error() string { return e.Msg }

const (
	apiBase           = "https://api.anthropic.com"
	keychain          = "Claude Code-credentials"
	fallbackCLIVer    = "2.1.322"
	oauthBeta         = "oauth-2025-04-20"
	claudeOAuthClient = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeTokenURL    = "https://platform.claude.com/v1/oauth/token"
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
	kc           keychainGate // stops a refused keychain read from re-prompting
	// A failed self-refresh is not retried within claudeRefreshMinGap, so a
	// rate-limited token endpoint is never hit twice in one poll.
	lastRefreshTry time.Time
	lastRefreshErr error
	// test hooks: post overrides the token-endpoint call, write the keychain
	// update (nil = real curl / security).
	post  func(url string, body []byte) (int, []byte, error)
	write func(stdin string) error
}

const claudeRefreshMinGap = 2 * time.Minute

// claudeDefaultScopes mirrors Claude Code's refresh request when the keychain
// item carries no scope list.
var claudeDefaultScopes = []string{"user:profile", "user:inference",
	"user:sessions:claude_code", "user:mcp_servers", "user:file_upload"}

// claudeKeychainCreds is the parsed `Claude Code-credentials` item. The raw
// objects are kept so a refreshed token pair can be written back with every
// other field (scopes, subscriptionType, rateLimitTier, …) untouched.
type claudeKeychainCreds struct {
	top     map[string]json.RawMessage
	oauth   map[string]json.RawMessage
	nested  bool // tokens live under "claudeAiOauth"
	access  string
	refresh string
	exp     time.Time
	scopes  []string
}

// token returns a usable access token. The keychain is the source of truth —
// Claude Code keeps it fresh while you work, so when its token is still valid we
// use it directly and never interfere. Only when the keychain token is also
// expired (Claude Code idle, e.g. overnight) do we mint a fresh one ourselves
// from the refresh token, the same way Claude Code would, and write the new
// pair back into the keychain so Claude Code picks it up too (refresh tokens
// rotate — leaving the old one in the keychain would strand Claude Code).
func (c *Client) token() (string, error) {
	c.mu.Lock()
	if c.cachedToken != "" && (c.tokenExp.IsZero() || time.Now().Before(c.tokenExp)) {
		tok := c.cachedToken
		c.mu.Unlock()
		return tok, nil
	}
	c.mu.Unlock()

	if creds, ok := c.readKeychain(false); ok {
		if creds.exp.IsZero() || time.Now().Before(creds.exp) {
			c.storeTokens(creds.access, creds.refresh, creds.exp) // adopt Claude Code's fresh token
			return creds.access, nil
		}
		// Keychain token is expired too. Claude Code may have rotated the refresh
		// token since we last saw it, so its copy goes first; ours is the fallback
		// (fresher only if our own last refresh never made it back to the keychain).
		candidates := []string{creds.refresh}
		if mine := c.RefreshToken(); mine != "" && mine != creds.refresh {
			candidates = append(candidates, mine)
		}
		return c.refreshWith(candidates, creds.scopes, creds)
	}
	// No keychain creds — last resort, a refresh token from persisted state.
	if rt := c.RefreshToken(); rt != "" {
		if tok, err := c.refreshWith([]string{rt}, nil, nil); err == nil {
			return tok, nil
		}
	}
	if blocked, reason := c.kc.state(); blocked {
		return "", &APIError{Msg: reason}
	}
	return "", &APIError{Msg: "No credentials — sign in to Claude Code"}
}

// readKeychain reads Claude Code's stored credentials. manual marks a read the
// user asked for from the menu — those may prompt even after an earlier refusal,
// which background reads may not.
func (c *Client) readKeychain(manual bool) (*claudeKeychainCreds, bool) {
	out, err := c.kc.find("Claude", manual, "-s", keychain, "-w")
	if err != nil {
		return nil, false
	}
	return parseClaudeKeychain(out)
}

func parseClaudeKeychain(raw []byte) (*claudeKeychainCreds, bool) {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return nil, false
	}
	k := &claudeKeychainCreds{top: top, oauth: top}
	if inner, ok := top["claudeAiOauth"]; ok {
		var m map[string]json.RawMessage
		if json.Unmarshal(inner, &m) == nil {
			k.oauth, k.nested = m, true
		}
	}
	var ms int64
	_ = json.Unmarshal(k.oauth["accessToken"], &k.access)
	_ = json.Unmarshal(k.oauth["refreshToken"], &k.refresh)
	_ = json.Unmarshal(k.oauth["expiresAt"], &ms)
	_ = json.Unmarshal(k.oauth["scopes"], &k.scopes)
	if k.access == "" {
		return nil, false
	}
	if ms > 0 {
		k.exp = time.UnixMilli(ms).Add(-time.Minute) // a minute of slack
	}
	return k, true
}

// claudeCredsFromJSON is the flat view of parseClaudeKeychain.
func claudeCredsFromJSON(raw []byte) (access, refresh string, expiresMs int64) {
	k, ok := parseClaudeKeychain(raw)
	if !ok {
		return "", "", 0
	}
	if !k.exp.IsZero() {
		expiresMs = k.exp.Add(time.Minute).UnixMilli()
	}
	return k.access, k.refresh, expiresMs
}

// withTokens returns the keychain JSON with the token pair replaced, every
// other field preserved, in the same nesting Claude Code wrote it.
func (k *claudeKeychainCreds) withTokens(res claudeTokenResponse, now time.Time) ([]byte, error) {
	oauth := make(map[string]json.RawMessage, len(k.oauth)+5)
	for key, v := range k.oauth {
		oauth[key] = v
	}
	set := func(key string, v any) {
		b, _ := json.Marshal(v)
		oauth[key] = b
	}
	set("accessToken", res.AccessToken)
	set("refreshToken", res.RefreshToken)
	set("expiresAt", now.Add(time.Duration(res.ExpiresIn)*time.Second).UnixMilli())
	if res.RefreshTokenExpiresIn > 0 {
		set("refreshTokenExpiresAt", now.Add(time.Duration(res.RefreshTokenExpiresIn)*time.Second).UnixMilli())
	}
	if scopes := strings.Fields(res.Scope); len(scopes) > 0 {
		set("scopes", scopes)
	}
	if !k.nested {
		return json.Marshal(oauth)
	}
	top := make(map[string]json.RawMessage, len(k.top))
	for key, v := range k.top {
		top[key] = v
	}
	inner, err := json.Marshal(oauth)
	if err != nil {
		return nil, err
	}
	top["claudeAiOauth"] = inner
	return json.Marshal(top)
}

type claudeTokenResponse struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresIn             int    `json:"expires_in"`
	RefreshTokenExpiresIn int    `json:"refresh_token_expires_in"`
	Scope                 string `json:"scope"`
}

// refreshWith tries each candidate refresh token in turn until one mints a
// token. A rejected token (4xx other than 429) moves on to the next candidate;
// a rate limit or transport failure stops immediately — the other candidate
// would only burn more quota. The result is written back to the keychain when
// the item was read from there.
func (c *Client) refreshWith(candidates []string, scopes []string, creds *claudeKeychainCreds) (string, error) {
	c.mu.Lock()
	if c.lastRefreshErr != nil && time.Since(c.lastRefreshTry) < claudeRefreshMinGap {
		err := c.lastRefreshErr
		c.mu.Unlock()
		return "", err
	}
	c.mu.Unlock()

	var lastErr error
	for _, rt := range candidates {
		if rt == "" {
			continue
		}
		res, err := c.refresh(rt, scopes)
		if err == nil {
			c.noteRefresh(nil)
			if creds != nil {
				c.writeKeychain(creds, res)
			}
			return res.AccessToken, nil
		}
		lastErr = err
		log.Printf("claude token refresh failed: %v", err)
		if ae, ok := err.(*APIError); ok && (ae.Status == 0 || ae.Status == 429 || ae.Status >= 500) {
			break
		}
	}
	if lastErr == nil {
		lastErr = &APIError{Msg: "No Claude refresh token", Refresh: true}
	}
	c.noteRefresh(lastErr)
	return "", lastErr
}

func (c *Client) noteRefresh(err error) {
	c.mu.Lock()
	c.lastRefreshTry, c.lastRefreshErr = time.Now(), err
	c.mu.Unlock()
}

// refresh exchanges the refresh token for a new access token via Anthropic's
// OAuth endpoint — the same request Claude Code sends (endpoint, client id and
// scope list included). The refresh token goes on stdin, never argv, so it
// can't leak into the process list.
func (c *Client) refresh(rt string, scopes []string) (claudeTokenResponse, error) {
	if len(scopes) == 0 {
		scopes = claudeDefaultScopes
	}
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": rt,
		"client_id":     claudeOAuthClient,
		"scope":         strings.Join(scopes, " "),
	})
	log.Printf("claude token refresh")
	status, resp, err := c.postToken(claudeTokenURL, body)
	if err != nil {
		return claudeTokenResponse{}, &APIError{Msg: "Offline — can't refresh Claude token", Refresh: true}
	}
	if status >= 400 {
		e := &APIError{Status: status, Refresh: true}
		switch {
		case status == 429:
			e.Msg = "Claude token refresh rate-limited (429)"
		case status == 400 || status == 401 || status == 403:
			e.Msg = fmt.Sprintf("Claude refresh token rejected (HTTP %d) — open Claude Code to sign in again", status)
		default:
			e.Msg = fmt.Sprintf("Claude token refresh failed (HTTP %d)", status)
		}
		log.Printf("claude token refresh -> %d: %s", status, truncate(resp, 300))
		return claudeTokenResponse{}, e
	}
	var d claudeTokenResponse
	if json.Unmarshal(resp, &d) != nil || d.AccessToken == "" {
		return claudeTokenResponse{}, &APIError{Msg: "Malformed Claude refresh JSON", Refresh: true}
	}
	if d.RefreshToken == "" {
		d.RefreshToken = rt // not rotated
	}
	var exp time.Time
	if d.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(d.ExpiresIn)*time.Second - time.Minute)
	}
	c.storeTokens(d.AccessToken, d.RefreshToken, exp)
	log.Printf("claude token refreshed (expires in %ds, refresh token %s)", d.ExpiresIn,
		map[bool]string{true: "rotated", false: "unchanged"}[d.RefreshToken != rt])
	return d, nil
}

func (c *Client) postToken(url string, body []byte) (int, []byte, error) {
	if c.post != nil {
		return c.post(url, body)
	}
	cmd := exec.Command("curl", "-sS", "--max-time", "15", "-X", "POST",
		"-H", "Content-Type: application/json",
		"-H", "User-Agent: "+userAgent(),
		"-w", metaMark+"%{http_code}",
		"--data-binary", "@-", url)
	cmd.Stdin = bytes.NewReader(body)
	out, err := cmd.Output()
	if err != nil {
		return 0, nil, err
	}
	s := string(out)
	idx := strings.LastIndex(s, metaMark)
	if idx < 0 {
		return 0, nil, fmt.Errorf("malformed curl response")
	}
	status, _ := strconv.Atoi(strings.TrimSpace(s[idx+len(metaMark):]))
	return status, []byte(s[:idx]), nil
}

// truncate shortens a server error body for the log (never a token: error
// bodies carry reasons, not credentials).
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// writeKeychain stores a refreshed token pair back into Claude Code's
// keychain item, exactly as Claude Code does: the update command goes to
// `security -i` on stdin (so the token is not in argv) with the payload hex
// encoded. Failure is logged only — the in-memory token still works, and the
// persisted refresh token is retried from state next time.
func (c *Client) writeKeychain(creds *claudeKeychainCreds, res claudeTokenResponse) {
	payload, err := creds.withTokens(res, time.Now())
	if err != nil {
		log.Printf("claude keychain write skipped: %v", err)
		return
	}
	acct := c.keychainAccount()
	if acct == "" {
		log.Printf("claude keychain write skipped: no account name")
		return
	}
	stdin := fmt.Sprintf("add-generic-password -U -a %q -s %q -X %q\n", acct, keychain, hex.EncodeToString(payload))
	if err := c.runKeychainWrite(stdin); err != nil {
		log.Printf("claude keychain write failed: %v", err)
		return
	}
	log.Printf("claude credentials written back to keychain")
}

func (c *Client) runKeychainWrite(stdin string) error {
	if c.write != nil {
		return c.write(stdin)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "security", "-i")
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, truncate(out, 200))
	}
	return nil
}

// keychainAccount is the account attribute of the credentials item (Claude
// Code uses the login user name), read without touching the secret so no
// password prompt can appear; falls back to the current user.
func (c *Client) keychainAccount() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := c.kc.exec(ctx, []string{"find-generic-password", "-s", keychain}); err == nil {
		if acct := keychainAcctAttr(string(out)); acct != "" {
			return acct
		}
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

// keychainAcctAttr pulls the account out of `security find-generic-password`
// attribute output: a line like `    "acct"<blob>="jdoe"`.
func keychainAcctAttr(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `"acct"`) {
			continue
		}
		if i := strings.Index(line, `="`); i >= 0 && strings.HasSuffix(line, `"`) {
			return line[i+2 : len(line)-1]
		}
	}
	return ""
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
	creds, ok := c.readKeychain(true)
	if !ok {
		return &APIError{Msg: "No credentials — sign in to Claude Code"}
	}
	c.adoptKeychainTokens(creds.access, creds.refresh, creds.exp)
	c.noteRefresh(nil) // a fresh login may refresh at once
	log.Printf("claude credentials reloaded from keychain")
	return nil
}

// ForceRefresh mints a new token pair right now from the keychain's refresh
// token (ours as fallback) and writes it back, regardless of whether the
// current access token is still valid. Used by the menu action.
func (c *Client) ForceRefresh() error {
	creds, ok := c.readKeychain(true)
	if !ok {
		return &APIError{Msg: "No credentials — sign in to Claude Code"}
	}
	candidates := []string{creds.refresh}
	if mine := c.RefreshToken(); mine != "" && mine != creds.refresh {
		candidates = append(candidates, mine)
	}
	c.noteRefresh(nil) // a manual request bypasses the retry gap
	_, err := c.refreshWith(candidates, creds.scopes, creds)
	return err
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
		"-H", "User-Agent: "+userAgent(),
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
	// NoSession is set when the response carries no 5-hour window at all
	// (five_hour null and no "session" limit): the account is weekly-only, so
	// the session half of the UI is hidden rather than drawn as an empty 100%.
	NoSession bool `json:"-"`
	// Credit budgets, kept raw and parsed leniently (see claudeCredits) so an
	// unexpected shape can never fail the whole usage response.
	ExtraUsage json.RawMessage `json:"extra_usage,omitempty"`
	Spend      json.RawMessage `json:"spend,omitempty"`
	// Rate-limit reset credits, if Claude ever exposes them the way Codex
	// does (rate_limit_reset_credits.available_count). Read leniently.
	ResetCredits json.RawMessage `json:"rate_limit_reset_credits,omitempty"`
	// The limit-reset block (grants of resets the account holds), present only
	// when asked for — see usagePath.
	LimitResets json.RawMessage `json:"cedar_ember,omitempty"`
}

// claudeVersionsDir is where Claude Code keeps its installed builds, one entry
// per version. A var so tests can point it elsewhere.
var claudeVersionsDir = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "claude", "versions")
}

// userAgent names the Claude Code build installed on this machine. The server
// gates parts of the usage response on the CLI version (limit resets come back
// "ineligible: cli_version" for an old one), so a hard-coded version silently
// goes stale; the installed one keeps pace with Claude Code's own updates.
func userAgent() string {
	return "claude-cli/" + installedCLIVersion(claudeVersionsDir()) + " (external, cli)"
}

// installedCLIVersion is the highest x.y.z entry in dir, never lower than the
// version this build was written against.
func installedCLIVersion(dir string) string {
	best, bestV := fallbackCLIVer, parseSemver(fallbackCLIVer)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		v, ok := parseSemverOK(e.Name())
		if !ok {
			continue
		}
		if semverLess(bestV, v) {
			best, bestV = e.Name(), v
		}
	}
	return best
}

func parseSemver(s string) [3]int { v, _ := parseSemverOK(s); return v }

func parseSemverOK(s string) ([3]int, bool) {
	var v [3]int
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

func semverLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// usagePath asks for the limit-reset block along with the usage: without the
// flag the response carries "cedar_ember": null whatever the account holds.
const usagePath = "/api/oauth/usage?cedar_ember=1"

// resetGrant is one batch of limit resets the account holds. Clears lists the
// windows a reset refills: a general one names seven_day (and/or five_hour), a
// model-scoped one only that model's own weekly.
type resetGrant struct {
	Label      string   `json:"label"`
	ResetsLeft int      `json:"resets_left"`
	Clears     []string `json:"clears"`
}

func (g resetGrant) general() bool {
	for _, c := range g.Clears {
		if c == "seven_day" || c == "five_hour" {
			return true
		}
	}
	return len(g.Clears) == 0
}

// claudeResetsText is the "Resets available" row for Claude. The grants in the
// limit-reset block are counted and split into general resets and per-model
// ones ("2  ·  1 general, 1 Fable"); failing that, a Codex-style
// rate_limit_reset_credits count is used; "—" when neither is known.
func claudeResetsText(u *Usage) string {
	if u == nil {
		return "Resets available: —"
	}
	if text, ok := claudeGrantsText(u); ok {
		return text
	}
	if len(u.ResetCredits) == 0 {
		return "Resets available: —"
	}
	var obj struct {
		Available json.RawMessage `json:"available_count"`
	}
	raw := u.ResetCredits
	if json.Unmarshal(raw, &obj) == nil && len(obj.Available) > 0 {
		raw = obj.Available
	}
	if n, _, ok := lenientAmount(raw); ok {
		return fmt.Sprintf("Resets available: %d", int(n))
	}
	return "Resets available: —"
}

func claudeGrantsText(u *Usage) (string, bool) {
	var block struct {
		Grants []resetGrant `json:"grants"`
	}
	raw := strings.TrimSpace(string(u.LimitResets))
	if raw == "" || raw == "null" || json.Unmarshal(u.LimitResets, &block) != nil {
		return "", false
	}
	total := 0
	var kinds []string
	counts := map[string]int{}
	for _, g := range block.Grants {
		if g.ResetsLeft <= 0 {
			continue
		}
		kind := "general"
		if !g.general() {
			kind = u.scopedGrantName(g)
		}
		if counts[kind] == 0 {
			kinds = append(kinds, kind)
		}
		counts[kind] += g.ResetsLeft
		total += g.ResetsLeft
	}
	// A block with no grants left still answers the question: zero.
	if len(kinds) < 2 && counts["general"] == total {
		return fmt.Sprintf("Resets available: %d", total), true
	}
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
	}
	return fmt.Sprintf("Resets available: %d  ·  %s", total, strings.Join(parts, ", ")), true
}

// scopedGrantName names the model a scoped grant refills: the model-scoped
// weekly limit its label mentions, else the only such limit, else "model".
func (u *Usage) scopedGrantName(g resetGrant) string {
	var names []string
	for _, l := range u.Limits {
		if l.Kind != "weekly_scoped" || l.Scope == nil || l.Scope.Model == nil || l.Scope.Model.DisplayName == "" {
			continue
		}
		names = append(names, l.Scope.Model.DisplayName)
	}
	label := strings.ToLower(g.Label)
	for _, n := range names {
		if strings.Contains(label, strings.ToLower(n)) {
			return n
		}
	}
	if len(names) == 1 {
		return names[0]
	}
	return "model"
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
	raw, err := c.get(usagePath)
	if err != nil {
		return nil, nil, err
	}
	u, err := decodeUsage(raw)
	if err != nil {
		return nil, nil, &APIError{Msg: "Malformed usage JSON"}
	}
	return raw, u, nil
}

// decodeUsage parses a usage response and notes whether a 5-hour window came
// with it. The window counts as present when five_hour is a non-null object
// or a "session"-kind limit is listed.
func decodeUsage(raw []byte) (*Usage, error) {
	var u Usage
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, err
	}
	var probe struct {
		FiveHour json.RawMessage `json:"five_hour"`
	}
	_ = json.Unmarshal(raw, &probe)
	fiveHour := strings.TrimSpace(string(probe.FiveHour))
	u.NoSession = fiveHour == "" || fiveHour == "null"
	for _, l := range u.Limits {
		if l.Kind == "session" {
			u.NoSession = false
		}
	}
	return &u, nil
}

// HasSession reports whether the account has a 5-hour window to show.
func (u *Usage) HasSession() bool { return u != nil && !u.NoSession }

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
	u, err := decodeUsage(raw)
	if err != nil {
		return nil
	}
	return u
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
	kc           keychainGate
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
	PlanType          string
	ReachedType       string
	ResetCredits      int
	ResetCreditsKnown bool
	Limits            []CodexLimit
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
// keychain (which is the one that can prompt). manual marks a read the user
// asked for from the menu; background reads are refused once the keychain gate
// has latched, so a dismissed password dialog isn't re-shown every poll.
func (c *CodexClient) bootstrapCreds(manual bool) (*codexBootstrapCreds, error) {
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
	out, err := c.kc.find("Codex", manual, "-s", codexAuthService, "-a", key, "-w")
	if err == nil {
		if access, refresh, err := codexTokensFromJSON(out); err == nil && access != "" {
			return &codexBootstrapCreds{
				access:  access,
				refresh: refresh,
				source:  "keychain " + codexAuthService,
			}, nil
		}
	}
	// A refused or latched keychain read is the more useful thing to report; a
	// missing item means there is genuinely nothing to log in with.
	if blocked, reason := c.kc.state(); blocked {
		return nil, &APIError{Msg: reason}
	}
	return nil, &APIError{Msg: "No Codex ChatGPT credentials — run codex login"}
}

// bootstrap reads current Codex credentials and caches them so the next poll can
// refresh silently instead of hitting the keychain again.
func (c *CodexClient) bootstrap() (string, error) {
	creds, err := c.bootstrapCreds(false)
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
	creds, err := c.bootstrapCreds(true)
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
		u.ResetCreditsKnown = true
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
