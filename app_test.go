package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// Task 3: the default seed drives the dial and menu before any measurement.
func TestDefaultBlockShownBeforeCalibration(t *testing.T) {
	a := newApp()
	a.statePath = filepath.Join(t.TempDir(), "state.json")
	a.calib = &WeeklyCalibrator{}
	a.calibByModel = map[string]*WeeklyCalibrator{}

	if bp := a.weeklyBlockPct(); !near(bp, defaultBlockPct, 0.001) {
		t.Fatalf("default block want %v, got %v", defaultBlockPct, bp)
	}
	if txt := a.weekBlocksText(0); !strings.Contains(txt, "(default)") {
		t.Fatalf("want default tag in menu text, got %q", txt)
	}
	// The dial carries separators straight away (BlockPct > 0).
	a.lastUsage = &Usage{FiveHour: Window{Utilization: 20}, SevenDay: Window{Utilization: 30}}
	limits, _, ok, _ := a.claudeDial()
	if !ok || limits["weekly"].BlockPct <= 0 {
		t.Fatalf("default should draw separators, BlockPct=%v ok=%v", limits["weekly"].BlockPct, ok)
	}
}

// Task 1: per-model rows show the ratio and its Δsession basis once measured.
func TestModelCalibrationText(t *testing.T) {
	a := newApp()
	a.calibByModel = map[string]*WeeklyCalibrator{}

	if got := a.modelBlockText("Opus"); !strings.Contains(got, "default") {
		t.Fatalf("unmeasured model should show default, got %q", got)
	}
	c := a.modelCalib("Opus")
	c.Observe(0, 0)
	c.Observe(10, 2) // Δs=10, Δw=2 -> 20%/wk, basis 10% session
	got := a.modelBlockText("Opus")
	if !strings.Contains(got, "20.0%/wk") || !strings.Contains(got, "basis 10% sess") {
		t.Fatalf("measured model text wrong: %q", got)
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
	a.calib.Restore(8, 1)               // global k=0.125 -> 12.5%/session
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
