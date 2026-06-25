package main

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

const (
	fiveHourSecs = 5 * 3600.0
	weekSecs     = 7 * 24 * 3600.0
)

func clamp(x, lo, hi float64) float64 {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

// parseISO parses an ISO-8601 timestamp like "2026-07-01T04:59:59.62+00:00".
func parseISO(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func secondsUntil(resetAt string) (float64, bool) {
	t, ok := parseISO(resetAt)
	if !ok {
		return 0, false
	}
	return t.Sub(time.Now()).Seconds(), true
}

// timeFraction is the fraction of the window still remaining (0..1).
func timeFraction(resetAt string, windowSecs float64) float64 {
	rem, ok := secondsUntil(resetAt)
	if !ok {
		return 0
	}
	return clamp(rem/windowSecs, 0, 1)
}

// workingSecondsUntil counts seconds of working time between now and the reset,
// using the daily [wsMin, weMin] window (minutes from local midnight). The
// window may wrap past midnight (09:00 -> 02:00); end <= start runs into the
// next day. Returns (seconds, true), or (0, false) if resetAt is unparseable.
func workingSecondsUntil(resetAt string, wsMin, weMin int, now time.Time) (float64, bool) {
	t, ok := parseISO(resetAt)
	if !ok {
		return 0, false
	}
	if now.IsZero() {
		now = time.Now()
	}
	loc := now.Location()
	reset := t.In(loc)
	if !reset.After(now) {
		return 0, true
	}
	duration := (((weMin-wsMin)%(24*60))+24*60)%(24*60)
	if duration == 0 {
		duration = 24 * 60
	}
	total := 0.0
	day := now.AddDate(0, 0, -1) // start a day early for overnight windows
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	endDay := time.Date(reset.Year(), reset.Month(), reset.Day(), 0, 0, 0, 0, loc)
	for !day.After(endDay) {
		start := time.Date(day.Year(), day.Month(), day.Day(), wsMin/60, wsMin%60, 0, 0, loc)
		end := start.Add(time.Duration(duration) * time.Minute)
		lo, hi := maxTime(start, now), minTime(end, reset)
		if hi.After(lo) {
			total += hi.Sub(lo).Seconds()
		}
		day = day.AddDate(0, 0, 1)
	}
	return total, true
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// waitSeconds resolves a ratelimit-reset value (ISO timestamp or epoch seconds).
func waitSeconds(resetAt string) (float64, bool) {
	if resetAt == "" {
		return 0, false
	}
	if s, ok := secondsUntil(resetAt); ok {
		return s, true
	}
	if f, err := strconv.ParseFloat(resetAt, 64); err == nil {
		return f - float64(time.Now().Unix()), true
	}
	return 0, false
}

// backoffSeconds: ratelimit-reset header, then retry-after, then an exponential
// floor (capped at 10 min); jitter (0..0.25) only adds on top, never below the
// base. Failures that clear only when something outside us recovers don't back
// off exponentially — they retry once a minute: transport/credential errors
// (status 0 — network down, no token) and auth errors (401/403, waiting for
// Claude Code to refresh the access token).
func backoffSeconds(failCount, status, retryAfter int, resetAt string, jitter float64) float64 {
	if status == 0 || status == 401 || status == 403 {
		return 60
	}
	base := math.Min(600, 15*math.Pow(2, float64(failCount-1)))
	if status == 429 {
		if s, ok := waitSeconds(resetAt); ok && s > 0 {
			base = s
		} else if retryAfter > 0 {
			base = float64(retryAfter)
		} else {
			base = math.Max(60, base)
		}
	}
	return base * (1 + jitter)
}

func fmtRemaining(seconds float64, known bool) string {
	if !known {
		return "—"
	}
	s := int(math.Max(0, seconds))
	if s < 60 {
		return "<1m"
	}
	m := s / 60
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	h, m := m/60, m%60
	if h < 24 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	d, h := h/24, h%24
	return fmt.Sprintf("%dd %dh", d, h)
}

func fmtAgo(seconds float64) string {
	s := int(math.Max(0, seconds))
	if s < 60 {
		return fmt.Sprintf("%ds ago", s)
	}
	m, s := s/60, s%60
	if m < 60 {
		return fmt.Sprintf("%dm %ds ago", m, s)
	}
	h, m := m/60, m%60
	return fmt.Sprintf("%dh %dm ago", h, m)
}

// avgRate is the average consumption since the window opened: the pct used so
// far spread over the elapsed time, expressed per perUnitSecs (3600 -> %/h,
// 86400 -> %/d). leftSecs/known come from the reset time; windowSecs is the
// window length. ok is false when the reset is unknown or less than a minute
// has elapsed (which would blow the rate up right after a reset).
func avgRate(pct, leftSecs, windowSecs, perUnitSecs float64, known bool) (float64, bool) {
	if !known {
		return 0, false
	}
	elapsed := windowSecs - clamp(leftSecs, 0, windowSecs)
	if elapsed < 60 {
		return 0, false
	}
	return pct * perUnitSecs / elapsed, true
}

// forecastRate picks the worse (faster) of two consumption rates, both in % of
// quota per second: the recent 5-minute rate and the average since the window
// opened. A green spark off a quiet last-5-minutes is misleading when the
// long-run pace since reset is high, so the forecast uses whichever is higher.
func forecastRate(fiveMin, pct, leftSecs, windowSecs float64, known bool) float64 {
	rate := fiveMin
	if since, ok := avgRate(pct, leftSecs, windowSecs, 1, known); ok && since > rate {
		rate = since
	}
	return rate
}

// fmtRate renders a rate to one decimal, or "—" when it isn't known yet.
func fmtRate(v float64, ok bool) string {
	if !ok {
		return "—"
	}
	return fmt.Sprintf("%.1f", v)
}
