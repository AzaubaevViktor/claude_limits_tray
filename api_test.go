package main

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestParseCodexUsage(t *testing.T) {
	raw := []byte(`{
		"plan_type": "pro",
		"rate_limit": {
			"primary_window": {
				"used_percent": 42,
				"limit_window_seconds": 300,
				"reset_at": 2000
			},
			"secondary_window": {
				"used_percent": 84,
				"limit_window_seconds": 3600,
				"reset_at": 4000
			}
		},
		"credits": {
			"has_credits": true,
			"unlimited": false,
			"balance": "9.99"
		},
		"additional_rate_limits": [{
			"limit_name": "GPT-5.5",
			"metered_feature": "gpt-5.5",
			"rate_limit": {
				"primary_window": {
					"used_percent": 70,
					"limit_window_seconds": 900,
					"reset_at": 5000
				}
			}
		}],
		"rate_limit_reset_credits": {
			"available_count": 2
		},
		"rate_limit_reached_type": {
			"type": "rate_limit_reached"
		}
	}`)

	u, err := parseCodexUsage(raw)
	if err != nil {
		t.Fatalf("parseCodexUsage: %v", err)
	}
	if u.PlanType != "pro" || u.ResetCredits != 2 || u.ReachedType != "rate_limit_reached" {
		t.Fatalf("summary = %#v", u)
	}
	if len(u.Limits) != 2 {
		t.Fatalf("limits len = %d", len(u.Limits))
	}
	if got := u.Limits[0].Primary.UsedPercent; got != 42 {
		t.Errorf("primary used = %v", got)
	}
	if got := u.Limits[0].Secondary.WindowSeconds; got != 3600 {
		t.Errorf("secondary window = %v", got)
	}
	if u.Limits[0].Credits == nil || u.Limits[0].Credits.Balance == nil ||
		*u.Limits[0].Credits.Balance != "9.99" {
		t.Errorf("credits = %#v", u.Limits[0].Credits)
	}
	if u.Limits[1].ID != "gpt-5.5" || u.Limits[1].Name != "GPT-5.5" {
		t.Errorf("additional limit = %#v", u.Limits[1])
	}
}

func TestCodexStoreKeyShape(t *testing.T) {
	key, err := codexStoreKey("/tmp/codex-home")
	if err != nil {
		t.Fatalf("codexStoreKey: %v", err)
	}
	if !strings.HasPrefix(key, "cli|") || len(key) != 20 {
		t.Fatalf("key = %q", key)
	}
	if again, _ := codexStoreKey("/tmp/codex-home"); again != key {
		t.Fatalf("key not stable: %q vs %q", key, again)
	}
}

func TestCodexTokensFromJSONRejectsAPIKeyOnly(t *testing.T) {
	_, _, err := codexTokensFromJSON([]byte(`{"auth_mode":"apiKey","OPENAI_API_KEY":"sk-test"}`))
	if err == nil || !strings.Contains(err.Error(), "API-key") {
		t.Fatalf("err = %v", err)
	}
}

func TestCodexTokensFromJSONReturnsRefresh(t *testing.T) {
	access, refresh, err := codexTokensFromJSON(
		[]byte(`{"tokens":{"access_token":" a ","refresh_token":" r "}}`))
	if err != nil || access != "a" || refresh != "r" {
		t.Fatalf("access=%q refresh=%q err=%v", access, refresh, err)
	}
}

func TestClaudeCredsFromJSON(t *testing.T) {
	raw := []byte(`{"claudeAiOauth":{"accessToken":"acc","refreshToken":"ref","expiresAt":1782531116490},"organizationUuid":"x"}`)
	a, r, ms := claudeCredsFromJSON(raw)
	if a != "acc" || r != "ref" || ms != 1782531116490 {
		t.Fatalf("wrapped: a=%q r=%q ms=%d", a, r, ms)
	}
	// Flat shape (no claudeAiOauth wrapper).
	if a2, r2, _ := claudeCredsFromJSON([]byte(`{"accessToken":"x","refreshToken":"y"}`)); a2 != "x" || r2 != "y" {
		t.Fatalf("flat: a=%q r=%q", a2, r2)
	}
}

func TestClaudeAdoptKeychainTokensReplacesAccountCredentials(t *testing.T) {
	var c Client
	c.LoadRefresh("old-account-refresh")
	exp := time.Unix(1800000000, 0)
	c.adoptKeychainTokens("new-account-access", "new-account-refresh", exp)

	if c.cachedToken != "new-account-access" || !c.tokenExp.Equal(exp) {
		t.Fatalf("access token not adopted: token=%q exp=%v", c.cachedToken, c.tokenExp)
	}
	if got := c.RefreshToken(); got != "new-account-refresh" {
		t.Fatalf("refresh token = %q", got)
	}
	if !c.TakeRefreshDirty() {
		t.Fatal("replacing the account refresh token should mark state dirty")
	}

	c.adoptKeychainTokens("access-only", "", time.Time{})
	if got := c.RefreshToken(); got != "" {
		t.Fatalf("missing current refresh should clear stale token, got %q", got)
	}
}

func TestClaudeInvalidateDoesNotClearNewerToken(t *testing.T) {
	var c Client
	c.adoptKeychainTokens("new-token", "refresh", time.Time{})
	c.invalidateAccess("old-token")
	if c.cachedToken != "new-token" {
		t.Fatalf("older request cleared current token: %q", c.cachedToken)
	}
	c.invalidateAccess("new-token")
	if c.cachedToken != "" {
		t.Fatalf("rejected current token was not cleared: %q", c.cachedToken)
	}
}

func TestJWTExp(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	tok := enc(`{"alg":"none"}`) + "." + enc(`{"exp":1700000000}`) + ".sig"
	if got := jwtExp(tok); got.Unix() != 1700000000 {
		t.Fatalf("exp = %v", got)
	}
	if got := jwtExp("not-a-jwt"); !got.IsZero() {
		t.Fatalf("non-jwt should be zero, got %v", got)
	}
	if got := jwtExp(enc(`{"alg":"none"}`) + "." + enc(`{"sub":"x"}`) + ".s"); !got.IsZero() {
		t.Fatalf("missing exp should be zero, got %v", got)
	}
}

func TestCodexAccountFromJWT(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	tok := enc(`{"alg":"none"}`) + "." +
		enc(`{"name":"Ada Lovelace","email":"ada@example.com","sub":"user-1"}`) + ".sig"
	if got := codexAccountFromJWT(tok); got != "Ada Lovelace  ·  ada@example.com" {
		t.Fatalf("account = %q", got)
	}

	nested := enc(`{"alg":"none"}`) + "." +
		enc(`{"https://api.openai.com/profile":{"email":"codex@example.com"}}`) + ".sig"
	if got := codexAccountFromJWT(nested); got != "codex@example.com" {
		t.Fatalf("nested account = %q", got)
	}

	fallback := enc(`{"alg":"none"}`) + "." + enc(`{"sub":"acct_123"}`) + ".sig"
	if got := codexAccountFromJWT(fallback); got != "acct_123" {
		t.Fatalf("fallback account = %q", got)
	}
}

func TestStoreBootstrapTokensClearsStaleRefresh(t *testing.T) {
	var c CodexClient
	c.LoadRefresh("old-refresh")
	c.storeBootstrapTokens("access-only", "")
	if got := c.RefreshToken(); got != "" {
		t.Fatalf("refresh should be cleared, got %q", got)
	}
	if !c.TakeRefreshDirty() {
		t.Fatalf("clearing refresh should mark state dirty")
	}
}
