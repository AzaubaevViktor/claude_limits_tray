package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
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
	apiBase   = "https://api.anthropic.com"
	keychain  = "Claude Code-credentials"
	userAgent = "claude-cli/2.1.187 (external, cli)"
	oauthBeta = "oauth-2025-04-20"
	metaMark  = "\n__META__"
)

type Client struct{}

func (c *Client) token() (string, error) {
	out, err := exec.Command("security", "find-generic-password",
		"-s", keychain, "-w").Output()
	if err == nil {
		var d map[string]json.RawMessage
		if json.Unmarshal(out, &d) == nil {
			obj := d
			if inner, ok := d["claudeAiOauth"]; ok {
				_ = json.Unmarshal(inner, &obj)
			}
			var tok string
			if json.Unmarshal(obj["accessToken"], &tok) == nil && tok != "" {
				return tok, nil
			}
		}
	}
	return "", &APIError{Msg: "No credentials — sign in to Claude Code"}
}

func (c *Client) get(path string) ([]byte, error) {
	tok, err := c.token()
	if err != nil {
		return nil, err
	}
	wfmt := metaMark + "%{http_code}|%header{retry-after}" +
		"|%header{anthropic-ratelimit-unified-reset}"
	out, err := exec.Command("curl", "-sS", "--max-time", "15",
		"-H", "Authorization: Bearer "+tok,
		"-H", "anthropic-beta: "+oauthBeta,
		"-H", "User-Agent: "+userAgent,
		"-H", "Accept: application/json",
		"-w", wfmt, apiBase+path).Output()
	if err != nil {
		return nil, &APIError{Msg: "Offline — can't reach Claude API"}
	}
	s := string(out)
	idx := strings.LastIndex(s, metaMark)
	if idx < 0 {
		return nil, &APIError{Msg: "Malformed curl response"}
	}
	body := s[:idx]
	fields := strings.Split(s[idx+len(metaMark):], "|")
	status, convErr := strconv.Atoi(strings.TrimSpace(fields[0]))
	if convErr != nil {
		return nil, &APIError{Msg: "Malformed curl response"}
	}
	if status >= 400 {
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
