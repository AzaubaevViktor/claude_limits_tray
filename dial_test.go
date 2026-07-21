package main

import (
	"strings"
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
	_, nearlyDone := metricFor("time_elapsed", Limit{Pct: 0, TimeLeft: 0.1}) // little left
	_, justReset := metricFor("time_elapsed", Limit{Pct: 0, TimeLeft: 0.9})  // lots left
	if !(nearlyDone > justReset) {
		t.Errorf("less time left should be higher health: %v vs %v", nearlyDone, justReset)
	}
	if healthColor(nearlyDone) == healthColor(justReset) {
		t.Error("colours should differ")
	}
}

func TestTimeFillTogglesArcNotColour(t *testing.T) {
	defer func() { timeFill = fillElapsed }()
	l := Limit{Pct: 0, TimeLeft: 0.3} // 30% of the window left

	timeFill = fillElapsed
	elapsedLen, elapsedHealth := metricFor("time", l)
	timeFill = fillRemaining
	remainingLen, remainingHealth := metricFor("time", l)

	// Arc length flips between elapsed (0.7) and remaining (0.3)...
	if !nearly(elapsedLen, 0.7) || !nearly(remainingLen, 0.3) {
		t.Errorf("arc lengths = elapsed %v, remaining %v", elapsedLen, remainingLen)
	}
	// ...but the colour (greener as less time is left) is identical.
	if !nearly(elapsedHealth, remainingHealth) || !nearly(elapsedHealth, 0.7) {
		t.Errorf("health should match and be 1-tLeft: %v vs %v", elapsedHealth, remainingHealth)
	}
}

func TestTimeFillRoundTrips(t *testing.T) {
	defer func() { timeFill = fillElapsed }()
	setTimeFillFromString("remaining")
	if timeFill != fillRemaining || timeFillString() != "remaining" {
		t.Errorf("remaining did not round-trip: %v", timeFillString())
	}
	setTimeFillFromString("elapsed")
	if timeFill != fillElapsed || timeFillString() != "elapsed" {
		t.Errorf("elapsed did not round-trip: %v", timeFillString())
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
	if len(legendRows()) != len(segments)+3 {
		t.Error("legend should be one row per segment + three colour rows")
	}
}

func TestRenderDialProducesSVG(t *testing.T) {
	data := renderDial(blankLimits, [3]int{1, 2, 3}, true)
	s := string(data)
	if !strings.HasPrefix(s, `<svg `) {
		t.Fatalf("not an SVG: %q", s[:min(len(s), 32)])
	}
	if !strings.Contains(s, `viewBox="0 0 18.00 18"`) {
		t.Errorf("single dial viewBox missing: %s", s)
	}
}

func TestRenderDualDialProducesWideSVG(t *testing.T) {
	data := renderDualDial(blankLimits, [3]int{1, 2, 3}, true,
		blankLimits, [3]int{4, 5, 6}, true)
	s := string(data)
	if !strings.Contains(s, `viewBox="0 0 38.00 18"`) {
		t.Errorf("dual dial viewBox missing: %s", s)
	}
}

func nearly(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestLimitTimeColor(t *testing.T) {
	// Lots of quota, little time left -> purple (spending under pace).
	if got := limitTimeColor(0.9, 0.2); got != dialPurple {
		t.Errorf("surplus should be purple, got %v", got)
	}
	// Quota and time about equal -> green (on pace).
	if got := limitTimeColor(0.5, 0.5); got != dialGreen {
		t.Errorf("parity should be green, got %v", got)
	}
	// Little quota, lots of time -> red (will run out first).
	if got := limitTimeColor(0.2, 0.9); got != dialRed {
		t.Errorf("deficit should be red, got %v", got)
	}
}

func TestSessionCappedByWeeklyGreyTail(t *testing.T) {
	grey := svgRGB(dialUnavail)
	// Weekly (main) has less left (20%) than session (80% left) -> grey tail.
	capped := map[string]Limit{
		"session": {Pct: 20, TimeLeft: 0.5}, // 80% session left
		"weekly":  {Pct: 80, TimeLeft: 0.5}, // 20% weekly left
	}
	if svg := string(renderDial(capped, [3]int{0, 0, 0}, false)); !strings.Contains(svg, grey) {
		t.Fatalf("session should show a grey unavailable tail when weekly left is smaller:\n%s", svg)
	}
	// Weekly has more left than session -> nothing is capped, no grey.
	uncapped := map[string]Limit{
		"session": {Pct: 80, TimeLeft: 0.5}, // 20% session left
		"weekly":  {Pct: 20, TimeLeft: 0.5}, // 80% weekly left
	}
	if svg := string(renderDial(uncapped, [3]int{0, 0, 0}, false)); strings.Contains(svg, grey) {
		t.Fatalf("no grey tail expected when weekly left exceeds session left:\n%s", svg)
	}
}

func TestFullCircleMode(t *testing.T) {
	in := dialRenderInput{
		limits:  map[string]Limit{"weekly": {Pct: 0, TimeLeft: 0.5, TimeTicks: 7}},
		source:  "weekly",
		full:    true,
		hasMark: false,
	}
	svg := string(renderDialSVG([]dialRenderInput{in}))
	// Two ring tracks + a full outer quota ring (0% used -> 100% left) = 3 circles.
	if got := countSubstr(svg, "<circle"); got < 3 {
		t.Fatalf("full mode should draw closed circles, got %d", got)
	}
	// The semicircle layout draws none.
	half := string(renderDial(map[string]Limit{"weekly": {Pct: 0, TimeLeft: 0.5}}, [3]int{}, false))
	if countSubstr(half, "<circle") != 0 {
		t.Fatal("semicircle mode should draw no closed circles")
	}
}

func TestTimeTickCounts(t *testing.T) {
	if got := hourTicks(fiveHourSecs); got != 5 {
		t.Fatalf("5h window -> 5 hour segments, got %d", got)
	}
	if got := dayTicks(weekSecs); got != 7 {
		t.Fatalf("7d window -> 7 day segments, got %d", got)
	}
	if got := hourTicks(0); got != 0 {
		t.Fatalf("zero window -> 0, got %d", got)
	}
	if got := dayTicks(40 * 86400); got != 24 {
		t.Fatalf("40d should clamp to 24, got %d", got)
	}
}

func TestTimeTicksRendered(t *testing.T) {
	limits := map[string]Limit{
		"session": {Pct: 10, TimeLeft: 0.5, TimeTicks: 5}, // 4 internal ticks
		"weekly":  {Pct: 10, TimeLeft: 0.5, TimeTicks: 7}, // 6 internal ticks
	}
	svg := string(renderDial(limits, [3]int{0, 0, 0}, false))
	if got := countSubstr(svg, "<line"); got != 10 {
		t.Fatalf("want 4+6=10 time ticks, got %d", got)
	}
	// <2 draws nothing.
	limits["session"] = Limit{Pct: 10, TimeLeft: 0.5, TimeTicks: 1}
	limits["weekly"] = Limit{Pct: 10, TimeLeft: 0.5, TimeTicks: 0}
	if got := countSubstr(string(renderDial(limits, [3]int{0, 0, 0}, false)), "<line"); got != 0 {
		t.Fatalf("want 0 ticks for <2 segments, got %d", got)
	}
}
