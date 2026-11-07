package main

import (
	"testing"
	"time"
)

func TestQuotaPaceMatchesStatuslineCategories(t *testing.T) {
	const (
		window = 5 * 60 * 60
		waitOK = 30 * 60
	)
	tests := []struct {
		name      string
		pct       float64
		remaining float64
		want      quotaPace
	}{
		{"missing clock", 50, 0, paceUnknown},
		{"fresh window", 1, window, paceUnknown},
		{"first ten percent untrusted", 20, 0.91 * window, paceUnknown},
		{"zero spend has headroom", 0, 0.8 * window, pacePurple},
		{"projects at ninety percent", 18, 0.8 * window, pacePurple},
		{"projects just above ninety percent", 19, 0.8 * window, paceGreen},
		{"runs out with short wait", 95, 0.1 * window, paceAmber},
		{"runs out with long wait", 80, 0.5 * window, paceRed},
		{"already spent near reset", 100, waitOK, paceAmber},
		{"already spent far from reset", 100, waitOK + 1, paceRed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := quotaPaceFor(tc.pct, tc.remaining, window, waitOK); got != tc.want {
				t.Fatalf("quotaPaceFor(%v, %v) = %v, want %v", tc.pct, tc.remaining, got, tc.want)
			}
		})
	}
}

func TestQuotaPaceSurfacesObviousEarlyWeeklyBurn(t *testing.T) {
	const window = 7 * 24 * 60 * 60
	// The real-world case that exposed the blind spot: 21% of a weekly quota
	// consumed in roughly six hours. That is still inside the old 10% learning
	// period (~17h), but there is ample evidence that the pace is dangerous.
	remaining := float64(window - 6*60*60)
	if got := quotaPaceFor(21, remaining, window, 12*60*60); got != paceRed {
		t.Fatalf("21%% in 6h = %v, want red", got)
	}

	// Keep genuinely thin early samples grey: one rounded percentage point in
	// six hours is not enough evidence to colour a weekly projection.
	if got := quotaPaceFor(1, remaining, window, 12*60*60); got != paceUnknown {
		t.Fatalf("1%% in 6h = %v, want unknown", got)
	}
}

func TestWorstQuotaPaceIgnoresUnknown(t *testing.T) {
	if got := worstQuotaPace(paceUnknown, paceGreen, paceAmber); got != paceAmber {
		t.Fatalf("worst known pace = %v, want amber", got)
	}
	if got := worstQuotaPace(paceUnknown, paceUnknown); got != paceUnknown {
		t.Fatalf("all unknown = %v, want unknown", got)
	}
}

func TestRecentBurnRatioUsesLatestRate(t *testing.T) {
	const window = 7 * 24 * 60 * 60
	rate := 20.0 / (6 * 60 * 60) // latest use: 20% in six hours
	if got := recentBurnRatio(rate, window); !near(got, 5.6, 1e-9) {
		t.Fatalf("recent burn ratio = %v, want 5.6x sustainable", got)
	}
}

func TestBudgetRunwayText(t *testing.T) {
	tests := []struct {
		name          string
		usedPct, rate float64
		known         bool
		want          string
	}{
		{"unknown rate", 20, 0, false, "— left"},
		{"idle", 20, 0, true, "∞ left"},
		{"empty", 100, 1, true, "0H left"},
		{"sub-hour fraction", 50, 75.0 / 3600, true, "⅔H left"},
		{"fractional hours", 50, 15.0 / 3600, true, "3⅓H left"},
		{"fractional days", 50, 15.0 / 86400, true, "3⅓D left"},
		{"very short", 99, 12.0 / 3600, true, "<⅓H left"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := budgetRunwayText(100-tc.usedPct, tc.rate, tc.known); got != tc.want {
				t.Fatalf("budgetRunwayText() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestQuotaDurationText(t *testing.T) {
	if got := quotaDurationText(3*3600+20*60, true); got != "3⅓H" {
		t.Fatalf("duration = %q, want 3⅓H", got)
	}
	if got := quotaDurationText(0, false); got != "—" {
		t.Fatalf("unknown duration = %q, want dash", got)
	}
}

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

	// A poll gap longer than the 15-minute window must still yield a rate,
	// measured over the actual gap — not read 0 because only the newest sample
	// falls inside the window.
	rtGap := &RateTracker{}
	rtGap.Update(0, 10, 5)
	sess, week, ok = rtGap.Update(1200, 40, 11) // 1200s > rateWindow (900s)
	if !ok {
		t.Fatal("a poll gap longer than the window should still yield a rate")
	}
	approx(t, sess, 30.0/1200, 1e-6, "sess rate over the real gap")
	approx(t, week, 6.0/1200, 1e-6, "week rate over the real gap")

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
	// Three whole weekly points give enough resolution: Δs=30, Δw=3.
	c.Observe(30, 3)
	bp, ok := c.BlockPct()
	if !ok || !near(bp, 10, 1e-9) {
		t.Fatalf("want 10%%/session, got %v ok=%v", bp, ok)
	}
	if ds, _ := c.Snapshot(); !near(ds, 30, 1e-9) {
		t.Fatalf("basis should be the 30%% span, got %v", ds)
	}
}

func TestWeeklyCalibratorRebaselinesOnReset(t *testing.T) {
	var c WeeklyCalibrator
	c.Observe(10, 5)
	c.Observe(60, 10) // span Δs=50, Δw=5 -> 10%/session
	if bp, _ := c.BlockPct(); !near(bp, 10, 1e-9) {
		t.Fatalf("want 10%%, got %v", bp)
	}
	// Session resets (drops) -> re-baseline; a new span with fewer than three
	// weekly points must not override the retained sample.
	c.Observe(2, 10)
	c.Observe(32, 12) // span Δs=30, Δw=2 -> not enough weekly resolution
	if bp, _ := c.BlockPct(); !near(bp, 10, 1e-9) {
		t.Fatalf("retained sample should persist, got %v", bp)
	}
}

// TestCalibDetectsResetFromLowBaseline reproduces the missed-reset bug: the
// baseline is the window's low point (~0), so a session wrapping 100→0 never
// dips below it. If the reset goes unnoticed, Δweek accrues across many sessions
// while Δsession only tracks the current position, inflating the ratio well past
// 100%. Each session drop must re-anchor the baseline so the block stays ~10%.
func TestCalibDetectsResetFromLowBaseline(t *testing.T) {
	var c WeeklyCalibrator
	c.Observe(0, 2)   // baseline near zero
	c.Observe(90, 11) // one clean session: Δs=90, Δw=9 -> 10%/session
	if bp, ok := c.BlockPct(); !ok || !near(bp, 10, 1e-9) {
		t.Fatalf("want 10%%, got %v ok=%v", bp, ok)
	}
	// Three more sessions wrap the counter back toward zero. Under the old
	// baseline-comparison reset these drops were missed and dw ballooned.
	c.Observe(3, 20)  // reset -> re-baseline at (3,20)
	c.Observe(95, 29) // Δs=92, Δw=9 -> still 10%/session
	c.Observe(4, 38)  // reset -> re-baseline at (4,38)
	c.Observe(28, 45) // partial session, Δw=7 < resolution to replace
	if bp, ok := c.BlockPct(); !ok || bp > 100 {
		t.Fatalf("ratio must never exceed 100%%, got %v ok=%v", bp, ok)
	}
	if bp, _ := c.BlockPct(); !near(bp, 10, 0.5) {
		t.Fatalf("want ~10%% after wraps, got %v", bp)
	}
}

func TestCalibRestoreDropsImpossibleRatio(t *testing.T) {
	var c WeeklyCalibrator
	c.Restore(28, 43) // corrupt sample from the missed-reset bug (dw > ds)
	if _, ok := c.BlockPct(); ok {
		t.Fatal("dw > ds sample (BlockPct > 100) should be dropped on restore")
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

func TestCalibSkipsSaturatedSession(t *testing.T) {
	var c WeeklyCalibrator
	c.Observe(0, 0)  // baseline at the window open
	c.Observe(90, 9) // clean span Δs=90, Δw=9 -> 10%/session
	// The session saturates at the 100% cap while the user keeps working, so the
	// weekly climbs to 15 with the session frozen. That endpoint (100,15) must be
	// ignored — taking it would read as a 15%/session block, over-counting by 50%.
	c.Observe(100, 15)
	bp, ok := c.BlockPct()
	if !ok || !near(bp, 10, 1e-9) {
		t.Fatalf("saturated endpoint should be skipped, want 10%%/session, got %v ok=%v", bp, ok)
	}
	if ds, _ := c.Snapshot(); !near(ds, 90, 1e-9) {
		t.Fatalf("basis should stay the sub-cap 90%% span, got %v", ds)
	}
}

func TestCalibRestoredSaturatedSampleExpires(t *testing.T) {
	now := time.Now()
	var c WeeklyCalibrator
	c.RestoreAt(100, 15, now) // a suspect pre-fix sample remains a short fallback
	if bp, ok := c.BlockPctAt(now); !ok || !near(bp, 15, 1e-9) {
		t.Fatalf("fresh restored fallback should remain, got %v ok=%v", bp, ok)
	}
	if _, ok := c.BlockPctAt(now.Add(calibMaxAge)); ok {
		t.Fatal("restored fallback should expire after one day")
	}
}

func TestCalibFreshSampleNeedsThreeWeeklySteps(t *testing.T) {
	now := time.Now()
	var c WeeklyCalibrator
	c.RestoreAt(100, 10, now) // prior estimate: 1% weekly costs 10% session
	c.ObserveAt(now.Add(time.Minute), 0, 20)
	c.ObserveAt(now.Add(2*time.Minute), 29, 23)
	if bp, _ := c.BlockPctAt(now.Add(2 * time.Minute)); !near(bp, 10, 1e-9) {
		t.Fatalf("29%% session basis should not replace the fallback, got %v", bp)
	}
	c.ObserveAt(now.Add(3*time.Minute), 30, 23)
	if ds, dw := c.Snapshot(); !near(ds, 30, 1e-9) || !near(dw, 3, 1e-9) {
		t.Fatalf("30%%/3%% fresh sample should replace fallback, got ds=%v dw=%v", ds, dw)
	}
}

func TestCalibFreshResolvedWindowReplacesWiderOldSample(t *testing.T) {
	now := time.Now()
	var c WeeklyCalibrator
	c.RestoreAt(90, 9, now)
	c.ObserveAt(now.Add(time.Minute), 5, 20)
	c.ObserveAt(now.Add(2*time.Minute), 45, 25) // fresh 40/5 => 12.5%/session
	if bp, ok := c.BlockPctAt(now.Add(2 * time.Minute)); !ok || !near(bp, 12.5, 1e-9) {
		t.Fatalf("fresh resolved sample should replace wider old one, got %v ok=%v", bp, ok)
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

func TestAffordableBurnRatioUsesQuotaLeftOverTimeLeft(t *testing.T) {
	const window = 5 * 60 * 60.0
	// 50% left with 2.5h to go: 20%/h is exactly what can be afforded.
	if got := affordableBurnRatio(20.0/3600, 50, 2.5*3600, window, true); !near(got, 1, 1e-9) {
		t.Fatalf("on-pace ratio = %v, want 1", got)
	}
	// Little quota, long wait: a modest 10%/h is twice what 20% over 4h allows.
	if got := affordableBurnRatio(10.0/3600, 80, 4*3600, window, true); !near(got, 2, 1e-9) {
		t.Fatalf("scarce-quota ratio = %v, want 2", got)
	}
	// Plenty of quota, reset in 30 minutes: 20%/h barely registers.
	if got := affordableBurnRatio(20.0/3600, 20, 1800, window, true); !near(got, 0.125, 1e-9) {
		t.Fatalf("near-reset ratio = %v, want 0.125", got)
	}
	// Unknown reset falls back to the whole-window nominal pace.
	if got := affordableBurnRatio(20.0/3600, 50, 0, window, false); !near(got, 1, 1e-9) {
		t.Fatalf("fallback ratio = %v, want nominal 1", got)
	}
	if got := affordableBurnRatio(0, 50, 3600, window, true); got != 0 {
		t.Fatalf("idle ratio = %v, want 0", got)
	}
	if got := affordableBurnRatio(1.0/3600, 100, 3600, window, true); got != burnRatioCap {
		t.Fatalf("empty-quota ratio = %v, want capped %v", got, burnRatioCap)
	}
}

func TestNominalPaceRatioMovesWithBudget(t *testing.T) {
	const window = 5 * 60 * 60.0
	// Halfway through time and quota: normal pace is exactly affordable.
	if got, ok := nominalPaceRatio(50, window/2, window, true); !ok || !near(got, 1, 1e-9) {
		t.Fatalf("balanced nominal = %v ok=%v, want 1", got, ok)
	}
	// 80% left with 20% of the time left: can afford 4x the normal pace.
	if got, ok := nominalPaceRatio(20, window/5, window, true); !ok || !near(got, 0.25, 1e-9) {
		t.Fatalf("rich nominal = %v ok=%v, want 0.25", got, ok)
	}
	// 20% left with 80% of the time left: normal pace is 4x too fast.
	if got, ok := nominalPaceRatio(80, 0.8*window, window, true); !ok || !near(got, 4, 1e-9) {
		t.Fatalf("poor nominal = %v ok=%v, want 4", got, ok)
	}
	if _, ok := nominalPaceRatio(50, window/2, window, false); ok {
		t.Fatal("unknown reset should hide the nominal mark")
	}
}
