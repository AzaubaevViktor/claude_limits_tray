package main

import "testing"

func TestForecastCategories(t *testing.T) {
	cases := []struct {
		name             string
		pct, rate, ttr   float64
		known            bool
		wantCat          string
		wantBreak        bool
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
