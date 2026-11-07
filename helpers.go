package main

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

const (
	fiveHourSecs = 5 * 3600.0
	daySecs      = 24 * 3600.0
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
	return workingSecondsBetween(now, t, wsMin, weMin), true
}

// workMinutesPerDay is the length of the daily working window in minutes; an
// empty window (end == start) means the whole day.
func workMinutesPerDay(wsMin, weMin int) int {
	duration := (((weMin - wsMin) % (24 * 60)) + 24*60) % (24 * 60)
	if duration == 0 {
		duration = 24 * 60
	}
	return duration
}

// workDayFraction is the share of each day that is working time.
func workDayFraction(wsMin, weMin int) float64 {
	return float64(workMinutesPerDay(wsMin, weMin)) / (24 * 60)
}

// workingSecondsBetween counts the seconds of working time from one moment to
// another (0 when to is not after from), using the daily [wsMin, weMin]
// window in from's local zone; see workingSecondsUntil.
func workingSecondsBetween(from, to time.Time, wsMin, weMin int) float64 {
	loc := from.Location()
	to = to.In(loc)
	if !to.After(from) {
		return 0
	}
	duration := workMinutesPerDay(wsMin, weMin)
	total := 0.0
	day := from.AddDate(0, 0, -1) // start a day early for overnight windows
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	endDay := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, loc)
	for !day.After(endDay) {
		start := time.Date(day.Year(), day.Month(), day.Day(), wsMin/60, wsMin%60, 0, 0, loc)
		end := start.Add(time.Duration(duration) * time.Minute)
		lo, hi := maxTime(start, from), minTime(end, to)
		if hi.After(lo) {
			total += hi.Sub(lo).Seconds()
		}
		day = day.AddDate(0, 0, 1)
	}
	return total
}

// workSecsBetween is workingSecondsBetween on unix seconds.
func workSecsBetween(from, to int64, wsMin, weMin int) float64 {
	return workingSecondsBetween(time.Unix(from, 0), time.Unix(to, 0), wsMin, weMin)
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
// base. Transport errors (status 0 — network down) retry flat every minute: the
// network may return any moment. Auth errors (401/403) back off exponentially —
// after a failed self-refresh the token is dead until Claude Code is reopened,
// so hammering only risks a 429.
func backoffSeconds(failCount, status, retryAfter int, resetAt string, jitter float64) float64 {
	if status == 0 {
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

// fmtResetClock formats an absolute reset moment as a short local clock time:
// bare "15:04" when it lands today, "Mon 15:04" within the coming week, and
// "Jan 2 15:04" beyond. Empty when the reset time isn't known.
func fmtResetClock(t time.Time, known bool) string {
	if !known {
		return ""
	}
	now := time.Now()
	t = t.In(now.Location())
	switch {
	case sameDay(t, now):
		return t.Format("15:04")
	case t.Sub(now) < 6*24*time.Hour:
		return t.Format("Mon 15:04")
	default:
		return t.Format("Jan 2 15:04")
	}
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// withClock appends the absolute reset moment to a "resets in <dur>" fragment,
// as " · <clock>"; unchanged when the moment isn't known.
func withClock(remaining string, t time.Time, known bool) string {
	if c := fmtResetClock(t, known); c != "" {
		return remaining + " · " + c
	}
	return remaining
}

// fmtSpan renders a measurement span compactly: "45s", "5m", "1h03m".
func fmtSpan(secs float64) string {
	if secs < 60 {
		return fmt.Sprintf("%ds", int(math.Round(math.Max(0, secs))))
	}
	m := int(math.Round(secs / 60))
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%02dm", m/60, m%60)
}

// spanLabel is fmtSpan for a measured span, or the nominal "~15m" window before
// any rate has been measured yet (span 0).
func spanLabel(secs float64) string {
	if secs <= 0 {
		return "~15m"
	}
	return fmtSpan(secs)
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
// quota per second: the recent 15-minute rate and the average since the window
// opened. A green spark off a quiet last 15 minutes is misleading when the
// long-run pace since reset is high, so the forecast uses whichever is higher.
func forecastRate(recent, pct, leftSecs, windowSecs float64, known bool) float64 {
	rate := recent
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

// elide shortens a line to at most n characters, marking the cut with an
// ellipsis so it reads as truncated rather than as the whole message.
func elide(s string, n int) string {
	r := []rune(s)
	if n < 1 || len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
