package main

import "testing"

func TestForecastCategories(t *testing.T) {
	cases := []struct {
		name           string
		pct, rate, ttr float64
		known          bool
		wantCat        string
		wantBreak      bool
	}{
		{"idle", 20, 0, 2 * 3600, true, "purple", false},
		{"plenty", 20, 10.0 / 3600, 2 * 3600, true, "purple", false},
		{"exact", 50, 25.0 / 3600, 2 * 3600, true, "green", false},
		{"small overshoot", 60, 30.0 / 3600, 2 * 3600, true, "yellow", true},
		{"big overshoot", 80, 40.0 / 3600, 4 * 3600, true, "red", true},
		{"no reset", 90, 1, 0, false, "purple", false},
	}
	for _, c := range cases {
		fc := projectForecast(c.pct, c.rate, c.ttr, c.known)
		if fc.Category != c.wantCat || fc.HasBreak != c.wantBreak {
			t.Errorf("%s: got %q hasBreak=%v want %q/%v", c.name,
				fc.Category, fc.HasBreak, c.wantCat, c.wantBreak)
		}
	}
}

func TestForecastPosMonotonic(t *testing.T) {
	p := projectForecast(10, 0, 3600, true).Pos()
	g := projectForecast(50, 25.0/3600, 2*3600, true).Pos()
	y := projectForecast(60, 30.0/3600, 2*3600, true).Pos()
	r := projectForecast(80, 40.0/3600, 4*3600, true).Pos()
	if !(p < g && g < y && y < r) {
		t.Errorf("pos not monotonic: %v %v %v %v", p, g, y, r)
	}
}

func TestGradientEndpoints(t *testing.T) {
	if gradient(0) != [3]int{175, 82, 222} {
		t.Error("pos 0 should be purple")
	}
	if gradient(1) != [3]int{255, 69, 58} {
		t.Error("pos 1 should be red")
	}
}

func TestSpeedColor(t *testing.T) {
	idle := projectForecast(20, 0, 3600, true)
	overshoot := projectForecast(80, 40.0/3600, 4*3600, true)

	// No spend right now -> grey, regardless of the forecast.
	if got := speedColor(idle, overshoot, false); got != dialGray {
		t.Fatalf("inactive should be grey, got %v", got)
	}
	// Relaxed pace collapses onto green (never purple/head-room on the centre).
	if got := speedColor(idle, idle, true); got != dialGreen {
		t.Fatalf("relaxed active should be green, got %v", got)
	}
	// A big overshoot in either window drives the dot red-ward (red high, green
	// low) — distinct from the green "on pace" dot.
	if got := speedColor(idle, overshoot, true); got[0] < 200 || got[1] > 150 {
		t.Fatalf("overshoot should be red-ward, got %v", got)
	}
}

func TestForecastText(t *testing.T) {
	if txt := projectForecast(20, 0, 3600, true).Text(); !contains(txt, "spare") {
		t.Errorf("purple text = %q", txt)
	}
	if txt := projectForecast(80, 40.0/3600, 4*3600, true).Text(); !contains(txt, "pause") {
		t.Errorf("over text = %q", txt)
	}
}

func TestSpareText(t *testing.T) {
	// 50% used, burning 25%/h for 2h -> 50% used + 50% burned = 0% spare.
	if got := spareText(50, 25.0/3600, 2*3600, true); got != "0%" {
		t.Errorf("on-track spare = %q, want 0%%", got)
	}
	// 60% used, burning 30%/h for 2h -> 40% left, 60% projected = -20% spare.
	if got := spareText(60, 30.0/3600, 2*3600, true); got != "-20%" {
		t.Errorf("overshoot spare = %q, want -20%%", got)
	}
	// Idle at 20% used -> all 80% remaining is spare.
	if got := spareText(20, 0, 3600, true); got != "80%" {
		t.Errorf("idle spare = %q, want 80%%", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestRateTracker(t *testing.T) {
	rt := &RateTracker{}
	if _, _, ok := rt.Update(1000, 10, 2); ok {
		t.Error("first sample should be not-ok")
	}
	if _, _, ok := rt.Update(1005, 11, 2); ok {
		t.Error("dt<10 should be not-ok")
	}
	sess, week, ok := rt.Update(1020, 30, 2)
	if !ok {
		t.Fatal("should be ok after 20s")
	}
	approx(t, sess, 1.0, 1e-3, "sess rate")
	approx(t, week, 0.0, 1e-3, "week rate")

	// A poll gap longer than the 5-minute window must still yield a rate,
	// measured over the actual gap — not read 0 because only the newest sample
	// falls inside the window.
	rtGap := &RateTracker{}
	rtGap.Update(0, 10, 5)
	sess, week, ok = rtGap.Update(600, 40, 11) // 600s > rateWindow (300s)
	if !ok {
		t.Fatal("a poll gap longer than the window should still yield a rate")
	}
	approx(t, sess, 30.0/600, 1e-6, "sess rate over the real gap")
	approx(t, week, 6.0/600, 1e-6, "week rate over the real gap")

	rt2 := &RateTracker{}
	rt2.Update(1000, 80, 50)
	if _, _, ok := rt2.Update(1020, 5, 50); ok { // a drop resets
		t.Error("drop should reset and be not-ok")
	}
	if len(rt2.history) != 1 {
		t.Errorf("history should be 1 after reset, got %d", len(rt2.history))
	}
}

func TestWeeklyCalibratorCumulativeBasis(t *testing.T) {
	var c WeeklyCalibrator
	if _, ok := c.BlockPct(); ok {
		t.Fatal("uncalibrated should report not-ok")
	}
	c.Observe(0, 0)   // baseline at the window open
	c.Observe(3, 0.6) // span Δs=3 < calibMinDs -> the noisy small-basis case, skip
	if _, ok := c.BlockPct(); ok {
		t.Fatal("a small basis must not calibrate")
	}
	// Across a whole session the span is large: Δs=20, Δw=2 -> 10%/session.
	c.Observe(20, 2)
	bp, ok := c.BlockPct()
	if !ok || !near(bp, 10, 1e-9) {
		t.Fatalf("want 10%%/session, got %v ok=%v", bp, ok)
	}
	if ds, _ := c.Snapshot(); !near(ds, 20, 1e-9) {
		t.Fatalf("basis should be the 20%% span, got %v", ds)
	}
}

func TestWeeklyCalibratorRebaselinesOnReset(t *testing.T) {
	var c WeeklyCalibrator
	c.Observe(10, 5)
	c.Observe(60, 10) // span Δs=50, Δw=5 -> 10%/session
	if bp, _ := c.BlockPct(); !near(bp, 10, 1e-9) {
		t.Fatalf("want 10%%, got %v", bp)
	}
	// Session resets (drops) -> re-baseline; a small new span must not override
	// the larger-basis sample from the previous window.
	c.Observe(2, 10)
	c.Observe(13, 10.4) // span Δs=11, Δw=0.4 -> ~3.6%, but basis 11 < 50 -> kept
	if bp, _ := c.BlockPct(); !near(bp, 10, 1e-9) {
		t.Fatalf("largest-basis sample should persist, got %v", bp)
	}
}

func TestCalibRestoreDropsTinyBasis(t *testing.T) {
	var c WeeklyCalibrator
	c.Restore(4, 1) // the old noisy 25%/session sample (basis 4 < floor)
	if _, ok := c.BlockPct(); ok {
		t.Fatal("stale sub-threshold sample should be dropped on restore")
	}
	c.Restore(20, 2) // a solid large-basis sample is kept
	if bp, ok := c.BlockPct(); !ok || !near(bp, 10, 1e-9) {
		t.Fatalf("solid sample should restore, got %v ok=%v", bp, ok)
	}
}

func TestWeeklyBlockSeparatorsRendered(t *testing.T) {
	limits := map[string]Limit{
		"session": {Pct: 10, TimeLeft: 0.5},
		"weekly":  {Pct: 10, TimeLeft: 0.5, BlockPct: 10}, // 10 blocks -> 9 ticks
	}
	svg := string(renderDial(limits, [3]int{0, 0, 0}, false))
	if got := countSubstr(svg, "<line"); got != 9 {
		t.Fatalf("want 9 block separators for 10%% blocks, got %d", got)
	}
	// No BlockPct -> no separators.
	limits["weekly"] = Limit{Pct: 10, TimeLeft: 0.5}
	if got := countSubstr(string(renderDial(limits, [3]int{0, 0, 0}, false)), "<line"); got != 0 {
		t.Fatalf("want 0 separators without BlockPct, got %d", got)
	}
}

func near(a, b, eps float64) bool { return a-b < eps && b-a < eps }

func countSubstr(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}
