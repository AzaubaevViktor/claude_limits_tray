package main

import (
	"fmt"
	"math"
	"testing"
	"time"
)

func approx(t *testing.T, got, want, tol float64, msg string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s: got %v, want %v (±%v)", msg, got, want, tol)
	}
}

func TestFmtRemaining(t *testing.T) {
	cases := []struct {
		s     float64
		known bool
		want  string
	}{
		{0, false, "—"}, {0, true, "<1m"}, {30, true, "<1m"}, {90, true, "1m"},
		{90 * 60, true, "1h30m"}, {50 * 3600, true, "2d 2h"},
	}
	for _, c := range cases {
		if got := fmtRemaining(c.s, c.known); got != c.want {
			t.Errorf("fmtRemaining(%v,%v)=%q want %q", c.s, c.known, got, c.want)
		}
	}
}

func TestFmtAgo(t *testing.T) {
	for _, c := range []struct {
		s    float64
		want string
	}{{5, "5s ago"}, {75, "1m 15s ago"}, {3700, "1h 1m ago"}, {-3, "0s ago"}} {
		if got := fmtAgo(c.s); got != c.want {
			t.Errorf("fmtAgo(%v)=%q want %q", c.s, got, c.want)
		}
	}
}

func TestSecondsUntilAndFraction(t *testing.T) {
	future := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	s, ok := secondsUntil(future)
	if !ok {
		t.Fatal("secondsUntil not ok")
	}
	approx(t, s, 3600, 5, "secondsUntil")
	approx(t, timeFraction(future, fiveHourSecs), 0.2, 0.01, "timeFraction")
	if timeFraction("", weekSecs) != 0 {
		t.Error("timeFraction of empty should be 0")
	}
}

func TestWaitSeconds(t *testing.T) {
	if _, ok := waitSeconds(""); ok {
		t.Error("empty should not be ok")
	}
	epoch := fmt.Sprintf("%d", time.Now().Unix()+120)
	s, _ := waitSeconds(epoch)
	approx(t, s, 120, 5, "waitSeconds epoch")
	iso := time.Now().Add(90 * time.Second).Format(time.RFC3339Nano)
	s, _ = waitSeconds(iso)
	approx(t, s, 90, 5, "waitSeconds iso")
}

func resetFrom(base time.Time, d time.Duration) string {
	return base.Add(d).Format(time.RFC3339Nano)
}

func TestWorkingSeconds(t *testing.T) {
	noon := time.Date(2026, 6, 25, 12, 0, 0, 0, time.Local)

	ws, _ := workingSecondsUntil(resetFrom(noon, 5*time.Hour), 9*60, 18*60, noon)
	approx(t, ws, 5*3600, 1, "same-day 12->17")

	at17 := time.Date(2026, 6, 25, 17, 0, 0, 0, time.Local)
	ws, _ = workingSecondsUntil(resetFrom(at17, 5*time.Hour), 9*60, 18*60, at17)
	approx(t, ws, 3600, 1, "clamped to 18:00")

	ws, _ = workingSecondsUntil(resetFrom(noon, 7*24*time.Hour), 9*60, 18*60, noon)
	approx(t, ws/3600, 7*9, 0.1, "weekly 9h/day")

	ws, _ = workingSecondsUntil(resetFrom(noon, 24*time.Hour), 9*60, 2*60, noon)
	approx(t, ws/3600, 17, 0.1, "overnight 17h")

	at1 := time.Date(2026, 6, 25, 1, 0, 0, 0, time.Local)
	ws, _ = workingSecondsUntil(resetFrom(at1, 2*time.Hour), 9*60, 2*60, at1)
	approx(t, ws/3600, 1, 0.05, "overnight from previous day")

	if ws, _ := workingSecondsUntil(resetFrom(noon, -time.Hour), 9*60, 18*60, noon); ws != 0 {
		t.Errorf("reset in past should be 0, got %v", ws)
	}
}

func TestBackoff(t *testing.T) {
	if backoffSeconds(1, 500, 0, "", 0) != 15 {
		t.Error("fail1 -> 15")
	}
	if backoffSeconds(3, 500, 0, "", 0) != 60 {
		t.Error("fail3 -> 60")
	}
	if backoffSeconds(99, 500, 0, "", 0) != 600 {
		t.Error("cap 600")
	}
	// Network/credential failures (status 0): flat 60s, no exponential, no jitter.
	if backoffSeconds(1, 0, 0, "", 0.25) != 60 {
		t.Error("network fail1 -> 60")
	}
	if backoffSeconds(50, 0, 0, "", 0.25) != 60 {
		t.Error("network never escalates -> 60")
	}
	// Auth errors (401/403): flat 60s too — wait for Claude Code to refresh.
	if backoffSeconds(1, 401, 0, "", 0.25) != 60 || backoffSeconds(9, 403, 0, "", 0.25) != 60 {
		t.Error("auth errors -> 60")
	}
	if backoffSeconds(1, 429, 0, "", 0) != 60 {
		t.Error("429 floor 60")
	}
	if backoffSeconds(1, 429, 300, "", 0) != 300 {
		t.Error("429 uses retry-after")
	}
	reset := time.Now().Add(200 * time.Second).Format(time.RFC3339Nano)
	approx(t, backoffSeconds(1, 429, 30, reset, 0), 200, 5, "429 reset header wins")
	base := backoffSeconds(1, 429, 100, "", 0)
	hi := backoffSeconds(1, 429, 100, "", 0.25)
	if base != 100 || hi < base {
		t.Errorf("jitter only adds: base=%v hi=%v", base, hi)
	}
	approx(t, hi, 125, 1e-6, "jitter 0.25")
}
