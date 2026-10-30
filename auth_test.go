package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

const sampleKeychain = `{"claudeAiOauth":{"accessToken":"old-access","refreshToken":"kc-refresh","expiresAt":1000,
"refreshTokenExpiresAt":5000,"scopes":["user:inference","user:profile"],"subscriptionType":"max","rateLimitTier":"default_claude_max_20x"}}`

const sampleAttrs = "keychain: \"/Users/x/Library/Keychains/login.keychain-db\"\n    \"acct\"<blob>=\"tester\"\n    \"svce\"<blob>=\"Claude Code-credentials\"\n"

// fakeClaudeClient wires a Client to an in-memory keychain item and token
// endpoint. It records refresh tokens posted and keychain commands written.
func fakeClaudeClient(t *testing.T, keychainJSON string) (*Client, *[]string, *[]string) {
	t.Helper()
	var posts, writes []string
	c := &Client{}
	c.kc.run = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) > 0 && args[len(args)-1] == "-w" {
			return []byte(keychainJSON), nil
		}
		return []byte(sampleAttrs), nil
	}
	c.post = func(url string, body []byte) (int, []byte, error) {
		var req map[string]string
		_ = json.Unmarshal(body, &req)
		posts = append(posts, req["refresh_token"])
		if url != claudeTokenURL {
			t.Fatalf("refresh went to %s", url)
		}
		if req["scope"] != "user:inference user:profile" {
			t.Fatalf("scope should follow the keychain item, got %q", req["scope"])
		}
		switch req["refresh_token"] {
		case "kc-refresh":
			return 200, []byte(`{"access_token":"new-access","refresh_token":"rotated","expires_in":28800,"refresh_token_expires_in":2592000}`), nil
		case "limited":
			return 429, []byte(`{"error":"rate_limited"}`), nil
		default:
			return 400, []byte(`{"error":"invalid_grant"}`), nil
		}
	}
	c.write = func(stdin string) error {
		writes = append(writes, stdin)
		return nil
	}
	return c, &posts, &writes
}

func TestClaudeRefreshUsesKeychainTokenAndWritesBack(t *testing.T) {
	c, posts, writes := fakeClaudeClient(t, sampleKeychain)
	c.LoadRefresh("mine-stale") // persisted from an earlier run

	tok, err := c.token()
	if err != nil || tok != "new-access" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	if len(*posts) != 1 || (*posts)[0] != "kc-refresh" {
		t.Fatalf("the keychain's refresh token must be tried first, got %v", *posts)
	}
	if c.RefreshToken() != "rotated" || !c.TakeRefreshDirty() {
		t.Fatalf("rotated refresh token should be stored and marked dirty, got %q", c.RefreshToken())
	}
	if len(*writes) != 1 {
		t.Fatalf("want one keychain write, got %d", len(*writes))
	}
	cmd := (*writes)[0]
	if !strings.HasPrefix(cmd, `add-generic-password -U -a "tester" -s "Claude Code-credentials" -X "`) || !strings.HasSuffix(cmd, "\"\n") {
		t.Fatalf("keychain command = %q", cmd)
	}
	hexPayload := cmd[strings.LastIndex(cmd, `-X "`)+4 : len(cmd)-2]
	payload, err := hex.DecodeString(hexPayload)
	if err != nil {
		t.Fatalf("payload is not hex: %v", err)
	}
	if strings.Contains(cmd, "new-access") || strings.Contains(cmd, "rotated") {
		t.Fatal("tokens must only appear hex-encoded in the security command")
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(payload, &top) != nil {
		t.Fatalf("payload is not JSON: %s", payload)
	}
	var oauth map[string]any
	if json.Unmarshal(top["claudeAiOauth"], &oauth) != nil {
		t.Fatalf("nesting lost: %s", payload)
	}
	if oauth["accessToken"] != "new-access" || oauth["refreshToken"] != "rotated" ||
		oauth["subscriptionType"] != "max" || oauth["rateLimitTier"] != "default_claude_max_20x" {
		t.Fatalf("payload fields wrong: %v", oauth)
	}
	exp, _ := oauth["expiresAt"].(float64)
	if want := float64(time.Now().Add(8 * time.Hour).UnixMilli()); exp < want-5000 || exp > want+5000 {
		t.Fatalf("expiresAt = %v, want ≈ %v", exp, want)
	}
	if rexp, _ := oauth["refreshTokenExpiresAt"].(float64); rexp <= exp {
		t.Fatalf("refreshTokenExpiresAt should be updated from refresh_token_expires_in, got %v", rexp)
	}
	// Second call: the fresh token is cached, no further refresh.
	if tok, _ := c.token(); tok != "new-access" || len(*posts) != 1 {
		t.Fatalf("cached token should be reused, posts=%v", *posts)
	}
}

func TestClaudeRefreshFallsBackToOwnTokenOnRejection(t *testing.T) {
	kc := strings.Replace(sampleKeychain, `"kc-refresh"`, `"dead"`, 1)
	c, posts, _ := fakeClaudeClient(t, kc)
	c.post = func(url string, body []byte) (int, []byte, error) {
		var req map[string]string
		_ = json.Unmarshal(body, &req)
		*posts = append(*posts, req["refresh_token"])
		if req["refresh_token"] == "mine" {
			return 200, []byte(`{"access_token":"a2","expires_in":3600}`), nil
		}
		return 400, []byte(`{"error":"invalid_grant"}`), nil
	}
	c.LoadRefresh("mine")
	tok, err := c.token()
	if err != nil || tok != "a2" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	if len(*posts) != 2 || (*posts)[0] != "dead" || (*posts)[1] != "mine" {
		t.Fatalf("want keychain token then ours, got %v", *posts)
	}
	if c.RefreshToken() != "mine" {
		t.Fatal("an unrotated refresh token stays")
	}
}

func TestClaudeRefreshRateLimitStopsAndThrottles(t *testing.T) {
	kc := strings.Replace(sampleKeychain, `"kc-refresh"`, `"limited"`, 1)
	c, posts, writes := fakeClaudeClient(t, kc)
	c.LoadRefresh("mine")
	tok, err := c.token()
	if err == nil || tok != "" {
		t.Fatalf("an expired keychain token must never be handed out: tok=%q err=%v", tok, err)
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 429 || !ae.Refresh {
		t.Fatalf("want a refresh 429 error, got %#v", err)
	}
	if len(*posts) != 1 {
		t.Fatalf("a 429 must not burn the second candidate, posts=%v", *posts)
	}
	if len(*writes) != 0 {
		t.Fatal("nothing to write back after a failure")
	}
	// Within the gap the same error is returned without another request.
	if _, err2 := c.token(); err2 != err || len(*posts) != 1 {
		t.Fatalf("refresh should be throttled: posts=%v err=%v", *posts, err2)
	}
	c.lastRefreshTry = time.Now().Add(-claudeRefreshMinGap - time.Second)
	if _, _ = c.token(); len(*posts) != 2 {
		t.Fatalf("after the gap a refresh is retried, posts=%v", *posts)
	}
	if describe(err) != "Claude token refresh rate-limited (429)" {
		t.Fatalf("status text = %q", describe(err))
	}
	if !isRefreshError(err) {
		t.Fatal("refresh errors must be distinguishable from usage-API errors")
	}
}

func TestClaudeFreshKeychainTokenIsAdoptedWithoutRefresh(t *testing.T) {
	exp := time.Now().Add(time.Hour).UnixMilli()
	kc := strings.Replace(sampleKeychain, `"expiresAt":1000`, `"expiresAt":`+strconv.FormatInt(exp, 10), 1)
	c, posts, _ := fakeClaudeClient(t, kc)
	tok, err := c.token()
	if err != nil || tok != "old-access" || len(*posts) != 0 {
		t.Fatalf("valid keychain token should be adopted as-is: %q %v posts=%v", tok, err, *posts)
	}
}

func TestWithTokensKeepsFlatShape(t *testing.T) {
	k, ok := parseClaudeKeychain([]byte(`{"accessToken":"a","refreshToken":"r","expiresAt":1,"extra":true}`))
	if !ok || k.nested {
		t.Fatalf("flat item parse = %+v ok=%v", k, ok)
	}
	out, err := k.withTokens(claudeTokenResponse{AccessToken: "a2", RefreshToken: "r2", ExpiresIn: 60}, time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if m["accessToken"] != "a2" || m["refreshToken"] != "r2" || m["extra"] != true || m["expiresAt"] != float64(1060000) {
		t.Fatalf("flat payload = %s", out)
	}
}

func TestKeychainAcctAttr(t *testing.T) {
	out := "keychain: \"x\"\nversion: 512\nclass: \"genp\"\nattributes:\n    0x00000007 <blob>=\"Claude Code-credentials\"\n    \"acct\"<blob>=\"jdoe\"\n    \"svce\"<blob>=\"Claude Code-credentials\"\n"
	if got := keychainAcctAttr(out); got != "jdoe" {
		t.Fatalf("acct = %q", got)
	}
	if got := keychainAcctAttr("nothing"); got != "" {
		t.Fatalf("missing acct = %q", got)
	}
}
