package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// After a restart the rate history is empty, so a naive tracker needs two fresh
// polls before "Last 5m" can leave zero. Seeding from the last persisted sample
// lets the very first poll measure a rate.
func TestSeedRatesEnablesFirstPollRate(t *testing.T) {
	a := newApp()
	a.lastOk = time.Unix(1000, 0)
	a.lastUsage = &Usage{FiveHour: Window{Utilization: 40}, SevenDay: Window{Utilization: 20}}
	a.seedRates()

	s, w, ok := a.rates.Update(1300, 46, 21) // one poll, 300s after the seed
	if !ok {
		t.Fatal("seeded tracker should compute a rate on the first poll")
	}
	approx(t, s, 6.0/300, 1e-6, "sess rate from seed")
	approx(t, w, 1.0/300, 1e-6, "week rate from seed")
}

// Task 9: no synthetic default — nothing is shown until a real spend is measured.
func TestNoBlockBeforeCalibration(t *testing.T) {
	a := newApp()
	a.statePath = filepath.Join(t.TempDir(), "state.json")
	a.calib = &WeeklyCalibrator{}
	a.calibByModel = map[string]*WeeklyCalibrator{}

	if bp := a.weeklyBlockPct(); bp != 0 {
		t.Fatalf("uncalibrated block should be 0, got %v", bp)
	}
	if txt := a.weekBlocksText(0); !strings.Contains(txt, "calibrating") {
		t.Fatalf("want calibrating placeholder, got %q", txt)
	}
	// The dial draws no separators until measured (BlockPct == 0).
	a.lastUsage = &Usage{FiveHour: Window{Utilization: 20}, SevenDay: Window{Utilization: 30}}
	in, _ := a.claudeDial()
	if in.limits["weekly"].BlockPct != 0 {
		t.Fatalf("uncalibrated should draw no separators, BlockPct=%v", in.limits["weekly"].BlockPct)
	}
}

// Task 14: once measured, the global Claude weekly line shows the Δsession basis.
func TestGlobalWeeklyShowsBasis(t *testing.T) {
	a := newApp()
	a.calib = &WeeklyCalibrator{}
	a.calib.Observe(0, 0)
	a.calib.Observe(20, 2) // Δs=20, Δw=2 -> 10%/wk, basis 20% session
	txt := a.weekBlocksText(0)
	if !strings.Contains(txt, "10.0%/wk") || !strings.Contains(txt, "basis 20% sess") {
		t.Fatalf("global weekly line should show ratio + basis, got %q", txt)
	}
}

// Task 1+9: per-model rows are empty until measured, then show ratio + basis.
func TestModelCalibrationText(t *testing.T) {
	a := newApp()
	a.calibByModel = map[string]*WeeklyCalibrator{}

	if got := a.modelBlockText("Opus"); got != "" {
		t.Fatalf("unmeasured model should show nothing, got %q", got)
	}
	c := a.modelCalib("Opus")
	c.Observe(0, 0)
	c.Observe(10, 2) // Δs=10, Δw=2 -> 20%/wk, basis 10% session
	got := a.modelBlockText("Opus")
	if !strings.Contains(got, "20.0%/wk") || !strings.Contains(got, "basis 10% sess") {
		t.Fatalf("measured model text wrong: %q", got)
	}
}

// Task 7: the auto-size opt-out persists; absent => on (dynIconOff false).
func TestDynIconPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	a := newApp()
	a.statePath = path
	a.dynIconOff = true
	a.saveState(true)

	b := newApp()
	b.statePath = path
	b.dynIconOff = false
	b.loadState()
	if !b.dynIconOff {
		t.Fatal("dynIconOff should persist as true")
	}

	c := newApp()
	c.statePath = filepath.Join(t.TempDir(), "none.json")
	c.loadState()
	if c.dynIconOff {
		t.Fatal("fresh install should default to auto-size ON (dynIconOff false)")
	}
}

// Task 13: Sonnet is never given a 5h-block coefficient.
func TestSonnetSkippedFromCalibration(t *testing.T) {
	if !skipCalibModel("Sonnet") || !skipCalibModel("Claude Sonnet 4.6") {
		t.Fatal("Sonnet should be skipped")
	}
	if skipCalibModel("Claude Opus 4.8") {
		t.Fatal("Opus should be calibrated")
	}
	a := newApp()
	a.calibByModel = map[string]*WeeklyCalibrator{}
	c := a.modelCalib("Sonnet")
	c.Observe(0, 0)
	c.Observe(20, 2)
	if got := a.modelBlockText("Sonnet"); got != "" {
		t.Fatalf("Sonnet should show no coefficient, got %q", got)
	}
}

// Task 5: the steady-state throttle is at least hourly (exit/quit/SIGINT force a
// flush via saveState(true), verified in signalLoop/onExit/the Quit handler).
func TestSaveThrottleAtLeastHourly(t *testing.T) {
	if saveEverySecs < 3600 {
		t.Fatalf("steady-state save should be >= hourly, got %v", saveEverySecs)
	}
}

// Tasks 1+5: global and per-model calibration survive a save/load round-trip.
func TestPersistCalibRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	a := newApp()
	a.statePath = path
	a.calib = &WeeklyCalibrator{}
	a.calibByModel = map[string]*WeeklyCalibrator{}
	a.calib.Restore(16, 2)              // global k=0.125 -> 12.5%/session
	a.modelCalib("Opus").Restore(10, 2) // 20%/session
	a.saveState(true)

	b := newApp()
	b.statePath = path
	b.calib = &WeeklyCalibrator{}
	b.calibByModel = map[string]*WeeklyCalibrator{}
	b.loadState()

	if bp, measured := b.calib.BlockPct(); !measured || !near(bp, 12.5, 0.01) {
		t.Fatalf("global calib not restored: %v measured=%v", bp, measured)
	}
	mc := b.calibByModel["Opus"]
	if mc == nil {
		t.Fatal("per-model calib not restored")
	}
	if bp, measured := mc.BlockPct(); !measured || !near(bp, 20, 0.01) {
		t.Fatalf("model calib not restored: %v measured=%v", bp, measured)
	}
}

func TestSessionLeftTextWeeklyCap(t *testing.T) {
	a := newApp()
	// One session costs 15% of the weekly (ds=60 -> dw=9; a sub-cap sample).
	a.calib.Restore(60, 9)

	// 3% weekly left -> only 3/15 = 20% of the session is usable, though its own
	// budget shows 80% left.
	got := a.sessionLeftText(20, 97) // sessPct=20 (80% left), weekPct=97 (3% left)
	if !strings.Contains(got, "80% left") || !strings.Contains(got, "20% usable (weekly cap)") {
		t.Fatalf("capped session text = %q", got)
	}

	// Plenty of weekly left -> no cap note.
	if got := a.sessionLeftText(80, 20); got != "20% left" {
		t.Fatalf("uncapped session text = %q", got)
	}

	// Not calibrated -> no cap note even when the weekly is low.
	b := newApp()
	if got := b.sessionLeftText(20, 97); got != "80% left" {
		t.Fatalf("uncalibrated session text = %q", got)
	}
}
