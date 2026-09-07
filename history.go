package main

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// history.go stores a per-series log of raw utilisation readings and turns it
// into a burn-rate (first-derivative) view: how much of a window's quota was
// spent inside each grid interval. Unlike the live rate/forecast, the charts
// here never extrapolate — they only ever show measured spend, so a wrong
// estimate can't creep in. See renderBurnChart for the drawing.

// histSample is one utilisation reading: cumulative used-% (0..100) at unix
// second T. A window reset shows up as V dropping below the previous sample.
type histSample struct {
	T int64   `json:"t"`
	V float64 `json:"v"`
}

// Series names for the persisted history map. Claude tracks its 5-hour session
// and 7-day weekly counters; Codex its primary and secondary windows.
const (
	seriesClaudeSession  = "claude_session"
	seriesClaudeWeekly   = "claude_weekly"
	seriesCodexPrimary   = "codex_primary"
	seriesCodexSecondary = "codex_secondary"
)

const (
	histFullRes  = int64(24 * 3600)     // keep every sample from the last 24h
	histThinStep = int64(15 * 60)       // older than that: thin to >= 15m apart
	histMaxAge   = int64(7 * 24 * 3600) // drop anything older than 7 days
)

// addHistSample appends a reading and re-decimates. Out-of-order or duplicate
// timestamps are ignored so a clock wobble can't corrupt the ordering the
// derivative relies on.
func addHistSample(s []histSample, t int64, v float64) []histSample {
	if n := len(s); n > 0 && t <= s[n-1].T {
		return s
	}
	s = append(s, histSample{t, v})
	return decimateHistory(s, t)
}

// decimateHistory keeps full resolution for the last 24h, thins older samples
// to at least histThinStep apart, and drops everything past histMaxAge. Samples
// must be in ascending time order (addHistSample guarantees it).
func decimateHistory(s []histSample, now int64) []histSample {
	kept := make([]histSample, 0, len(s))
	for _, smp := range s {
		age := now - smp.T
		if age > histMaxAge {
			continue
		}
		if age <= histFullRes {
			kept = append(kept, smp)
			continue
		}
		// Older region: keep the first sample after each histThinStep gap.
		if len(kept) == 0 || smp.T-kept[len(kept)-1].T >= histThinStep {
			kept = append(kept, smp)
		}
	}
	return kept
}

// burnBucket is the quota spent inside one grid interval starting at Start.
type burnBucket struct {
	Start int64
	Spent float64 // sum of positive utilisation deltas landing in the bucket (%)
}

// burnBuckets slices [now-horizon, now] into buckets of bucketSecs and sums the
// positive utilisation deltas in each. A reset (value drop) contributes 0, so
// spend is counted correctly even across a window wrap. horizon should be a
// whole multiple of bucketSecs.
func burnBuckets(s []histSample, now, horizon, bucketSecs int64) []burnBucket {
	start := now - horizon
	n := int(horizon / bucketSecs)
	if n < 1 {
		n = 1
	}
	buckets := make([]burnBucket, n)
	for i := range buckets {
		buckets[i].Start = start + int64(i)*bucketSecs
	}
	for i := 1; i < len(s); i++ {
		d := s[i].V - s[i-1].V
		if d <= 0 || s[i].T <= start {
			continue
		}
		idx := int((s[i].T - start) / bucketSecs)
		if idx < 0 || idx >= n {
			continue
		}
		buckets[idx].Spent += d
	}
	return buckets
}

// totalSpent is the quota consumed across the whole horizon.
func totalSpent(buckets []burnBucket) float64 {
	sum := 0.0
	for _, b := range buckets {
		sum += b.Spent
	}
	return sum
}

// inWorkWindow reports whether local minute-of-day min falls in the working
// window [ws, we), which may wrap past midnight (e.g. 12:00 -> 02:00). Matches
// workingSecondsUntil's semantics so "night" here is exactly "off the clock".
func inWorkWindow(min, ws, we int) bool {
	dur := (((we - ws) % (24 * 60)) + 24*60) % (24 * 60)
	if dur == 0 {
		dur = 24 * 60
	}
	off := (((min - ws) % (24 * 60)) + 24*60) % (24 * 60)
	return off < dur
}

func minuteOfDay(t int64) int {
	lt := time.Unix(t, 0)
	return lt.Hour()*60 + lt.Minute()
}

// nightBands returns the [from,to) spans of [start, now] that fall outside
// working hours, for shading the chart background. Scanned at 5-minute
// resolution and merged into contiguous runs.
func nightBands(start, now int64, ws, we int) [][2]int64 {
	const step = int64(300)
	var bands [][2]int64
	runStart := int64(-1)
	for t := start; t < now; t += step {
		night := !inWorkWindow(minuteOfDay(t), ws, we)
		switch {
		case night && runStart < 0:
			runStart = t
		case !night && runStart >= 0:
			bands = append(bands, [2]int64{runStart, t})
			runStart = -1
		}
	}
	if runStart >= 0 {
		bands = append(bands, [2]int64{runStart, now})
	}
	return bands
}

// pctColor colours a fill by how full it is: green low, yellow mid, red high.
func pctColor(pct float64) [3]int {
	if pct <= 50 {
		return lerpColor(dialGreen, dialYellow, pct/50)
	}
	return lerpColor(dialYellow, dialRed, (pct-50)/50)
}

var svgTextEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// renderPctBar draws a horizontal progress bar (pct used, 0..100) as an SVG
// icon with label text drawn inside it. Menu-item icons render through WebKit,
// which supports <text>, so the label shows in-bar. The text carries a thin
// dark outline so it stays readable over both the fill and the empty track.
func renderPctBar(pct float64, label string) []byte {
	const W, H = 120.0, 18.0
	pct = clamp(pct, 0, 100)
	fillW := W * pct / 100
	var b strings.Builder
	b.WriteString(fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f">`,
		W, H, W, H))
	b.WriteString(fmt.Sprintf(
		`<rect x="0.5" y="2.5" width="%.2f" height="13" rx="2.5" fill="rgb(120,120,120)" fill-opacity="0.25"/>`,
		W-1))
	if fillW > 0.5 {
		b.WriteString(fmt.Sprintf(
			`<rect x="0.5" y="2.5" width="%.2f" height="13" rx="2.5" fill="%s" fill-opacity="0.9"/>`,
			math.Max(fillW-1, 1), svgRGB(pctColor(pct))))
	}
	b.WriteString(fmt.Sprintf(
		`<text x="4" y="11.8" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="9" `+
			`font-weight="600" fill="white" stroke="black" stroke-width="0.5" stroke-opacity="0.5" `+
			`paint-order="stroke">%s</text>`,
		svgTextEscape.Replace(label)))
	b.WriteString(`</svg>`)
	return []byte(b.String())
}

// burnColor maps a burn ratio (spend vs the sustainable share for the interval)
// to green -> yellow -> red: 0 is idle-green, 1 is exactly on pace (yellow),
// 1.5+ is well over pace (red).
func burnColor(ratio float64) [3]int {
	if ratio <= 1 {
		return lerpColor(dialGreen, dialYellow, clamp(ratio, 0, 1))
	}
	return lerpColor(dialYellow, dialRed, clamp((ratio-1)/0.5, 0, 1))
}

const (
	pairedChartWidth  = 320.0
	pairedChartHeight = 46.0
)

var claudeChartAccent = [3]int{217, 119, 87}
var codexChartAccent = [3]int{16, 163, 127}

// burnChartPanel is one self-contained provider chart inside a paired history
// row. The two panels deliberately keep their own horizon, bucket size and
// quota-window denominator: Claude and Codex can expose different windows even
// though their cards sit side by side.
type burnChartPanel struct {
	Provider   string
	Label      string
	Buckets    []burnBucket
	WindowSecs int64
	BucketSecs int64
	Start      int64
	Now        int64
	Known      bool
	Accent     [3]int
}

// renderPairedBurnChart draws two independent provider cards in one wide menu
// image. Bars are measured against sustainable spend for their own interval;
// the dashed line is exactly sustainable pace, and the y scale always leaves
// room above it so "healthy" does not look pegged to the ceiling.
func renderPairedBurnChart(left, right burnChartPanel, ws, we int) []byte {
	const gap = 8.0
	panelW := (pairedChartWidth - gap) / 2
	var b strings.Builder
	b.WriteString(fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f">`,
		pairedChartWidth, pairedChartHeight, pairedChartWidth, pairedChartHeight))
	writeBurnPanel(&b, left, 0, panelW, ws, we)
	writeBurnPanel(&b, right, panelW+gap, panelW, ws, we)
	b.WriteString(`</svg>`)
	return []byte(b.String())
}

func writeBurnPanel(b *strings.Builder, panel burnChartPanel, x, width float64, ws, we int) {
	const (
		panelTop = 0.5
		plotTop  = 16.0
		plotBase = 41.0
		pad      = 6.0
	)
	plotX, plotW := x+pad, width-2*pad
	accent := svgRGB(panel.Accent)
	b.WriteString(fmt.Sprintf(
		`<rect x="%.2f" y="%.2f" width="%.2f" height="44.5" rx="5" fill="rgb(28,28,30)" stroke="%s" stroke-width="0.7" stroke-opacity="0.45"/>`,
		x+0.35, panelTop, width-0.7, accent))
	b.WriteString(fmt.Sprintf(
		`<text x="%.2f" y="11.2" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="7.5" font-weight="700" letter-spacing="0.35" fill="%s">%s · %s</text>`,
		plotX, accent, svgTextEscape.Replace(strings.ToUpper(panel.Provider)), svgTextEscape.Replace(strings.ToUpper(panel.Label))))
	total := "—"
	if panel.Known {
		total = fmt.Sprintf("%.1f%%", totalSpent(panel.Buckets))
	}
	b.WriteString(fmt.Sprintf(
		`<text x="%.2f" y="11.2" text-anchor="end" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="7.5" font-weight="650" fill="rgb(225,225,230)">%s</text>`,
		x+width-pad, svgTextEscape.Replace(total)))
	b.WriteString(fmt.Sprintf(
		`<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" rx="2" fill="rgb(48,48,52)"/>`,
		plotX, plotTop, plotW, plotBase-plotTop))

	horizon := panel.Now - panel.Start
	if horizon <= 0 {
		horizon = 1
	}
	xOf := func(t int64) float64 {
		return plotX + clamp(float64(t-panel.Start)/float64(horizon), 0, 1)*plotW
	}
	for _, nb := range nightBands(panel.Start, panel.Now, ws, we) {
		x0, x1 := xOf(nb[0]), xOf(nb[1])
		b.WriteString(fmt.Sprintf(
			`<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" fill="black" fill-opacity="0.24"/>`,
			x0, plotTop, x1-x0, plotBase-plotTop))
	}

	n := len(panel.Buckets)
	if n < 1 {
		n = 1
	}
	barW := plotW / float64(n)
	for i := 1; i < n; i++ {
		gx := plotX + float64(i)*barW
		b.WriteString(fmt.Sprintf(
			`<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="rgb(150,150,155)" stroke-width="0.35" stroke-opacity="0.22"/>`,
			gx, plotTop+1, gx, plotBase))
	}

	sustain := 0.0
	if panel.WindowSecs > 0 && panel.BucketSecs > 0 {
		sustain = 100 * float64(panel.BucketSecs) / float64(panel.WindowSecs)
	}
	maxRatio := 1.35
	if sustain > 0 {
		for _, bucket := range panel.Buckets {
			maxRatio = math.Max(maxRatio, bucket.Spent/sustain*1.08)
		}
	}
	usableH := plotBase - plotTop - 2
	if sustain > 0 {
		y := plotBase - usableH/maxRatio
		b.WriteString(fmt.Sprintf(
			`<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="%s" stroke-width="0.55" stroke-opacity="0.72" stroke-dasharray="2 1.5"/>`,
			plotX, y, plotX+plotW, y, accent))
	}
	for i, bucket := range panel.Buckets {
		if bucket.Spent <= 0 || sustain <= 0 {
			continue
		}
		ratio := bucket.Spent / sustain
		h := usableH * ratio / maxRatio
		h = math.Max(h, 1.2)
		bx := plotX + float64(i)*barW + 0.65
		bw := math.Max(0.7, barW-1.3)
		b.WriteString(fmt.Sprintf(
			`<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" rx="0.8" fill="%s" fill-opacity="0.96"/>`,
			bx, plotBase-h, bw, h, svgRGB(burnColor(ratio))))
	}
	b.WriteString(fmt.Sprintf(
		`<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="rgb(185,185,190)" stroke-width="0.45" stroke-opacity="0.5"/>`,
		plotX, plotBase, plotX+plotW, plotBase))
}

// renderBurnChart draws the burn-rate bars for one horizon as SVG bytes, ready
// to hang off a menu item via SetIcon. Each bucket is a full-width bar so its
// area reads as the spend (no invisibly-thin lines); the height auto-scales to
// the busiest bar so the shape always fills the strip. Bars are coloured by how
// the interval's spend compares to the sustainable share of windowSecs. A faint
// grid marks the bucket boundaries and off-hours are shaded darker.
func renderBurnChart(buckets []burnBucket, windowSecs, bucketSecs, start, now int64, ws, we int) []byte {
	const W, H = 120.0, 18.0
	const base, top = 16.5, 2.0
	usable := base - top
	n := len(buckets)
	horizon := now - start
	if horizon <= 0 {
		horizon = 1
	}
	xOf := func(t int64) float64 { return clamp(float64(t-start)/float64(horizon), 0, 1) * W }

	var b strings.Builder
	b.WriteString(fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f">`,
		W, H, W, H))

	// Off-hours shading behind everything.
	for _, nb := range nightBands(start, now, ws, we) {
		x0, x1 := xOf(nb[0]), xOf(nb[1])
		b.WriteString(fmt.Sprintf(
			`<rect x="%.2f" y="0" width="%.2f" height="%.0f" fill="rgb(120,120,120)" fill-opacity="0.16"/>`,
			x0, x1-x0, H))
	}

	// Grid at every bucket boundary + the baseline.
	barW := W / float64(n)
	for i := 0; i <= n; i++ {
		x := float64(i) * barW
		b.WriteString(fmt.Sprintf(
			`<line x1="%.2f" y1="0.5" x2="%.2f" y2="%.2f" stroke="rgb(150,150,150)" stroke-width="0.35" stroke-opacity="0.35"/>`,
			x, x, base))
	}
	b.WriteString(fmt.Sprintf(
		`<line x1="0" y1="%.2f" x2="%.0f" y2="%.2f" stroke="rgb(150,150,150)" stroke-width="0.4" stroke-opacity="0.55"/>`,
		base, W, base))

	maxSpent := 0.0
	for _, bk := range buckets {
		if bk.Spent > maxSpent {
			maxSpent = bk.Spent
		}
	}
	sustain := 100 * float64(bucketSecs) / float64(windowSecs)
	for i, bk := range buckets {
		if bk.Spent <= 0 || maxSpent <= 0 {
			continue
		}
		h := usable * bk.Spent / maxSpent
		if h < 1.4 { // never let a real spend vanish into the baseline
			h = 1.4
		}
		c := burnColor(bk.Spent / sustain)
		x := float64(i)*barW + 0.5
		w := barW - 1.0
		if w < 0.6 {
			w = barW
		}
		b.WriteString(fmt.Sprintf(
			`<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" fill="%s" fill-opacity="0.9" rx="0.4"/>`,
			x, base-h, w, h, svgRGB(c)))
	}

	b.WriteString(`</svg>`)
	return []byte(b.String())
}
