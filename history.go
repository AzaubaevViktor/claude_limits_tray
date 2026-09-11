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
// estimate can't creep in. See renderPairedBurnChart for the drawing.

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
	histFullRes      = int64(24 * 3600)     // keep every sample from the last 24h
	histThinStep     = int64(15 * 60)       // older than that: thin to >= 15m apart
	histMaxAge       = int64(7 * 24 * 3600) // drop anything older than 7 days
	lastUseFreshSecs = int64(10 * 60)       // one maximum adaptive poll interval
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

type useRate struct {
	Rate float64
	Span float64
	At   int64
	OK   bool
}

// windowUseRate measures the utilisation change over the most recent sampling
// window. Flat polls remain in the interval, so this is a real rolling rate
// rather than the rate of only the last positive burst. Samples before the most
// recent counter reset are excluded. When polling is sparse, the immediately
// preceding sample is used so the result still has a real denominator.
func windowUseRate(samples []histSample, now, windowSecs int64) useRate {
	if len(samples) < 2 {
		return useRate{}
	}
	floor := 0
	for i := len(samples) - 1; i >= 1; i-- {
		if samples[i].V < samples[i-1].V {
			floor = i
			break
		}
	}
	end := len(samples) - 1
	lastUseAt := int64(0)
	for i := end; i > floor; i-- {
		if samples[i].V > samples[i-1].V {
			lastUseAt = samples[i].T
			break
		}
	}
	if end <= floor || now-samples[end].T > lastUseFreshSecs {
		return useRate{At: lastUseAt}
	}
	start := end - 1
	cutoff := samples[end].T - windowSecs
	for i := floor; i < end; i++ {
		if samples[i].T >= cutoff {
			start = i
			break
		}
	}
	dt := float64(samples[end].T - samples[start].T)
	if dt < rateMinDt {
		return useRate{At: lastUseAt}
	}
	return useRate{
		Rate: math.Max(0, samples[end].V-samples[start].V) / dt,
		Span: dt,
		At:   lastUseAt,
		OK:   true,
	}
}

// rateTrend compares the immediate five-minute speed with the primary
// fifteen-minute speed. The symmetric result maps -1..1 to the left/right
// halves of the trend bar; zero means the rates are equal.
func rateTrend(fast, slow useRate) (float64, bool) {
	if !fast.OK || !slow.OK {
		return 0, false
	}
	scale := math.Max(fast.Rate, slow.Rate)
	if scale <= 1e-9 {
		return 0, true
	}
	return clamp((fast.Rate-slow.Rate)/scale, -1, 1), true
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
// to purple -> green -> yellow -> red. Half the sustainable rate is green,
// exactly sustainable is yellow, and 1.5x or more is red.
func burnColor(ratio float64) [3]int {
	if ratio <= 0.5 {
		return lerpColor(dialPurple, dialGreen, clamp(ratio/0.5, 0, 1))
	}
	if ratio <= 1 {
		return lerpColor(dialGreen, dialYellow, clamp((ratio-0.5)/0.5, 0, 1))
	}
	return lerpColor(dialYellow, dialRed, clamp((ratio-1)/0.5, 0, 1))
}

var trendBlue = [3]int{10, 132, 255}

// trendColor follows the two-sided acceleration scale. The unchanged centre
// is green; acceleration moves through yellow to red, while deceleration moves
// through blue to purple.
func trendColor(trend float64) [3]int {
	trend = clamp(trend, -1, 1)
	if trend >= 0 {
		if trend <= 0.5 {
			return lerpColor(dialGreen, dialYellow, trend/0.5)
		}
		return lerpColor(dialYellow, dialRed, (trend-0.5)/0.5)
	}
	strength := -trend
	if strength <= 0.5 {
		return lerpColor(dialGreen, trendBlue, strength/0.5)
	}
	return lerpColor(trendBlue, dialPurple, (strength-0.5)/0.5)
}

const (
	pairedChartWidth  = 480.0
	pairedChartHeight = 92.0
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
	UsedPct    float64
	RecentRate float64
	RateKnown  bool
	RateSpan   float64
	LastUseAt  int64
	Trend      float64
	TrendKnown bool
	ResetAt    int64
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
	b.WriteString(`<defs><linearGradient id="burn-speed" x1="0" y1="0" x2="1" y2="0"><stop offset="0%" stop-color="rgb(191,64,255)"/><stop offset="25%" stop-color="rgb(48,209,88)"/><stop offset="50%" stop-color="rgb(255,204,0)"/><stop offset="75%" stop-color="rgb(255,69,58)"/><stop offset="100%" stop-color="rgb(255,69,58)"/></linearGradient><linearGradient id="burn-trend" x1="0" y1="0" x2="1" y2="0"><stop offset="0%" stop-color="rgb(191,64,255)"/><stop offset="25%" stop-color="rgb(10,132,255)"/><stop offset="50%" stop-color="rgb(48,209,88)"/><stop offset="75%" stop-color="rgb(255,204,0)"/><stop offset="100%" stop-color="rgb(255,69,58)"/></linearGradient></defs>`)
	writeBurnPanel(&b, left, 0, panelW, ws, we)
	writeBurnPanel(&b, right, panelW+gap, panelW, ws, we)
	b.WriteString(`</svg>`)
	return []byte(b.String())
}

func writeBurnPanel(b *strings.Builder, panel burnChartPanel, x, width float64, ws, we int) {
	const (
		panelTop   = 0.5
		plotTop    = 18.0
		plotBase   = 49.0
		pad        = 8.0
		axisGutter = 27.0
	)
	plotX, plotW := x+pad, width-2*pad-axisGutter
	axisX := plotX + plotW + 4
	accent := svgRGB(panel.Accent)
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
	axisMax := sustain * maxRatio
	b.WriteString(fmt.Sprintf(
		`<rect x="%.2f" y="%.2f" width="%.2f" height="90.5" rx="7" fill="rgb(28,28,30)" stroke="%s" stroke-width="0.8" stroke-opacity="0.48"/>`,
		x+0.35, panelTop, width-0.7, accent))
	b.WriteString(fmt.Sprintf(
		`<text x="%.2f" y="12.5" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="9" font-weight="700" letter-spacing="0.45" fill="%s">%s · %s</text>`,
		plotX, accent, svgTextEscape.Replace(strings.ToUpper(panel.Provider)), svgTextEscape.Replace(strings.ToUpper(panel.Label))))
	total := "SPENT —"
	if panel.Known {
		total = fmt.Sprintf("SPENT %.1f%%", totalSpent(panel.Buckets))
	}
	b.WriteString(fmt.Sprintf(
		`<text x="%.2f" y="12.5" text-anchor="end" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="9" font-weight="650" fill="rgb(225,225,230)">%s</text>`,
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

	// Three labelled horizontal levels make the independently scaled cards
	// readable. Values are quota percentage spent per bar interval.
	for _, fraction := range []float64{1, 0.5, 0} {
		y := plotTop + (1-fraction)*(plotBase-plotTop)
		b.WriteString(fmt.Sprintf(
			`<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="rgb(185,185,190)" stroke-width="0.35" stroke-opacity="0.30"/>`,
			plotX, y, plotX+plotW, y))
		b.WriteString(fmt.Sprintf(
			`<text data-axis-label="true" x="%.2f" y="%.2f" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="5.5" font-weight="550" fill="rgb(165,165,172)">%s</text>`,
			axisX, y+2, axisPctText(axisMax*fraction)))
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
	for _, mark := range []struct {
		fraction float64
		anchor   string
	}{
		{0, "start"},
		{0.5, "middle"},
		{1, "end"},
	} {
		t := panel.Start + int64(float64(horizon)*mark.fraction)
		b.WriteString(fmt.Sprintf(
			`<text data-time-label="true" x="%.2f" y="57" text-anchor="%s" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="5.5" font-weight="550" fill="rgb(165,165,172)">%s</text>`,
			plotX+plotW*mark.fraction, mark.anchor, chartTimeText(time.Unix(t, 0), horizon)))
	}

	measurementText, endText := "measured —", "empty —"
	if panel.WindowSecs > 0 {
		usedAt := time.Unix(panel.LastUseAt, 0)
		if panel.LastUseAt <= 0 {
			usedAt = time.Time{}
		}
		measurementText = recentMeasurementText(panel.RecentRate, panel.RateSpan, usedAt, time.Unix(panel.Now, 0))
		endText = quotaEndText(panel.UsedPct, panel.RecentRate, time.Unix(panel.ResetAt, 0), time.Unix(panel.Now, 0))
		if panel.ResetAt <= 0 {
			endText = quotaEndText(panel.UsedPct, panel.RecentRate, time.Time{}, time.Unix(panel.Now, 0))
		}
	}
	b.WriteString(fmt.Sprintf(
		`<text x="%.2f" y="73" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="6.5" font-weight="550" fill="rgb(180,180,186)">%s</text>`,
		plotX, svgTextEscape.Replace(strings.ToUpper(measurementText))))
	b.WriteString(fmt.Sprintf(
		`<text x="%.2f" y="86" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="7" font-weight="600" fill="rgb(215,215,220)">%s</text>`,
		plotX, svgTextEscape.Replace(strings.ToUpper(endText))))
	writeBurnSpeedometer(b, panel, x+width-20, 86)
}

func axisPctText(v float64) string {
	switch {
	case v >= 10:
		return fmt.Sprintf("%.0f%%", v)
	case v >= 1:
		return fmt.Sprintf("%.1f%%", v)
	default:
		return fmt.Sprintf("%.2f%%", v)
	}
}

func chartTimeText(t time.Time, horizonSecs int64) string {
	if horizonSecs >= 24*60*60 {
		return t.Format("Jan 02")
	}
	return t.Format("15:04")
}

func writeBurnSpeedometer(b *strings.Builder, panel burnChartPanel, cx, cy float64) {
	const r = 15.0
	ratio := recentBurnRatio(panel.RecentRate, float64(panel.WindowSecs))
	needle := dialGray
	if panel.WindowSecs > 0 && panel.RateKnown {
		needle = burnColor(ratio)
	}
	chartSpan := float64(panel.Now - panel.Start)
	b.WriteString(fmt.Sprintf(
		`<text data-speed-label="true" x="%.2f" y="%.2f" text-anchor="middle" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="6.5" font-weight="700" fill="%s">%s</text>`,
		cx, cy-r-4, svgRGB(needle), svgTextEscape.Replace(strings.ToUpper(recentRateText(panel.RecentRate, chartSpan, panel.RateKnown)))))
	b.WriteString(fmt.Sprintf(
		`<path d="M%.2f,%.2f A%.2f,%.2f 0 0 1 %.2f,%.2f" fill="none" stroke="rgb(75,75,80)" stroke-width="4" stroke-linecap="round"/>`,
		cx-r, cy, r, r, cx+r, cy))
	b.WriteString(fmt.Sprintf(
		`<path d="M%.2f,%.2f A%.2f,%.2f 0 0 1 %.2f,%.2f" fill="none" stroke="url(#burn-speed)" stroke-width="2.8" stroke-linecap="round" opacity="0.95"/>`,
		cx-r, cy, r, r, cx+r, cy))
	writeBurnTrendBar(b, panel, cx, cy, r-6)
	position := clamp(ratio/2, 0, 1)
	angle := math.Pi * (1 - position)
	nx := cx + math.Cos(angle)*(r-3)
	ny := cy - math.Sin(angle)*(r-3)
	b.WriteString(fmt.Sprintf(
		`<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="%s" stroke-width="1.5" stroke-linecap="round"/>`,
		cx, cy, nx, ny, svgRGB(needle)))
	b.WriteString(fmt.Sprintf(`<circle cx="%.2f" cy="%.2f" r="2.1" fill="%s"/>`, cx, cy, svgRGB(needle)))
}

// writeBurnTrendBar draws a smaller concentric semicircular bar beneath the
// speedometer needle. It compares 5m with 15m: centre is unchanged, right is
// accelerating, and left is decelerating. The value is an arc, never a second
// needle, so the two readings remain visually distinct.
func writeBurnTrendBar(b *strings.Builder, panel burnChartPanel, cx, cy, r float64) {
	b.WriteString(fmt.Sprintf(
		`<path data-trend-bar="true" d="M%.2f,%.2f A%.2f,%.2f 0 0 1 %.2f,%.2f" fill="none" stroke="url(#burn-trend)" stroke-width="2.2" stroke-linecap="round" opacity="0.35"/>`,
		cx-r, cy, r, r, cx+r, cy))
	if !panel.TrendKnown {
		return
	}
	trend := clamp(panel.Trend, -1, 1)
	from, to := math.Min(0, trend), math.Max(0, trend)
	if math.Abs(trend) < 0.04 {
		from, to = -0.04, 0.04
	}
	x0, y0 := trendPoint(cx, cy, r, from)
	x1, y1 := trendPoint(cx, cy, r, to)
	b.WriteString(fmt.Sprintf(
		`<path data-trend-value="true" data-trend="%.3f" d="M%.2f,%.2f A%.2f,%.2f 0 0 1 %.2f,%.2f" fill="none" stroke="%s" stroke-width="3.4" stroke-linecap="round"/>`,
		trend, x0, y0, r, r, x1, y1, svgRGB(trendColor(trend))))
}

func trendPoint(cx, cy, r, position float64) (float64, float64) {
	angle := math.Pi * (1 - clamp((position+1)/2, 0, 1))
	return cx + math.Cos(angle)*r, cy - math.Sin(angle)*r
}
