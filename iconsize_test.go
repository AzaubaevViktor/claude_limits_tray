package main

import (
	"testing"
	"time"
)

// Task 7+11: the reference burn (5%/h over the 5-hour window) maxes the level,
// and an equally aggressive burn on any other window scores the same.
func TestWindowBurnLevel(t *testing.T) {
	if got := windowBurnLevel(5.0/3600, fiveHourSecs); !near(got, 1.0, 1e-9) {
		t.Fatalf("5%%/h on 5h window want 1.0, got %v", got)
	}
	if got := windowBurnLevel(0, fiveHourSecs); got != 0 {
		t.Fatalf("idle want 0, got %v", got)
	}
	// Same projected share of a window -> same level, regardless of length.
	if got := windowBurnLevel(25.0/weekSecs, weekSecs); !near(got, 1.0, 1e-9) {
		t.Fatalf("equally aggressive weekly burn want 1.0, got %v", got)
	}
	if got := windowBurnLevel(2.5/3600, fiveHourSecs); !near(got, 0.5, 1e-9) {
		t.Fatalf("half the reference want 0.5, got %v", got)
	}
	if got := windowBurnLevel(50.0/3600, fiveHourSecs); got != 1 {
		t.Fatalf("over the reference should clamp to 1, got %v", got)
	}
}

func TestDynamicIconHeight(t *testing.T) {
	if got := dynamicIconHeight(0); got != dynIconMin {
		t.Fatalf("idle height want %v, got %v", dynIconMin, got)
	}
	if got := dynamicIconHeight(1); got != dynIconMax {
		t.Fatalf("max height want %v, got %v", dynIconMax, got)
	}
	if got := dynamicIconHeight(0.5); !near(got, (dynIconMin+dynIconMax)/2, 1e-9) {
		t.Fatalf("midpoint want %v, got %v", (dynIconMin+dynIconMax)/2, got)
	}
	if got := dynamicIconHeight(5); got != dynIconMax {
		t.Fatalf("over-1 should clamp, got %v", got)
	}
}

func TestTargetIconHeightModes(t *testing.T) {
	a := &App{iconHeight: 16}

	// Auto-size off -> the manual height applies.
	a.dynIconOff = true
	if got := a.targetIconHeight(); got != 16 {
		t.Fatalf("manual mode want 16, got %v", got)
	}

	// Auto-size on, nothing consuming -> idle minimum.
	a.dynIconOff = false
	if got := a.targetIconHeight(); got != dynIconMin {
		t.Fatalf("auto idle want %v, got %v", dynIconMin, got)
	}

	// A weekly-only burn (session idle) still grows the icon: "active at AT
	// LEAST ONE window", each scored on its own scale.
	a.lastUsage = &Usage{}
	// one weekly step, measured over a minute: 25% per weekly window, which
	// projects to the reference. The session series stays empty (idle).
	now := time.Now().Unix()
	a.history = map[string][]histSample{
		seriesClaudeWeekly: {{T: now - 60, V: 0}, {T: now, V: 25.0 / weekSecs * 60}},
	}
	if got := a.targetIconHeight(); !near(got, dynIconMax, 1e-9) {
		t.Fatalf("weekly-only burn want max %v, got %v", dynIconMax, got)
	}
}
