package main

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func TestAddHistSampleOrdering(t *testing.T) {
	var s []histSample
	s = addHistSample(s, 100, 10)
	s = addHistSample(s, 90, 5)  // out of order -> ignored
	s = addHistSample(s, 100, 7) // duplicate timestamp -> ignored
	s = addHistSample(s, 200, 12)
	if len(s) != 2 {
		t.Fatalf("want 2 samples, got %d: %+v", len(s), s)
	}
	if s[0].V != 10 || s[1].V != 12 {
		t.Fatalf("unexpected values: %+v", s)
	}
}

func TestDecimateHistory(t *testing.T) {
	now := int64(10_000_000)
	var s []histSample
	// Old region (older than 24h): one sample every 5 min over an hour.
	for t0 := now - histMaxAge + 3600; t0 < now-histMaxAge+3600+3600; t0 += 300 {
		s = append(s, histSample{t0, 1})
	}
	// Recent region (< 24h): one every 2 min for 20 min — all should survive.
	for t0 := now - 1200; t0 <= now; t0 += 120 {
		s = append(s, histSample{t0, 2})
	}
	// A truly ancient sample that must be pruned.
	s = append([]histSample{{now - histMaxAge - 1, 0}}, s...)

	out := decimateHistory(s, now)
	for _, smp := range out {
		if now-smp.T > histMaxAge {
			t.Fatalf("sample older than max age survived: %+v", smp)
		}
	}
	// The old region (12 samples, 5 min apart) must be thinned to >= 15 min gaps.
	for i := 1; i < len(out); i++ {
		if now-out[i].T > histFullRes && out[i].T-out[i-1].T < histThinStep {
			t.Fatalf("old samples not thinned: gap %ds at %+v", out[i].T-out[i-1].T, out[i])
		}
	}
	// Recent samples (11 of them) are kept in full.
	recent := 0
	for _, smp := range out {
		if now-smp.T <= histFullRes {
			recent++
		}
	}
	if recent != 11 {
		t.Fatalf("want 11 recent samples kept, got %d", recent)
	}
}

func TestBurnIntervalsFollowEveryUpdate(t *testing.T) {
	now := int64(1_000_000)
	horizon := int64(3600)
	s := []histSample{
		{now - 3600, 0}, // baseline at start
		{now - 3150, 5}, // +5 over 450s
		{now - 2250, 8}, // +3 over 900s
		{now - 2200, 2}, // reset (drop) -> zero-rate update
		{now - 1350, 6}, // +4 over 850s
		{now - 450, 10}, // +4 over 900s
	}
	intervals := burnIntervals(s, now, horizon)
	if len(intervals) != len(s)-1 {
		t.Fatalf("want one interval per update, got %d", len(intervals))
	}
	if got := intervals[0].Rate; !near(got, 5.0/450, 1e-12) {
		t.Fatalf("first update rate = %v", got)
	}
	if intervals[2].Rate != 0 || intervals[2].Spent != 0 {
		t.Fatalf("reset interval should be zero, got %+v", intervals[2])
	}
	if got := totalSpent(intervals); got != 16 {
		t.Fatalf("total: want 16, got %.2f", got)
	}
}

func TestBurnIntervalsProrateHorizonBoundary(t *testing.T) {
	now := int64(10_000)
	intervals := burnIntervals([]histSample{{T: now - 1200, V: 0}, {T: now - 600, V: 6}}, now, 900)
	if len(intervals) != 1 {
		t.Fatalf("intervals = %+v", intervals)
	}
	if intervals[0].Start != now-900 || intervals[0].End != now-600 || !near(intervals[0].Spent, 3, 1e-12) {
		t.Fatalf("boundary interval = %+v", intervals[0])
	}
}

func TestInWorkWindowWrap(t *testing.T) {
	// Work 12:00 -> 02:00 (wraps midnight). Day inside, night outside.
	ws, we := 12*60, 2*60
	cases := []struct {
		min  int
		work bool
	}{
		{13 * 60, true},  // 13:00 working
		{1 * 60, true},   // 01:00 still working (wrapped)
		{3 * 60, false},  // 03:00 night
		{11 * 60, false}, // 11:00 night
		{12 * 60, true},  // exactly start
		{2 * 60, false},  // exactly end -> excluded
	}
	for _, c := range cases {
		if got := inWorkWindow(c.min, ws, we); got != c.work {
			t.Fatalf("min %d: want work=%v, got %v", c.min, c.work, got)
		}
	}
}

func TestNightBandsSplit(t *testing.T) {
	// A window straddling the 02:00 day/night boundary should produce a band.
	loc := time.Local
	// 01:30 local today.
	base := time.Date(2026, 3, 3, 1, 30, 0, 0, loc).Unix()
	start := base
	now := base + 2*3600 // spans 01:30 -> 03:30, crossing 02:00
	bands := nightBands(start, now, 12*60, 2*60)
	if len(bands) == 0 {
		t.Fatal("expected a night band after 02:00")
	}
	// The band should begin at/after 02:00, not before.
	first := bands[0][0]
	if minuteOfDay(first) < 2*60-5 {
		t.Fatalf("night band starts before 02:00: minute %d", minuteOfDay(first))
	}
}

func TestBurnColorThresholds(t *testing.T) {
	if burnColor(0) != dialPurple {
		t.Errorf("ratio 0 should be purple, got %v", burnColor(0))
	}
	if burnColor(0.5) != dialGreen {
		t.Errorf("ratio 0.5 should be green, got %v", burnColor(0.5))
	}
	if burnColor(1) != dialYellow {
		t.Errorf("ratio 1 should be yellow, got %v", burnColor(1))
	}
	if burnColor(1.5) != dialRed {
		t.Errorf("ratio 1.5 should be red, got %v", burnColor(1.5))
	}
}

func TestWindowUseRateIncludesTrailingIdlePolls(t *testing.T) {
	s := []histSample{
		{T: 100, V: 10},
		{T: 220, V: 11},
		{T: 340, V: 11},
		{T: 460, V: 11},
	}
	got := windowUseRate(s, 460, 15*60)
	if !got.OK || got.At != 220 || got.Span != 360 || !near(got.Rate, 1.0/360, 1e-12) {
		t.Fatalf("rolling rate = %+v", got)
	}
}

func TestWindowUseRateExpiresAfterMaxPollGap(t *testing.T) {
	s := []histSample{
		{T: 100, V: 10},
		{T: 220, V: 11},
		{T: 340, V: 11},
	}
	got := windowUseRate(s, 340+lastUseFreshSecs+1, 15*60)
	if got.OK || got.Rate != 0 || got.At != 220 {
		t.Fatalf("stale use should retain only its timestamp: %+v", got)
	}
}

func TestWindowUseRateDoesNotCrossReset(t *testing.T) {
	s := []histSample{
		{T: 100, V: 90},
		{T: 220, V: 95},
		{T: 340, V: 0}, // reset
		{T: 460, V: 0},
	}
	if got := windowUseRate(s, 460, 15*60); !got.OK || got.Rate != 0 || got.At != 0 {
		t.Fatalf("pre-reset use must not drive a new-window rate: %+v", got)
	}
	s = append(s, histSample{T: 580, V: 1})
	if got := windowUseRate(s, 580, 15*60); !got.OK || got.At != 580 || !near(got.Rate, 1.0/240, 1e-12) {
		t.Fatalf("post-reset use should establish a fresh rate: %+v", got)
	}
}

func TestRateTrendComparesFiveMinutesWithFifteen(t *testing.T) {
	s := []histSample{
		{T: 0, V: 0},
		{T: 300, V: 1},
		{T: 600, V: 3},
		{T: 900, V: 6},
	}
	fifteen := windowUseRate(s, 900, 15*60)
	five := windowUseRate(s, 900, 5*60)
	trend, ok := rateTrend(five, fifteen)
	if !ok || !near(fifteen.Rate, 6.0/900, 1e-12) || !near(five.Rate, 3.0/300, 1e-12) {
		t.Fatalf("rates = five %+v, fifteen %+v", five, fifteen)
	}
	if !near(trend, 1.0/3, 1e-12) {
		t.Fatalf("trend = %v, want 1/3 acceleration", trend)
	}
	if stable, ok := rateTrend(fifteen, fifteen); !ok || stable != 0 {
		t.Fatalf("equal rates should sit at centre, got %v ok=%v", stable, ok)
	}
}

func TestTrendColorRunsThroughBothScales(t *testing.T) {
	for value, want := range map[float64][3]int{
		-1: dialPurple, -0.5: trendBlue, 0: dialGreen,
		0.5: dialYellow, 1: dialRed,
	} {
		if got := trendColor(value); got != want {
			t.Errorf("trendColor(%v) = %v, want %v", value, got, want)
		}
	}
}

func TestRenderPctBar(t *testing.T) {
	svg := string(renderPctBar(42, "Session 42%"))
	if !strings.HasPrefix(svg, "<svg") || !strings.HasSuffix(svg, "</svg>") {
		t.Fatalf("not well-formed svg: %.40s...", svg)
	}
	if !strings.Contains(svg, "<text") || !strings.Contains(svg, "Session 42%") {
		t.Fatal("label text should be drawn inside the bar")
	}
	// A near-empty and a full bar must both stay well-formed (no negative widths).
	for _, p := range []float64{0, 100} {
		if s := string(renderPctBar(p, "x")); !strings.HasSuffix(s, "</svg>") {
			t.Fatalf("pct %.0f produced malformed svg", p)
		}
	}
}

func TestPctColor(t *testing.T) {
	if pctColor(0) != dialGreen {
		t.Errorf("0%% should be green")
	}
	if pctColor(100) != dialRed {
		t.Errorf("100%% should be red")
	}
}

func TestRenderPairedBurnChartKeepsProvidersSeparate(t *testing.T) {
	now := int64(1_000_000)
	leftIntervals := []burnInterval{
		{Start: now - 2700, End: now - 1800, Spent: 2, Rate: 2.0 / 900},
		{Start: now - 1800, End: now - 900, Spent: 8, Rate: 8.0 / 900},
	}
	rightIntervals := []burnInterval{
		{Start: now - 2700, End: now - 1800, Spent: 1, Rate: 1.0 / 900},
		{Start: now - 1800, End: now - 900, Spent: 4, Rate: 4.0 / 900},
	}
	left := burnChartPanel{
		Provider: "Claude", Label: "5h", Intervals: leftIntervals,
		WindowSecs: int64(fiveHourSecs), GridSecs: 900,
		Start: now - 3600, Now: now, Known: true, Accent: claudeChartAccent,
		UsedPct: 40, RecentRate: 10.0 / 3600, RateKnown: true,
		AverageRate: 5.0 / 3600, AverageKnown: true,
		PeriodLeft: 7 * 3600, PeriodKnown: true, RateSpan: 900,
		LastUseAt: now - 300, Trend: 0.5, TrendKnown: true, ResetAt: now + 7*3600,
	}
	right := burnChartPanel{
		Provider: "Codex", Label: "7d", Intervals: rightIntervals,
		WindowSecs: int64(weekSecs), GridSecs: 900,
		Start: now - int64(weekSecs), Now: now, Known: true, Accent: codexChartAccent,
		UsedPct: 20, RecentRate: 20.0 / 21600, RateKnown: true,
		AverageRate: 10.0 / 86400, AverageKnown: true,
		PeriodLeft: weekSecs, PeriodKnown: true, RateSpan: 900,
		LastUseAt: now - 720, Trend: -0.5, TrendKnown: true, ResetAt: now + int64(weekSecs),
	}
	svg := string(renderPairedBurnChart(left, right, 9*60, 18*60))
	var root struct {
		XMLName xml.Name `xml:"svg"`
	}
	if err := xml.Unmarshal([]byte(svg), &root); err != nil {
		t.Fatalf("paired chart is not valid SVG: %v", err)
	}
	if !strings.Contains(svg, `width="480" height="104"`) {
		t.Fatalf("paired dimensions missing: %.100s", svg)
	}
	if !strings.Contains(svg, "CLAUDE 5H") || !strings.Contains(svg, "CODEX 7D") {
		t.Fatal("both provider labels should be embedded in their own panels")
	}
	for _, kind := range []string{"period", "current", "average"} {
		if got := countSubstr(svg, `data-runway-kind="`+kind+`"`); got != 2 {
			t.Fatalf("want one %s runway per provider, got %d", kind, got)
		}
	}
	if !strings.Contains(svg, "RESET 7H") || !strings.Contains(svg, "CURRENT 6H") ||
		!strings.Contains(svg, "AVG 12H · 5.0%/h") ||
		!strings.Contains(svg, "RESET 7D") || !strings.Contains(svg, "CURRENT 1D") ||
		!strings.Contains(svg, "AVG 8D · 10.0%/d") {
		t.Fatal("reset, current-rate, and average-rate runway labels should remain separate")
	}
	if !strings.Contains(svg, svgRGB(periodRunwayColor)) ||
		!strings.Contains(svg, svgRGB(currentRunwayColor)) ||
		!strings.Contains(svg, svgRGB(averageRunwayColor)) {
		t.Fatal("the three runway meanings should have distinct fixed colors")
	}
	if !strings.Contains(svg, svgRGB(claudeChartAccent)) || !strings.Contains(svg, svgRGB(codexChartAccent)) {
		t.Fatal("provider panels should retain distinct accents")
	}
	if got := countSubstr(svg, `stroke-dasharray="2 1.5"`); got != 2 {
		t.Fatalf("want one sustainable-pace line per provider, got %d", got)
	}
	if got := countSubstr(svg, `data-burn-bar="true"`); got != 4 {
		t.Fatalf("want one granular bar per update interval, got %d", got)
	}
	if !strings.Contains(svg, `data-burn-ratio="0.400"`) || !strings.Contains(svg, `data-burn-ratio="26.880"`) {
		t.Fatal("bars should carry their provider-specific speed ratio")
	}
	if got := countSubstr(svg, `stroke="url(#burn-speed)"`); got != 2 {
		t.Fatalf("want one speedometer per provider, got %d", got)
	}
	// Claude: 60% left over 7h affords 8.57%/h; the 10%/h reading is 1.167x that.
	// Codex: 80% left over 7d affords 11.4%/d; 80%/d is 7x — pegged past the dial.
	if !strings.Contains(svg, `data-speed-ratio="1.167"`) || !strings.Contains(svg, `data-speed-ratio="7.000"`) {
		t.Fatalf("needles should measure against the affordable pace to reset: %s", svg)
	}
	for _, tick := range []string{"0.5", "1.0", "1.5"} {
		if got := countSubstr(svg, `data-speed-tick="`+tick+`"`); got != 2 {
			t.Fatalf("want a %sx threshold tick per provider, got %d", tick, got)
		}
	}
	// Nominal pace (100%/window) on the affordable scale: Claude 20%/h vs 8.57%/h
	// affordable = 2.333x; Codex 14.3%/d vs 11.4%/d = 1.25x.
	if !strings.Contains(svg, `data-pace-ratio="2.333"`) || !strings.Contains(svg, `data-pace-ratio="1.250"`) {
		t.Fatalf("nominal pace marks should sit on the affordable scale: %s", svg)
	}
	if got := countSubstr(svg, `data-trend-bar="true"`); got != 2 {
		t.Fatalf("want one semicircular trend bar per provider, got %d", got)
	}
	if got := countSubstr(svg, `data-trend-value="true"`); got != 2 {
		t.Fatalf("want one trend value arc per provider, got %d", got)
	}
	if !strings.Contains(svg, `data-trend="0.500"`) || !strings.Contains(svg, `data-trend="-0.500"`) {
		t.Fatal("trend bars should independently show acceleration and deceleration")
	}
	if got := countSubstr(svg, `data-axis-label="true"`); got != 6 {
		t.Fatalf("want three numeric y-axis labels per provider, got %d", got)
	}
	if got := countSubstr(svg, `data-time-label="true"`); got != 6 {
		t.Fatalf("want start, midpoint, and end labels under each provider, got %d", got)
	}
	if !strings.Contains(svg, "0.00%/h") || !strings.Contains(svg, "0.00%/d") {
		t.Fatal("y-axis labels should state quota consumption speed")
	}
	if !strings.Contains(svg, "SPENT 10.0%") || !strings.Contains(svg, "SPENT 5.0%") {
		t.Fatal("top-right totals should be explicitly labelled as spent quota")
	}
	if !strings.Contains(svg, "10.0%/H") || !strings.Contains(svg, "80.0%/D") ||
		!strings.Contains(svg, "MEASURED OVER 15M · LAST USE 5M AGO") ||
		!strings.Contains(svg, "CURRENT EMPTY 6H00M") {
		t.Fatal("scale-specific speed, measurement span, and quota-end forecast should be embedded in the panel")
	}
}

func TestMakeBurnPanelShowsCodexFiveHourAverage(t *testing.T) {
	now := int64(1_000_000)
	series := historyWindow{
		samples: []histSample{{T: now - 900, V: 20}, {T: now, V: 30}},
		secs:    int64(fiveHourSecs), present: true, used: 30, resetAt: now + 3*3600,
	}
	panel := makeBurnPanel("Codex", "5h", series, int64(fiveHourSecs), 900, now, codexChartAccent)
	if !panel.RateKnown || !near(panel.RecentRate, 10.0/900, 1e-12) {
		t.Fatalf("current rate = %v known=%v", panel.RecentRate, panel.RateKnown)
	}
	if !panel.AverageKnown || !near(panel.AverageRate, 15.0/3600, 1e-12) {
		t.Fatalf("5h average = %v known=%v, want 15%%/h", panel.AverageRate, panel.AverageKnown)
	}
	svg := string(renderPairedBurnChart(panel, burnChartPanel{}, 9*60, 18*60))
	if !strings.Contains(svg, "AVG 4⅔H · 15.0%/h") {
		t.Fatalf("Codex 5h average should be explicit in chart: %s", svg)
	}
}

func TestRunwayStatsDoNotHideIdleCurrentRate(t *testing.T) {
	panel := burnChartPanel{
		WindowSecs: int64(fiveHourSecs), UsedPct: 20,
		RecentRate: 0, RateKnown: true,
		AverageRate: 10.0 / 3600, AverageKnown: true,
		PeriodLeft: 2 * 3600, PeriodKnown: true,
	}
	var b strings.Builder
	writeRunwayStats(&b, panel, 0, 220, 10)
	stats := b.String()
	if !strings.Contains(stats, "CURRENT ∞") || !strings.Contains(stats, "AVG 8H · 10.0%/h") {
		t.Fatalf("idle current and finite average must remain distinct: %s", stats)
	}
}

func TestCodexMiddleHistoryUsesFiveHoursOfWeeklyOnlyData(t *testing.T) {
	weekly := historyWindow{
		samples: []histSample{{T: 100, V: 10}, {T: 200, V: 11}},
		secs:    int64(weekSecs), present: true, used: 11, resetAt: 300,
	}
	got, horizon, label := middleHistory(historyWindow{}, weekly)
	if !got.present || len(got.samples) != 2 || got.secs != int64(weekSecs) {
		t.Fatalf("middle chart should retain the real weekly series: %+v", got)
	}
	if horizon != int64(fiveHourSecs) || label != "5h" {
		t.Fatalf("weekly-only middle chart = %q/%ds, want 5h/%ds", label, horizon, int64(fiveHourSecs))
	}
}

func TestCodexMiddleHistoryPrefersRealShortWindow(t *testing.T) {
	short := historyWindow{secs: 3 * 60 * 60, present: true, used: 20}
	weekly := historyWindow{secs: int64(weekSecs), present: true, used: 30}
	got, horizon, label := middleHistory(short, weekly)
	if got.used != short.used || horizon != short.secs || label != "3h" {
		t.Fatalf("real short window should win: got %+v, %ds, %q", got, horizon, label)
	}
}

func TestAxisPctTextKeepsSmallScaleReadable(t *testing.T) {
	for value, want := range map[float64]string{
		0:   "0.00%",
		0.4: "0.40%",
		4.2: "4.2%",
		42:  "42%",
	} {
		if got := axisPctText(value); got != want {
			t.Errorf("axisPctText(%v) = %q, want %q", value, got, want)
		}
	}
}

func TestChartTimeTextMatchesChartScale(t *testing.T) {
	at := time.Date(2026, 9, 8, 16, 42, 0, 0, time.UTC)
	if got := chartTimeText(at, 5*60*60); got != "16:42" {
		t.Fatalf("sub-day axis label = %q", got)
	}
	if got := chartTimeText(at, 7*24*60*60); got != "Sep 08" {
		t.Fatalf("daily axis label = %q", got)
	}
}

func TestBurnIntervalsSpreadStepOverFlatRun(t *testing.T) {
	now := int64(1_000_000)
	s := []histSample{
		{now - 1200, 10},
		{now - 1080, 10}, // flat polls at 1% resolution
		{now - 960, 10},
		{now - 840, 11}, // the step is revealed here, spent over the whole run
		{now - 720, 11},
		{now - 600, 11}, // trailing run with no step yet
	}
	intervals := burnIntervals(s, now, 3600)
	if len(intervals) != 2 {
		t.Fatalf("want one bar per run, got %+v", intervals)
	}
	if intervals[0].Start != now-1200 || intervals[0].End != now-840 || !near(intervals[0].Rate, 1.0/360, 1e-12) {
		t.Fatalf("step run = %+v", intervals[0])
	}
	if intervals[1].Start != now-840 || intervals[1].End != now-600 || intervals[1].Rate != 0 {
		t.Fatalf("trailing run = %+v", intervals[1])
	}
	if got := totalSpent(intervals); !near(got, 1, 1e-12) {
		t.Fatalf("total: want 1, got %v", got)
	}
}

func TestBurnIntervalsResetEndsRun(t *testing.T) {
	now := int64(1_000_000)
	s := []histSample{
		{now - 900, 98},
		{now - 600, 98},
		{now - 300, 0}, // reset after a flat run
		{now - 100, 1}, // first step of the new window counts from the reset
	}
	intervals := burnIntervals(s, now, 3600)
	if len(intervals) != 2 {
		t.Fatalf("intervals = %+v", intervals)
	}
	if intervals[0].Spent != 0 || intervals[0].End != now-300 {
		t.Fatalf("reset run = %+v", intervals[0])
	}
	if intervals[1].Start != now-300 || !near(intervals[1].Rate, 1.0/200, 1e-12) {
		t.Fatalf("post-reset run = %+v", intervals[1])
	}
}

func TestSmoothRatesPreservesAreaAndSoftensEdges(t *testing.T) {
	now := int64(1_000_000)
	start := now - 3600
	// One run burning for the middle third of the horizon.
	intervals := []burnInterval{{Start: start + 1200, End: start + 2400, Spent: 2, Rate: 2.0 / 1200}}
	n := 120
	curve := smoothRates(intervals, start, now, n, 90)
	if len(curve) != n {
		t.Fatalf("len = %d", len(curve))
	}
	colSecs := 3600.0 / float64(n)
	area := 0.0
	for _, r := range curve {
		area += r * colSecs
	}
	if !near(area, 2, 1e-6) {
		t.Fatalf("blur should preserve spend: want 2, got %v", area)
	}
	peak := 2.0 / 1200
	if !near(curve[n/2], peak, 1e-9) {
		t.Fatalf("plateau should keep its level: want %v, got %v", peak, curve[n/2])
	}
	// At the edge of the run the curve is in transition: strictly between 0
	// and the plateau on both sides.
	edge := n / 3
	if !(curve[edge-1] > 0 && curve[edge-1] < peak && curve[edge] > 0 && curve[edge] < peak) {
		t.Fatalf("edge should be softened: %v / %v", curve[edge-1], curve[edge])
	}
	if curve[0] != 0 || curve[n-1] != 0 {
		t.Fatalf("far columns should stay zero: %v / %v", curve[0], curve[n-1])
	}
}

func TestSmoothRatesKeepsNarrowRun(t *testing.T) {
	now := int64(1_000_000)
	start := now - 7*24*3600
	// A 2-minute run in a 7-day horizon is far narrower than a column; its
	// spend must still land in the curve rather than fall between samples.
	intervals := []burnInterval{{Start: now - 3600, End: now - 3480, Spent: 1, Rate: 1.0 / 120}}
	curve := smoothRates(intervals, start, now, 160, 0)
	area := 0.0
	for _, r := range curve {
		area += r * float64(now-start) / 160
	}
	if !near(area, 1, 1e-9) {
		t.Fatalf("narrow run lost: area %v", area)
	}
}

func TestSmoothPanelDrawsAreaOverFaintBars(t *testing.T) {
	now := int64(1_000_000)
	panel := burnChartPanel{
		Provider: "Codex", Label: "1h",
		Intervals:  []burnInterval{{Start: now - 1800, End: now - 600, Spent: 1, Rate: 1.0 / 1200}},
		WindowSecs: 604800, GridSecs: 300, Start: now - 3600, Now: now, Known: true,
		Accent: codexChartAccent, SmoothSecs: 60,
	}
	svg := string(renderPairedBurnChart(panel, smoothed(panel, 0), 9*60, 18*60))
	if got := countSubstr(svg, `data-burn-area="true"`); got != 1 {
		t.Fatalf("want one smoothed area, got %d", got)
	}
	if got := countSubstr(svg, `fill-opacity="0.30"`); got != 1 {
		t.Fatalf("smoothed panel should fade its raw bar, got %d faint bars", got)
	}
	if got := countSubstr(svg, `fill-opacity="0.96"`); got != 1 {
		t.Fatalf("raw panel should keep its solid bar, got %d", got)
	}
}

func TestSmoothRatesKeepsMassAtTheEdges(t *testing.T) {
	now := int64(1_000_000)
	start := now - 3600
	// Runs touching both edges: a wide blur would spill past the window, but
	// the curve must still integrate to exactly what the bars do.
	intervals := []burnInterval{
		{Start: start, End: start + 300, Spent: 3, Rate: 3.0 / 300},
		{Start: now - 120, End: now, Spent: 1, Rate: 1.0 / 120},
	}
	n := 120
	curve := smoothRates(intervals, start, now, n, 600)
	area := 0.0
	for _, r := range curve {
		area += r * 3600 / float64(n)
	}
	if !near(area, totalSpent(intervals), 1e-9) {
		t.Fatalf("edge mass leaked: curve %v vs bars %v", area, totalSpent(intervals))
	}
}

func TestUsedAtSpreadsStepsAndHoldsAcrossResets(t *testing.T) {
	s := []histSample{{0, 10}, {100, 10}, {200, 14}, {300, 14}, {400, 2}, {500, 2}}
	for _, tc := range []struct {
		t    int64
		want float64
	}{
		{-50, 10}, {0, 10}, {50, 11}, {150, 13}, {200, 14}, {350, 14}, {400, 2}, {450, 2}, {900, 2},
	} {
		if got := usedAt(s, tc.t); !near(got, tc.want, 1e-9) {
			t.Fatalf("usedAt(%d) = %v, want %v", tc.t, got, tc.want)
		}
	}
	if end, ok := windowEndAfter(s, 150); !ok || end != 400 {
		t.Fatalf("reset after 150 = %v,%v, want 400", end, ok)
	}
	if _, ok := windowEndAfter(s, 400); ok {
		t.Fatal("the sample that reveals the reset already belongs to the open window")
	}
}

func TestAffordablePaceFollowsRemainingQuotaAndTime(t *testing.T) {
	now := int64(10_000)
	// One window: 20% used at the start, a 20% step revealed at now-1000 and
	// so spread over the 2000s since the previous change, then idle; the reset
	// is 1000s after now.
	samples := []histSample{{now - 3000, 20}, {now - 2000, 20}, {now - 1000, 40}, {now, 40}}
	panel := burnChartPanel{
		Provider: "Claude", Label: "1h", Samples: samples,
		Intervals:  burnIntervals(samples, now, 3600),
		WindowSecs: int64(fiveHourSecs), Start: now - 3600, Now: now, Known: true,
		UsedPct: 40, PeriodLeft: 1000, PeriodKnown: true, ResetAt: now + 1000,
		Accent: claudeChartAccent,
	}
	// Idle at the start: 80% left over 4000s to reset.
	if got, ok := panel.affordableAt(now - 3000); !ok || !near(got, 80.0/4000, 1e-12) {
		t.Fatalf("afford at start = %v,%v", got, ok)
	}
	// Three quarters through the run 35% is used, 2500s remain.
	if got, ok := panel.affordableAt(now - 1500); !ok || !near(got, 65.0/2500, 1e-12) {
		t.Fatalf("afford mid-run = %v,%v", got, ok)
	}
	// Idle again: the same 60% over a shrinking wait climbs.
	a1, _ := panel.affordableAt(now - 900)
	a2, _ := panel.affordableAt(now - 100)
	if !(a2 > a1) || !near(a2, 60.0/1100, 1e-12) {
		t.Fatalf("idle pace should climb toward the reset: %v then %v", a1, a2)
	}
	// Unknown reset: the flat whole-window pace, as the needle falls back to.
	flat := panel
	flat.PeriodKnown = false
	if got, ok := flat.affordableAt(now - 100); !ok || !near(got, 100/float64(fiveHourSecs), 1e-12) {
		t.Fatalf("flat fallback = %v,%v", got, ok)
	}
	// The bar is coloured against the pace affordable at its midpoint, not the
	// whole-window pace: 20%/2000s vs 70%/3000s at the midpoint = 0.429x.
	svg := string(renderPairedBurnChart(panel, burnChartPanel{}, 9*60, 18*60))
	if !strings.Contains(svg, `data-burn-ratio="0.429"`) {
		t.Fatalf("bar ratio should use the local affordable pace: %s", svg)
	}
	if got := countSubstr(svg, `data-pace-line="true"`); got != 1 {
		t.Fatalf("want one affordable-pace line, got %d", got)
	}
	// The reference line lies over the burn, not under it.
	if strings.Index(svg, `data-pace-line="true"`) < strings.LastIndex(svg, `data-burn-bar="true"`) {
		t.Fatal("pace line must be drawn after the bars")
	}
}

func TestAffordablePaceRestartsAtObservedResets(t *testing.T) {
	now := int64(10_000)
	// A reset 2000s ago: before it 90% was used with the drop revealed at
	// now-2000; after it the window is fresh and resets 8000s from now.
	samples := []histSample{{now - 3600, 90}, {now - 2000, 5}, {now, 5}}
	panel := burnChartPanel{
		Samples: samples, WindowSecs: int64(fiveHourSecs), Start: now - 3600, Now: now,
		UsedPct: 5, PeriodLeft: 8000, PeriodKnown: true, ResetAt: now + 8000,
	}
	if got, ok := panel.affordableAt(now - 3000); !ok || !near(got, 10.0/1000, 1e-12) {
		t.Fatalf("before the reset 10%% over 1000s: %v,%v", got, ok)
	}
	if got, ok := panel.affordableAt(now - 1000); !ok || !near(got, 95.0/9000, 1e-12) {
		t.Fatalf("after the reset 95%% over 9000s: %v,%v", got, ok)
	}
	credits := burnChartPanel{
		Samples: samples, Unit: "$", Remaining: 50, Start: now - 3600, Now: now,
		PeriodLeft: 8000, PeriodKnown: true, ResetAt: now + 8000,
	}
	if _, ok := credits.affordableAt(now - 3000); ok {
		t.Fatal("a credit card cannot pace the previous period: its budget is unknown")
	}
	if got, ok := credits.affordableAt(now - 1000); !ok || !near(got, 50.0/9000, 1e-12) {
		t.Fatalf("credits after the reset: %v,%v", got, ok)
	}
}
