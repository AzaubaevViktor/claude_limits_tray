package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	codexBase         = "https://chatgpt.com/backend-api"
	codexAuthService  = "Codex Auth"
	codexUserAgent    = "codex-cli"
	codexAccessEnvVar = "CODEX_ACCESS_TOKEN"
	codexHomeEnvVar   = "CODEX_HOME"
	metaMark          = "\n__META__"
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
		log.Printf("GET %s -> %v", path, err)
		return nil, err
	}
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
	log.Printf("GET %s -> %d in %s (%d bytes): %s", path, status, took, len(body), body)
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

// --- Codex usage API --------------------------------------------------------

type CodexClient struct {
	CodexHome string
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

func (c *CodexClient) token() (string, error) {
	if tok := strings.TrimSpace(os.Getenv(codexAccessEnvVar)); tok != "" {
		return tok, nil
	}
	home, err := c.codexHome()
	if err != nil {
		return "", err
	}
	if tok, err := codexTokenFromFile(filepath.Join(home, "auth.json")); err == nil && tok != "" {
		return tok, nil
	}
	key, err := codexStoreKey(home)
	if err != nil {
		return "", err
	}
	out, err := exec.Command("security", "find-generic-password",
		"-s", codexAuthService, "-a", key, "-w").Output()
	if err == nil {
		if tok, err := codexTokenFromJSON(out); err == nil && tok != "" {
			return tok, nil
		}
	}
	return "", &APIError{Msg: "No Codex ChatGPT credentials — run codex login"}
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

func codexTokenFromFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return codexTokenFromJSON(raw)
}

func codexTokenFromJSON(raw []byte) (string, error) {
	var d struct {
		AuthMode     string `json:"auth_mode"`
		OpenAIAPIKey string `json:"OPENAI_API_KEY"`
		Tokens       *struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return "", err
	}
	if d.Tokens != nil && strings.TrimSpace(d.Tokens.AccessToken) != "" {
		return strings.TrimSpace(d.Tokens.AccessToken), nil
	}
	if d.OpenAIAPIKey != "" || d.AuthMode == "apiKey" || d.AuthMode == "api_key" {
		return "", &APIError{Msg: "Codex uses API-key auth — ChatGPT login is required for limits"}
	}
	return "", &APIError{Msg: "No Codex ChatGPT access token"}
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
	log.Printf("GET codex %s -> %d in %s (%d bytes): %s", path, status, took, len(body), body)
	if status >= 400 {
		e := &APIError{Msg: fmt.Sprintf("HTTP %d", status), Status: status}
		if len(fields) > 1 {
			if ra, err := strconv.ParseFloat(strings.TrimSpace(fields[1]), 64); err == nil {
				e.RetryAfter = int(ra)
			}
		}
		return nil, e
	}
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
