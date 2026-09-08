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

func TestBurnBucketsDerivative(t *testing.T) {
	now := int64(1_000_000)
	horizon := int64(3600)
	bucket := int64(900) // 4 buckets of 15 min
	// Sample times sit mid-bucket (deltas land in the bucket of the later sample).
	s := []histSample{
		{now - 3600, 0}, // baseline at start
		{now - 3150, 5}, // +5 in bucket 0 (450s in)
		{now - 2250, 8}, // +3 in bucket 1 (1350s in)
		{now - 2200, 2}, // reset (drop) -> ignored
		{now - 1350, 6}, // +4 in bucket 2 (2250s in)
		{now - 450, 10}, // +4 in bucket 3 (3150s in)
	}
	b := burnBuckets(s, now, horizon, bucket)
	if len(b) != 4 {
		t.Fatalf("want 4 buckets, got %d", len(b))
	}
	want := []float64{5, 3, 4, 4}
	for i, w := range want {
		if b[i].Spent != w {
			t.Fatalf("bucket %d: want %.0f, got %.2f", i, w, b[i].Spent)
		}
	}
	if got := totalSpent(b); got != 16 {
		t.Fatalf("total: want 16, got %.2f", got)
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

func TestLastUseRateSkipsTrailingIdlePolls(t *testing.T) {
	s := []histSample{
		{T: 100, V: 10},
		{T: 220, V: 11}, // latest actual use: +1% over 2m
		{T: 340, V: 11},
		{T: 460, V: 11}, // trailing idle polls must not turn the rate into zero
	}
	got := lastUseRate(s, 460)
	if !got.OK || got.At != 220 || got.Span != 120 || !near(got.Rate, 1.0/120, 1e-12) {
		t.Fatalf("last use rate = %+v", got)
	}
}

func TestLastUseRateExpiresAfterMaxPollGap(t *testing.T) {
	s := []histSample{
		{T: 100, V: 10},
		{T: 220, V: 11},
		{T: 340, V: 11},
	}
	got := lastUseRate(s, 220+lastUseFreshSecs+1)
	if got.OK || got.Rate != 0 || got.At != 220 {
		t.Fatalf("stale use should retain only its timestamp: %+v", got)
	}
}

func TestLastUseRateDoesNotCrossReset(t *testing.T) {
	s := []histSample{
		{T: 100, V: 90},
		{T: 220, V: 95},
		{T: 340, V: 0}, // reset
		{T: 460, V: 0},
	}
	if got := lastUseRate(s, 460); got.OK {
		t.Fatalf("pre-reset use must not drive a new-window rate: %+v", got)
	}
	s = append(s, histSample{T: 580, V: 1})
	if got := lastUseRate(s, 580); !got.OK || got.At != 580 || !near(got.Rate, 1.0/120, 1e-12) {
		t.Fatalf("post-reset use should establish a fresh rate: %+v", got)
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
	leftBuckets := []burnBucket{{Start: now - 900, Spent: 2}}
	rightBuckets := []burnBucket{{Start: now - 900, Spent: 8}}
	left := burnChartPanel{
		Provider: "Claude", Label: "5h", Buckets: leftBuckets,
		WindowSecs: int64(fiveHourSecs), BucketSecs: 900,
		Start: now - 3600, Now: now, Known: true, Accent: claudeChartAccent,
		UsedPct: 40, RecentRate: 10.0 / 3600, RateSpan: 120,
		LastUseAt: now - 300, ResetAt: now + 7*3600,
	}
	right := burnChartPanel{
		Provider: "Codex", Label: "7d", Buckets: rightBuckets,
		WindowSecs: int64(weekSecs), BucketSecs: 900,
		Start: now - int64(weekSecs), Now: now, Known: true, Accent: codexChartAccent,
		UsedPct: 20, RecentRate: 20.0 / 21600, LastUseAt: now - 720, ResetAt: now + int64(weekSecs),
	}
	svg := string(renderPairedBurnChart(left, right, 9*60, 18*60))
	var root struct {
		XMLName xml.Name `xml:"svg"`
	}
	if err := xml.Unmarshal([]byte(svg), &root); err != nil {
		t.Fatalf("paired chart is not valid SVG: %v", err)
	}
	if !strings.Contains(svg, `width="480" height="92"`) {
		t.Fatalf("paired dimensions missing: %.100s", svg)
	}
	if !strings.Contains(svg, "CLAUDE · 5H") || !strings.Contains(svg, "CODEX · 7D") {
		t.Fatal("both provider labels should be embedded in their own panels")
	}
	if !strings.Contains(svg, svgRGB(claudeChartAccent)) || !strings.Contains(svg, svgRGB(codexChartAccent)) {
		t.Fatal("provider panels should retain distinct accents")
	}
	if got := countSubstr(svg, `stroke-dasharray="2 1.5"`); got != 2 {
		t.Fatalf("want one sustainable-pace line per provider, got %d", got)
	}
	if got := countSubstr(svg, `stroke="url(#burn-speed)"`); got != 2 {
		t.Fatalf("want one speedometer per provider, got %d", got)
	}
	if got := countSubstr(svg, `data-axis-label="true"`); got != 6 {
		t.Fatalf("want three numeric y-axis labels per provider, got %d", got)
	}
	if !strings.Contains(svg, "0.00%") {
		t.Fatal("y-axis labels should state quota percent per bucket")
	}
	if !strings.Contains(svg, "SPENT 2.0%") || !strings.Contains(svg, "SPENT 8.0%") {
		t.Fatal("top-right totals should be explicitly labelled as spent quota")
	}
	if !strings.Contains(svg, "10.0%/H") || !strings.Contains(svg, "80.0%/D") ||
		!strings.Contains(svg, "MEASURED OVER 2M · LAST USE 5M AGO") ||
		!strings.Contains(svg, "EMPTY 6H00M") {
		t.Fatal("scale-specific speed, measurement span, and quota-end forecast should be embedded in the panel")
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
