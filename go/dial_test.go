package main

import (
	"bytes"
	"image/png"
	"testing"
)

func TestMetricMapping(t *testing.T) {
	l := Limit{Pct: 21, TimeLeft: 0.55}
	if got, _ := metricFor("percent_left", l); !nearly(got, 0.79) {
		t.Errorf("percent_left len = %v", got)
	}
	if got, _ := metricFor("time_left", l); !nearly(got, 0.55) {
		t.Errorf("time_left len = %v", got)
	}
	// time_elapsed: length = elapsed, and colour greens with elapsed
	length, health := metricFor("time_elapsed", l)
	if !nearly(length, 0.45) || !nearly(health, 0.45) {
		t.Errorf("time_elapsed = (%v,%v) want (0.45,0.45)", length, health)
	}
}

func TestTimeArcGreenerAsLessTimeLeft(t *testing.T) {
	_, nearlyDone := metricFor("time_elapsed", Limit{0, 0.1}) // little left
	_, justReset := metricFor("time_elapsed", Limit{0, 0.9})  // lots left
	if !(nearlyDone > justReset) {
		t.Errorf("less time left should be higher health: %v vs %v", nearlyDone, justReset)
	}
	if healthColor(nearlyDone) == healthColor(justReset) {
		t.Error("colours should differ")
	}
}

func TestSidesSessionLeftWeeklyRight(t *testing.T) {
	sides := map[string]string{}
	for _, s := range segments {
		sides[s.source] = s.side
	}
	if sides["session"] != "left" || sides["weekly"] != "right" {
		t.Errorf("sides = %v", sides)
	}
}

func TestHealthColorEndpoints(t *testing.T) {
	if healthColor(1) != (dialGreen) {
		t.Error("health 1 -> green")
	}
	if healthColor(0) != (dialRed) {
		t.Error("health 0 -> red")
	}
}

func TestLegendMatchesSegments(t *testing.T) {
	if len(legendRows()) != len(segments)+1 {
		t.Error("legend should be one row per segment + colour")
	}
}

func TestRenderDialProducesPNG(t *testing.T) {
	data := renderDial(blankLimits, [3]int{1, 2, 3}, true)
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not a valid PNG: %v", err)
	}
	if img.Bounds().Dx() != pxSize || img.Bounds().Dy() != pxSize {
		t.Errorf("size = %v, want %dx%d", img.Bounds(), pxSize, pxSize)
	}
}

func nearly(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }
