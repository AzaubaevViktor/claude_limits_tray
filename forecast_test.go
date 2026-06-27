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

	rt2 := &RateTracker{}
	rt2.Update(1000, 80, 50)
	if _, _, ok := rt2.Update(1020, 5, 50); ok { // a drop resets
		t.Error("drop should reset and be not-ok")
	}
	if len(rt2.history) != 1 {
		t.Errorf("history should be 1 after reset, got %d", len(rt2.history))
	}
}

func TestWeeklyCalibratorLargestSpendWins(t *testing.T) {
	var c WeeklyCalibrator
	if _, ok := c.BlockPct(); ok {
		t.Fatal("uncalibrated should report not-ok")
	}
	c.Observe(0, 0)   // seed prev
	c.Observe(2, 0.4) // Δs=2, Δw=0.4  -> k=0.20 -> 20%/session
	bp, ok := c.BlockPct()
	if !ok || !near(bp, 20, 0.001) {
		t.Fatalf("after small spend want 20%%, got %v ok=%v", bp, ok)
	}
	// A bigger spend with a different ratio must replace the estimate.
	c.Observe(12, 1.4) // Δs=10, Δw=1.0 -> k=0.10 -> 10%/session
	bp, _ = c.BlockPct()
	if !near(bp, 10, 0.001) {
		t.Fatalf("largest spend should win: want 10%%, got %v", bp)
	}
	// A later smaller spend must NOT override the largest-spend estimate.
	c.Observe(15, 2.0) // Δs=3, Δw=0.6 -> k=0.20, but Δs<10 so ignored
	bp, _ = c.BlockPct()
	if !near(bp, 10, 0.001) {
		t.Fatalf("smaller spend should not override: want 10%%, got %v", bp)
	}
}

func TestWeeklyCalibratorSkipsResetsAndNoise(t *testing.T) {
	var c WeeklyCalibrator
	c.Observe(80, 50)
	c.Observe(5, 50) // session reset (Δs<0) -> skip
	if _, ok := c.BlockPct(); ok {
		t.Fatal("a reset must not register a sample")
	}
	c.Observe(5.5, 50) // Δs=0.5 < calibMinDs -> noise, skip
	if _, ok := c.BlockPct(); ok {
		t.Fatal("sub-threshold jump must not register a sample")
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
