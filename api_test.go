package main

import (
	"strings"
	"testing"
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

func TestCodexTokenFromJSONRejectsAPIKeyOnly(t *testing.T) {
	_, err := codexTokenFromJSON([]byte(`{"auth_mode":"apiKey","OPENAI_API_KEY":"sk-test"}`))
	if err == nil || !strings.Contains(err.Error(), "API-key") {
		t.Fatalf("err = %v", err)
	}
}
