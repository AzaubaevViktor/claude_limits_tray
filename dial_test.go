package main

import (
	"fmt"
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
	if len(legendRows()) != len(segments)+5 {
		t.Error("legend should be one row per segment + four colour rows + the spend pulse")
	}
}

func TestRenderDialProducesSVG(t *testing.T) {
	data := renderDial(blankLimits, [3]int{1, 2, 3}, true)
	s := string(data)
	if !strings.HasPrefix(s, `<svg `) {
		t.Fatalf("not an SVG: %q", s[:min(len(s), 32)])
	}
	// The default mode throws sparks, so the box carries sparkPad on each side.
	if want := fmt.Sprintf(`viewBox="0 0 %.2f 18"`, 18+2*sparkPad); !strings.Contains(s, want) {
		t.Errorf("single dial viewBox %s missing: %s", want, s)
	}
}

// Sparks are the only thing that needs room around the dial: with them off the
// icon must be exactly as wide as it always was.
func TestPulseModeSetsTheCanvasPadding(t *testing.T) {
	defer func(m pulseMode) { pulseFX = m }(pulseFX)
	for _, tc := range []struct {
		mode pulseMode
		want string
	}{
		{pulseOff, `viewBox="0 0 18.00 18"`},
		{pulseGlow, `viewBox="0 0 18.00 18"`},
		{pulseSparks, fmt.Sprintf(`viewBox="0 0 %.2f 18"`, 18+2*sparkPad)},
	} {
		pulseFX = tc.mode
		if s := string(renderDial(blankLimits, [3]int{1, 2, 3}, true)); !strings.Contains(s, tc.want) {
			t.Errorf("mode %s: want %s, got %s", pulseModeString(), tc.want, s)
		}
	}
}

func TestRenderDualDialProducesWideSVG(t *testing.T) {
	data := renderDualDial(blankLimits, [3]int{1, 2, 3}, true,
		blankLimits, [3]int{4, 5, 6}, true)
	s := string(data)
	want := fmt.Sprintf(`viewBox="0 0 %.2f 18"`, 2*sparkPad+2*18+(2+sparkPad))
	if !strings.Contains(s, want) {
		t.Errorf("dual dial viewBox %s missing: %s", want, s)
	}
}

func nearly(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestQuotaPaceColors(t *testing.T) {
	want := map[quotaPace][3]int{
		paceUnknown: dialGray,
		pacePurple:  dialPurple,
		paceGreen:   dialGreen,
		paceAmber:   dialYellow,
		paceRed:     dialRed,
	}
	for pace, color := range want {
		if got := quotaPaceColor(pace); got != color {
			t.Errorf("pace %v color = %v, want %v", pace, got, color)
		}
	}
}

func TestAggregateDialPaceUsesWorstKnownOuterWindow(t *testing.T) {
	limits := map[string]Limit{
		"session": {Pct: 20, TimeLeft: 0.95, WindowSecs: fiveHourSecs, WaitOKSecs: 30 * 60},
		"weekly":  {Pct: 80, TimeLeft: 0.50, WindowSecs: weekSecs, WaitOKSecs: 12 * 60 * 60},
	}
	if got := aggregateDialPace(limits); got != paceRed {
		t.Fatalf("unknown session + red weekly = %v, want red", got)
	}
	limits["weekly"] = Limit{Pct: 1, TimeLeft: 0.95, WindowSecs: weekSecs, WaitOKSecs: 12 * 60 * 60}
	if got := aggregateDialPace(limits); got != paceUnknown {
		t.Fatalf("two untrusted windows = %v, want unknown", got)
	}
}

func TestSessionCappedByWeeklyGreyTail(t *testing.T) {
	grey := svgRGB(dialUnavail)
	// 3% weekly left, one session costs 15% of the weekly -> only 3/15 = 20% of
	// the session is spendable, though 80% of its budget remains. Grey tail.
	capped := map[string]Limit{
		"session": {Pct: 20, TimeLeft: 0.5},               // 80% session left
		"weekly":  {Pct: 97, TimeLeft: 0.5, BlockPct: 15}, // 3% weekly left
	}
	if svg := string(renderDial(capped, [3]int{0, 0, 0}, false)); !strings.Contains(svg, grey) {
		t.Fatalf("session should show a grey unavailable tail when the weekly caps it:\n%s", svg)
	}
	// Plenty of weekly left (80%, ~5 sessions) -> nothing is capped, no grey.
	uncapped := map[string]Limit{
		"session": {Pct: 80, TimeLeft: 0.5},               // 20% session left
		"weekly":  {Pct: 20, TimeLeft: 0.5, BlockPct: 15}, // 80% weekly left
	}
	if svg := string(renderDial(uncapped, [3]int{0, 0, 0}, false)); strings.Contains(svg, grey) {
		t.Fatalf("no grey tail expected when the weekly has sessions to spare:\n%s", svg)
	}
	// No BlockPct calibrated yet -> can't convert, so no cap is drawn.
	nocal := map[string]Limit{
		"session": {Pct: 20, TimeLeft: 0.5},
		"weekly":  {Pct: 97, TimeLeft: 0.5},
	}
	if svg := string(renderDial(nocal, [3]int{0, 0, 0}, false)); strings.Contains(svg, grey) {
		t.Fatalf("no grey tail expected before BlockPct is known:\n%s", svg)
	}
}

func TestOuterRingColorReflectsCap(t *testing.T) {
	tf := 0.5
	// 92% session left but only 2% weekly at 15%/session -> usable 2/15 ≈ 0.133.
	limits := map[string]Limit{
		"session": {Pct: 8, TimeLeft: tf, WindowSecs: fiveHourSecs, WaitOKSecs: 30 * 60}, // 92% session left
		"weekly":  {Pct: 98, TimeLeft: tf, BlockPct: 15},                                 // 2% weekly left
	}
	svg := string(renderDial(limits, [3]int{0, 0, 0}, false))
	// The lit session arc must be coloured by the usable (capped) fraction, which
	// trails the time left -> red, NOT the plentiful raw 92% -> purple.
	capped := svgRGB(quotaPaceColor(limitPace(limits["session"], 0.02/0.15)))
	raw := svgRGB(dialPurple)
	if !strings.Contains(svg, capped) {
		t.Fatalf("outer session colour should follow the cap (%s):\n%s", capped, svg)
	}
	if strings.Contains(svg, raw) {
		t.Fatalf("outer session colour must not use the raw session %% (%s)", raw)
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

func TestLimitPaceProjectsOverWorkingHours(t *testing.T) {
	// A fresh week: 6% spent in the first 7.5 hours. On the wall clock that
	// extrapolates to 135% (red); on a 14h working day it is 7.5 of 98
	// working hours, 78% projected — purple.
	const workDay = 14 * 3600.0
	wall := Limit{Pct: 6, TimeLeft: 1 - 7.5/(7*24), WindowSecs: weekSecs, WaitOKSecs: 12 * 3600}
	if got := limitPace(wall, 0.94); got != paceRed {
		t.Fatalf("wall-clock pace = %v, want red", got)
	}
	work := wall
	work.PaceWindowSecs = 7 * workDay
	work.PaceLeftSecs = 7*workDay - 7.5*3600
	if got := limitPace(work, 0.94); got != pacePurple {
		t.Fatalf("working-hours pace = %v, want purple", got)
	}
}
