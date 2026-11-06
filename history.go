package main

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// history.go stores a per-series log of raw utilisation readings and turns it
// into a burn-rate (first-derivative) view: how much of a window's quota was
// spent between each pair of successful updates. Unlike the live rate/forecast,
// the charts here never extrapolate — they only ever show measured spend, so an
// incorrect estimate cannot creep in. See renderPairedBurnChart for the drawing.

// histSample is one utilisation reading: cumulative used-% (0..100) at unix
// second T. A window reset shows up as V dropping below the previous sample.
type histSample struct {
	T int64   `json:"t"`
	V float64 `json:"v"`
}

// Series names for the persisted history map. Claude tracks its 5-hour session
// and 7-day weekly counters; Codex its primary and secondary windows.
// The credit series hold cumulative spend in display units: Claude's monthly
// used-credits figure as reported, and for Codex a running counter of balance
// drops (the API only reports the balance, see recordCodexCreditSpend).
const (
	seriesClaudeSession  = "claude_session"
	seriesClaudeWeekly   = "claude_weekly"
	seriesCodexPrimary   = "codex_primary"
	seriesCodexSecondary = "codex_secondary"
	seriesClaudeCredits  = "claude_credits"
	seriesCodexCredits   = "codex_credits"
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

// burnInterval is one observed change between two successful API updates.
// Rate is normalised per second so uneven polling gaps remain comparable.
type burnInterval struct {
	Start int64
	End   int64
	Spent float64
	Rate  float64
}

// burnIntervals turns the history into bars at the resolution the APIs really
// offer. Both providers report quota in whole percent, so a step seen between
// two polls says nothing about which of the preceding flat polls did the
// spending — a per-poll derivative would show a towering spike followed by
// nothing. Each step is therefore spread evenly from the previous counter
// change (or the first sample) up to the poll that revealed it, which is the
// exact average speed over that run. The trailing run still waiting for its
// next step draws as zero rather than being guessed, and a reset ends its run
// with zero spend. A run crossing the horizon boundary is clipped and prorated.
func burnIntervals(s []histSample, now, horizon int64) []burnInterval {
	start := now - horizon
	intervals := make([]burnInterval, 0, len(s))
	runStart := 0
	for i := 1; i < len(s); i++ {
		if s[i].V == s[runStart].V && i < len(s)-1 {
			continue // flat poll inside a run: wait for the step that closes it
		}
		from, to := s[runStart].T, s[i].T
		delta := math.Max(0, s[i].V-s[runStart].V)
		runStart = i
		if to <= start || from >= now || to <= from {
			continue
		}
		drawStart := from
		if drawStart < start {
			drawStart = start
		}
		drawEnd := to
		if drawEnd > now {
			drawEnd = now
		}
		if drawEnd <= drawStart {
			continue
		}
		rate := delta / float64(to-from)
		intervals = append(intervals, burnInterval{
			Start: drawStart,
			End:   drawEnd,
			Spent: rate * float64(drawEnd-drawStart),
			Rate:  rate,
		})
	}
	return intervals
}

// totalSpent is the quota consumed across the whole horizon.
func totalSpent(intervals []burnInterval) float64 {
	return spentSince(intervals, math.MinInt64)
}

// spentSince is the quota consumed by the runs that began at or after t. A
// reset closes its run with zero spend and starts the next one at the sample
// that revealed the drop, so t = that reset counts exactly the new window.
func spentSince(intervals []burnInterval, t int64) float64 {
	sum := 0.0
	for _, interval := range intervals {
		if interval.Start >= t {
			sum += interval.Spent
		}
	}
	return sum
}

// resetsWithin lists the observed resets (samples whose reading dropped below
// the previous one) strictly inside (start, now], oldest first.
func resetsWithin(s []histSample, start, now int64) []int64 {
	var resets []int64
	for i := 1; i < len(s); i++ {
		if s[i].V < s[i-1].V && s[i].T > start && s[i].T <= now {
			resets = append(resets, s[i].T)
		}
	}
	return resets
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

// burnColor maps a burn ratio (a rate over the pace affordable at that moment,
// see ratioAt) to purple -> green -> yellow -> red, matching the dial: under
// pace shades from purple up to green at exactly 1x (lasts precisely to the
// reset, like the credits ring's green), yellow at 1¼x, red from 1½x on.
func burnColor(ratio float64) [3]int {
	if ratio <= 1 {
		return lerpColor(dialPurple, dialGreen, clamp(ratio, 0, 1))
	}
	if ratio <= 1.25 {
		return lerpColor(dialGreen, dialYellow, (ratio-1)/0.25)
	}
	return lerpColor(dialYellow, dialRed, clamp((ratio-1.25)/0.25, 0, 1))
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
	pairedChartHeight = 104.0
)

var claudeChartAccent = [3]int{217, 119, 87}
var codexChartAccent = [3]int{16, 163, 127}
var periodRunwayColor = [3]int{100, 210, 255}
var currentRunwayColor = [3]int{255, 159, 10}
var averageRunwayColor = [3]int{191, 64, 255}

// burnChartPanel is one self-contained provider chart inside a paired history
// row. The two panels deliberately keep their own horizon, grid size and
// quota-window denominator: Claude and Codex can expose different windows even
// though their cards sit side by side.
type burnChartPanel struct {
	Provider     string
	Label        string
	Intervals    []burnInterval
	WindowSecs   int64
	GridSecs     int64
	Start        int64
	Now          int64
	Known        bool
	Accent       [3]int
	UsedPct      float64
	RecentRate   float64
	RateKnown    bool
	AverageRate  float64
	AverageKnown bool
	PeriodLeft   float64
	PeriodKnown  bool
	RateSpan     float64
	LastUseAt    int64
	Trend        float64
	TrendKnown   bool
	ResetAt      int64
	// Unit switches the card from quota percent ("" — the default, where 100 is
	// the whole window) to credits in that unit (see fmtAmount); Remaining is
	// then the balance left, which the affordable pace (see affordableAt) is
	// measured from.
	Unit      string
	Remaining float64
	// SmoothSecs > 0 draws the burn as a blurred area (Gaussian sigma in
	// seconds, see smoothRates) over faint raw bars instead of hard-edged bars
	// alone. Raw bars still define the axis scale and poke out above the
	// curve, so outliers stay visible.
	SmoothSecs float64
	// Samples is the raw history the Intervals were cut from. With it the
	// card can read the quota level back at any past moment and draw the
	// affordable pace as it was then (see affordableAt); without it the
	// reference falls back to the flat sustainableRate.
	Samples []histSample
	// WorkStart/WorkEnd (minutes from local midnight) are the working hours
	// the quota pace is measured on, like the menu's forecast rows: time to a
	// reset counts only working time, so a night or a weekend does not make
	// the affordable pace look tighter than it is. Credits keep the wall
	// clock. Both zero means all day (see workMinutesPerDay).
	WorkStart, WorkEnd int
}

// paceSecs is the time between two moments on the panel's pace clock:
// working seconds for quota cards, wall-clock seconds for credit cards.
func (p burnChartPanel) paceSecs(from, to int64) float64 {
	if p.Unit != "" {
		return math.Max(0, float64(to-from))
	}
	return workSecsBetween(from, to, p.WorkStart, p.WorkEnd)
}

// paceWindowSecs is the whole quota window on the pace clock (0 if unknown).
func (p burnChartPanel) paceWindowSecs() float64 {
	if p.WindowSecs <= 0 {
		return 0
	}
	if p.Unit != "" {
		return float64(p.WindowSecs)
	}
	return float64(p.WindowSecs) * workDayFraction(p.WorkStart, p.WorkEnd)
}

// paceLeft is the time from now to the reset on the pace clock, and whether
// the reset is known.
func (p burnChartPanel) paceLeft() (float64, bool) {
	if !p.PeriodKnown {
		return 0, false
	}
	if p.Unit != "" {
		return p.PeriodLeft, p.PeriodLeft > 0
	}
	if p.ResetAt <= p.Now {
		return 0, false
	}
	return p.paceSecs(p.Now, p.ResetAt), true
}

// smooth reports whether the panel draws the blurred curve.
func (p burnChartPanel) smooth() bool { return p.SmoothSecs > 0 }

// remaining is the budget left in the panel's own units.
func (p burnChartPanel) remaining() float64 {
	if p.Unit == "" {
		return math.Max(0, 100-p.UsedPct)
	}
	return math.Max(0, p.Remaining)
}

// amount formats a value (a rate numerator or a total) in the panel's units.
func (p burnChartPanel) amount(v float64) string {
	if p.Unit == "" {
		return axisPctText(v)
	}
	return fmtAmount(v, p.Unit)
}

// rateText renders a per-second rate per hour or per day following the chart's
// time scale, in the panel's units.
func (p burnChartPanel) rateText(ratePerSec, chartSpanSecs float64, known bool) string {
	if p.Unit == "" {
		return recentRateText(ratePerSec, chartSpanSecs, known)
	}
	if !known {
		return "—"
	}
	unitSecs, unit := 3600.0, "h"
	if chartSpanSecs >= 24*60*60 {
		unitSecs, unit = 24*60*60, "d"
	}
	return fmtAmount(ratePerSec*unitSecs, p.Unit) + "/" + unit
}

// sustainableRate is the flat reference pace the y axis is scaled from (and
// the reference line falls back to when the panel carries no Samples): for
// quota cards the nominal whole-window pace on the working-hours clock, or
// the pace affordable right now when the window length is unknown; for
// credit cards the pace that spends the remaining balance exactly by the
// reset (0 when the reset is unknown).
func (p burnChartPanel) sustainableRate() float64 {
	if window := p.paceWindowSecs(); p.Unit == "" && window > 0 {
		return 100 / window
	}
	if left, ok := p.paceLeft(); ok && left > 0 {
		return p.remaining() / left
	}
	return 0
}

// affordableAt is the pace that could still be afforded at moment t: the
// budget unspent at t divided by the time from t to the reset of the window t
// falls in, on the pace clock (working hours for quota, see paceSecs). It is
// what the speedometer measures against, read back through the history: it
// climbs while nothing is spent (the same remainder over a shrinking wait),
// holds flat outside working hours, drops with every run, and starts over at
// each reset. Past resets are the drops in Samples; the current window ends
// at ResetAt. ok is false when there is nothing to pace against: a credit
// card before its last observed reset, or one with no reset at all. Quota
// cards with an unknown reset fall back to the flat whole-window pace, like
// the needle does.
func (p burnChartPanel) affordableAt(t int64) (float64, bool) {
	if len(p.Samples) == 0 {
		if left, ok := p.paceLeft(); p.Unit != "" && ok && left > 0 {
			return p.remaining() / left, true // 0 when exhausted
		}
		rate := p.sustainableRate()
		return rate, rate > 0
	}
	end, inPast := windowEndAfter(p.Samples, t)
	if !inPast {
		if !p.PeriodKnown || p.ResetAt <= t {
			if window := p.paceWindowSecs(); p.Unit == "" && window > 0 {
				return 100 / window, true
			}
			return 0, false
		}
		end = p.ResetAt
	}
	var remaining float64
	if p.Unit == "" {
		remaining = math.Max(0, 100-usedAt(p.Samples, t))
	} else {
		if inPast {
			return 0, false // the previous period's budget is not recorded
		}
		remaining = math.Max(0, p.Remaining+usedAt(p.Samples, p.Now)-usedAt(p.Samples, t))
	}
	return remaining / math.Max(1, p.paceSecs(t, end)), true
}

// ratioAt compares a measured rate at moment t with the pace affordable then,
// on the same scale as the speedometer (see panelBurnRatio). Unknown
// references read as 0 so the colour stays neutral rather than alarming.
func (p burnChartPanel) ratioAt(rate float64, t int64) float64 {
	afford, ok := p.affordableAt(t)
	switch {
	case !ok || rate <= 0:
		return 0
	case afford <= 1e-12:
		return burnRatioCap
	}
	return math.Min(rate/afford, burnRatioCap)
}

// usedAt reads the history back the way the bars draw it: a counter step is
// spread linearly from the previous change (or the first sample) up to the
// poll that revealed it, a drop (reset) ends its run flat, and the trailing
// run holds its level. Before the first sample the first level is assumed.
func usedAt(s []histSample, t int64) float64 {
	if len(s) == 0 {
		return 0
	}
	if t <= s[0].T {
		return s[0].V
	}
	runStart := 0
	for i := 1; i < len(s); i++ {
		if s[i].V == s[runStart].V && i < len(s)-1 {
			continue
		}
		if t < s[i].T {
			level := s[runStart].V
			if s[i].V > level {
				f := float64(t-s[runStart].T) / float64(s[i].T-s[runStart].T)
				return level + (s[i].V-level)*f
			}
			return level
		}
		runStart = i
	}
	return s[len(s)-1].V
}

// windowEndAfter is the first observed reset strictly after t (the sample
// whose reading dropped below its predecessor), or false when t lies in the
// window that is still open.
func windowEndAfter(s []histSample, t int64) (int64, bool) {
	for i := 1; i < len(s); i++ {
		if s[i].V < s[i-1].V && s[i].T > t {
			return s[i].T, true
		}
	}
	return 0, false
}

// smoothColumns is how many columns the blurred burn curve is sampled at
// across a panel; one per plot pixel would be ~200, so this stays crisp.
const smoothColumns = 160

// smoothSigmaDiv sets the Gaussian width of the short rows relative to their
// horizon: 1h charts blur over ~60s, 5h over ~5min. Runs keep their level;
// only the hard edges between runs are softened. The 7d row picks its width
// from the menu instead (see weekSmoothOptions).
const smoothSigmaDiv = 60.0

// smoothRates integrates the piecewise-constant burn rate into n equal
// columns across [start, now] (exactly, so a narrow run is not lost between
// sample points), then blurs the columns with a Gaussian of sigma seconds.
// Each column's mass is scattered with a kernel normalised over the columns
// that exist, so nothing leaks past the edges: the curve's integral equals
// the raw bars' integral — and the SPENT total — exactly.
func smoothRates(intervals []burnInterval, start, now int64, n int, sigma float64) []float64 {
	if n <= 0 || now <= start {
		return nil
	}
	colW := float64(now-start) / float64(n)
	raw := make([]float64, n)
	for _, iv := range intervals {
		if iv.End <= iv.Start || iv.Rate <= 0 {
			continue
		}
		first := int(math.Floor(float64(iv.Start-start) / colW))
		last := int(math.Ceil(float64(iv.End-start)/colW)) - 1
		for c := max(first, 0); c <= min(last, n-1); c++ {
			c0 := float64(start) + float64(c)*colW
			c1 := c0 + colW
			overlap := math.Min(c1, float64(iv.End)) - math.Max(c0, float64(iv.Start))
			if overlap > 0 {
				raw[c] += iv.Rate * overlap / colW
			}
		}
	}
	sigmaCols := sigma / colW
	if sigmaCols < 0.5 {
		return raw
	}
	radius := int(math.Ceil(3 * sigmaCols))
	kernel := make([]float64, radius+1)
	for k := range kernel {
		kernel[k] = math.Exp(-0.5 * float64(k*k) / (sigmaCols * sigmaCols))
	}
	out := make([]float64, n)
	for j, v := range raw {
		if v == 0 {
			continue
		}
		lo, hi := max(j-radius, 0), min(j+radius, n-1)
		wsum := 0.0
		for i := lo; i <= hi; i++ {
			wsum += kernel[abs(i-j)]
		}
		for i := lo; i <= hi; i++ {
			out[i] += v * kernel[abs(i-j)] / wsum
		}
	}
	return out
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// renderPairedBurnChart draws two independent provider cards in one wide menu
// image. Every bar is one run between quota counter changes, normalised to a
// speed and coloured against the pace affordable at that moment; the dashed
// line is that affordable pace through time (see writeAffordableLine), drawn
// over the burn. The y scale is set from the flat whole-window pace, so it
// stays put while the reference curve climbs toward each reset.
func renderPairedBurnChart(left, right burnChartPanel, ws, we int) []byte {
	const gap = 8.0
	panelW := (pairedChartWidth - gap) / 2
	var b strings.Builder
	b.WriteString(fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f">`,
		pairedChartWidth, pairedChartHeight, pairedChartWidth, pairedChartHeight))
	b.WriteString(`<defs><linearGradient id="burn-speed" x1="0" y1="0" x2="1" y2="0"><stop offset="0%" stop-color="rgb(191,64,255)"/><stop offset="50%" stop-color="rgb(48,209,88)"/><stop offset="62.5%" stop-color="rgb(255,204,0)"/><stop offset="75%" stop-color="rgb(255,69,58)"/><stop offset="100%" stop-color="rgb(255,69,58)"/></linearGradient><linearGradient id="burn-trend" x1="0" y1="0" x2="1" y2="0"><stop offset="0%" stop-color="rgb(191,64,255)"/><stop offset="25%" stop-color="rgb(10,132,255)"/><stop offset="50%" stop-color="rgb(48,209,88)"/><stop offset="75%" stop-color="rgb(255,204,0)"/><stop offset="100%" stop-color="rgb(255,69,58)"/></linearGradient></defs>`)
	left.WorkStart, left.WorkEnd = ws, we
	right.WorkStart, right.WorkEnd = ws, we
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
	sustainableRate := panel.sustainableRate()
	horizon := panel.Now - panel.Start
	if horizon <= 0 {
		horizon = 1
	}
	unitSecs := 3600.0
	if horizon >= 24*60*60 {
		unitSecs = 24 * 60 * 60
	}
	maxRatio := 1.35
	axisMax := 0.0
	if sustainableRate > 0 {
		for _, interval := range panel.Intervals {
			maxRatio = math.Max(maxRatio, interval.Rate/sustainableRate*1.08)
		}
		axisMax = sustainableRate * unitSecs * maxRatio
	} else if panel.Unit != "" {
		// A credit card without a reset has no reference pace: scale the axis to
		// the bars themselves so the spend is still visible.
		for _, interval := range panel.Intervals {
			axisMax = math.Max(axisMax, interval.Rate*unitSecs*1.08)
		}
	}
	b.WriteString(fmt.Sprintf(
		`<rect x="%.2f" y="%.2f" width="%.2f" height="102.5" rx="7" fill="rgb(28,28,30)" stroke="%s" stroke-width="0.8" stroke-opacity="0.48"/>`,
		x+0.35, panelTop, width-0.7, accent))
	title := fmt.Sprintf("%s %s", strings.ToUpper(panel.Provider), strings.ToUpper(panel.Label))
	b.WriteString(fmt.Sprintf(
		`<text x="%.2f" y="12.5" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="9" font-weight="700" letter-spacing="0.2" fill="%s">%s</text>`,
		plotX, accent, svgTextEscape.Replace(title)))
	// SPENT counts from the last reset in view (or the chart's start when no
	// reset is visible), so the previous window's burn is not added to the
	// current one; the reset itself is drawn as a vertical line.
	resets := resetsWithin(panel.Samples, panel.Start, panel.Now)
	since := panel.Start
	if len(resets) > 0 {
		since = resets[len(resets)-1]
	}
	total := "SPENT —"
	if panel.Known && panel.Unit == "" {
		total = fmt.Sprintf("SPENT %.1f%%", spentSince(panel.Intervals, since))
	} else if panel.Known {
		total = "SPENT " + fmtAmount(spentSince(panel.Intervals, since), panel.Unit)
	}
	b.WriteString(fmt.Sprintf(
		`<text x="%.2f" y="12.5" text-anchor="end" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="9" font-weight="650" fill="rgb(225,225,230)">%s</text>`,
		x+width-pad, svgTextEscape.Replace(total)))
	b.WriteString(fmt.Sprintf(
		`<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" rx="2" fill="rgb(48,48,52)"/>`,
		plotX, plotTop, plotW, plotBase-plotTop))

	xOf := func(t int64) float64 {
		return plotX + clamp(float64(t-panel.Start)/float64(horizon), 0, 1)*plotW
	}
	for _, nb := range nightBands(panel.Start, panel.Now, ws, we) {
		x0, x1 := xOf(nb[0]), xOf(nb[1])
		b.WriteString(fmt.Sprintf(
			`<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" fill="black" fill-opacity="0.24"/>`,
			x0, plotTop, x1-x0, plotBase-plotTop))
	}

	n := 1
	if panel.GridSecs > 0 {
		n = int(horizon / panel.GridSecs)
	}
	if n < 1 {
		n = 1
	}
	gridW := plotW / float64(n)
	for i := 1; i < n; i++ {
		gx := plotX + float64(i)*gridW
		b.WriteString(fmt.Sprintf(
			`<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="rgb(150,150,155)" stroke-width="0.35" stroke-opacity="0.22"/>`,
			gx, plotTop+1, gx, plotBase))
	}

	// Three labelled horizontal levels make the independently scaled cards
	// readable. Values are quota consumption speed per hour/day.
	for _, fraction := range []float64{1, 0.5, 0} {
		y := plotTop + (1-fraction)*(plotBase-plotTop)
		b.WriteString(fmt.Sprintf(
			`<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="rgb(185,185,190)" stroke-width="0.35" stroke-opacity="0.30"/>`,
			plotX, y, plotX+plotW, y))
		b.WriteString(fmt.Sprintf(
			`<text data-axis-label="true" x="%.2f" y="%.2f" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="5.5" font-weight="550" fill="rgb(165,165,172)">%s</text>`,
			axisX, y+2, panel.amount(axisMax*fraction)+"/"+axisUnit(horizon)))
	}
	usableH := plotBase - plotTop - 2
	if panel.Known && axisMax > 0 {
		barOpacity := 0.96
		if panel.smooth() {
			barOpacity = 0.3 // outliers above the curve stay visible as faint stubs
		}
		for _, interval := range panel.Intervals {
			ratio := panel.ratioAt(interval.Rate, (interval.Start+interval.End)/2)
			h := math.Max(1, usableH*interval.Rate*unitSecs/axisMax)
			x0, x1 := xOf(interval.Start), xOf(interval.End)
			barW := math.Max(0.55, x1-x0-0.35)
			barX := clamp((x0+x1-barW)/2, plotX, plotX+plotW-barW)
			b.WriteString(fmt.Sprintf(
				`<rect data-burn-bar="true" data-burn-ratio="%.3f" x="%.2f" y="%.2f" width="%.2f" height="%.2f" rx="0.55" fill="%s" fill-opacity="%.2f"/>`,
				ratio, barX, plotBase-h, barW, h, svgRGB(burnColor(ratio)), barOpacity))
		}
		if panel.smooth() {
			writeSmoothArea(b, panel, plotX, plotW, plotTop, plotBase, usableH, axisMax, unitSecs)
		}
	}
	for _, reset := range resets {
		rx := xOf(reset)
		b.WriteString(fmt.Sprintf(
			`<line data-reset-line="true" x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="rgb(235,235,240)" stroke-width="0.7" stroke-opacity="0.85" stroke-dasharray="1.2 0.8"/>`,
			rx, plotTop, rx, plotBase))
	}
	if axisMax > 0 {
		writeAffordableLine(b, panel, plotX, plotW, plotTop, plotBase, usableH, axisMax, unitSecs, accent)
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
	writeRunwayStats(b, panel, plotX, x+width-pad, 69)

	measurementText, endText := "measured —", "current empty —"
	if panel.WindowSecs > 0 {
		usedAt := time.Unix(panel.LastUseAt, 0)
		if panel.LastUseAt <= 0 {
			usedAt = time.Time{}
		}
		measurementText = recentMeasurementText(panel.RecentRate, panel.RateSpan, usedAt, time.Unix(panel.Now, 0))
		resetAt := time.Unix(panel.ResetAt, 0)
		if panel.ResetAt <= 0 {
			resetAt = time.Time{}
		}
		// A credit card with no balance and no reset is unlimited: there is
		// nothing to run out of.
		hasBudget := panel.Unit == "" || panel.Remaining > 0 || panel.PeriodKnown
		if panel.RateKnown && panel.RecentRate <= 1e-9 {
			endText = "current lasts to reset"
		} else if panel.RateKnown && hasBudget {
			endText = "current " + budgetEndText(panel.remaining(), panel.RecentRate, resetAt, time.Unix(panel.Now, 0))
		}
	}
	b.WriteString(fmt.Sprintf(
		`<text x="%.2f" y="85" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="6.5" font-weight="550" fill="rgb(180,180,186)">%s</text>`,
		plotX, svgTextEscape.Replace(strings.ToUpper(measurementText))))
	b.WriteString(fmt.Sprintf(
		`<text x="%.2f" y="98" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="7" font-weight="600" fill="%s">%s</text>`,
		plotX, svgRGB(currentRunwayColor), svgTextEscape.Replace(strings.ToUpper(endText))))
	writeBurnSpeedometer(b, panel, x+width-20, 98)
}

func writeRunwayStats(b *strings.Builder, panel burnChartPanel, left, right, y float64) {
	current := budgetRunwayText(panel.remaining(), panel.RecentRate, panel.RateKnown)
	average := budgetRunwayText(panel.remaining(), panel.AverageRate, panel.AverageKnown)
	averageRate := panel.rateText(panel.AverageRate, float64(panel.WindowSecs), panel.AverageKnown)
	stats := []struct {
		kind, anchor, text string
		x                  float64
		color              [3]int
	}{
		{"period", "start", "RESET " + quotaDurationText(panel.PeriodLeft, panel.PeriodKnown), left, periodRunwayColor},
		{"current", "middle", "CURRENT " + strings.TrimSuffix(current, " left"), (left + right) / 2, currentRunwayColor},
		{"average", "end", "AVG " + strings.TrimSuffix(average, " left") + " · " + averageRate, right, averageRunwayColor},
	}
	for _, stat := range stats {
		b.WriteString(fmt.Sprintf(
			`<text data-runway-kind="%s" x="%.2f" y="%.2f" text-anchor="%s" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="5.8" font-weight="700" fill="%s">%s</text>`,
			stat.kind, stat.x, y, stat.anchor, svgRGB(stat.color), svgTextEscape.Replace(stat.text)))
	}
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

func axisUnit(horizonSecs int64) string {
	if horizonSecs >= 24*60*60 {
		return "d"
	}
	return "h"
}

func chartTimeText(t time.Time, horizonSecs int64) string {
	if horizonSecs >= 24*60*60 {
		return t.Format("Jan 02")
	}
	return t.Format("15:04")
}

// panelBurnRatio is the speedometer reading: the last measured rate against the
// pace that can still be afforded before this window's reset (remaining quota
// over remaining time). Near a reset with quota to spare the needle relaxes even
// under heavy use; with little quota and a long wait it climbs on modest use.
// A credit card with nothing finite to pace against (unlimited, or no reset)
// reads 0 rather than pegging the needle.
func panelBurnRatio(panel burnChartPanel) float64 {
	if panel.Unit != "" {
		return panel.ratioAt(panel.RecentRate, panel.Now)
	}
	left, known := panel.paceLeft()
	return affordableBurnRatio(panel.RecentRate, panel.UsedPct, left, panel.paceWindowSecs(), known)
}

// speedTicks are the fixed affordable-pace thresholds notched on the speedometer
// arc: ½x (half pace), 1x (exactly lasting to the reset — where green peaks)
// and 1½x (where red begins), on the burnColor scale.
var speedTicks = []float64{0.5, 1, 1.5}

// speedAngle maps a burn ratio to the semicircle: 0 on the left, 2x+ on the right.
func speedAngle(ratio float64) float64 {
	return math.Pi * (1 - clamp(ratio/2, 0, 1))
}

func writeBurnSpeedometer(b *strings.Builder, panel burnChartPanel, cx, cy float64) {
	const r = 15.0
	ratio := panelBurnRatio(panel)
	needle := dialGray
	if _, ok := panel.affordableAt(panel.Now); ok && panel.RateKnown {
		needle = burnColor(ratio)
	}
	chartSpan := float64(panel.Now - panel.Start)
	b.WriteString(fmt.Sprintf(
		`<text data-speed-label="true" x="%.2f" y="%.2f" text-anchor="middle" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="6.5" font-weight="700" fill="%s">%s</text>`,
		cx, cy-r-5.5, svgRGB(needle), svgTextEscape.Replace(strings.ToUpper(panel.rateText(panel.RecentRate, chartSpan, panel.RateKnown)))))
	b.WriteString(fmt.Sprintf(
		`<path d="M%.2f,%.2f A%.2f,%.2f 0 0 1 %.2f,%.2f" fill="none" stroke="rgb(75,75,80)" stroke-width="4" stroke-linecap="round"/>`,
		cx-r, cy, r, r, cx+r, cy))
	b.WriteString(fmt.Sprintf(
		`<path d="M%.2f,%.2f A%.2f,%.2f 0 0 1 %.2f,%.2f" fill="none" stroke="url(#burn-speed)" stroke-width="2.8" stroke-linecap="round" opacity="0.95"/>`,
		cx-r, cy, r, r, cx+r, cy))
	writeSpeedTicks(b, cx, cy, r)
	writeNominalPaceMark(b, panel, cx, cy, r)
	writeBurnTrendBar(b, panel, cx, cy, r-6)
	angle := speedAngle(ratio)
	nx := cx + math.Cos(angle)*(r-3)
	ny := cy - math.Sin(angle)*(r-3)
	b.WriteString(fmt.Sprintf(
		`<line data-speed-needle="true" data-speed-ratio="%.3f" x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="%s" stroke-width="1.5" stroke-linecap="round"/>`,
		ratio, cx, cy, nx, ny, svgRGB(needle)))
	b.WriteString(fmt.Sprintf(`<circle cx="%.2f" cy="%.2f" r="2.1" fill="%s"/>`, cx, cy, svgRGB(needle)))
}

// writeSpeedTicks notches the speedometer arc at each speedTicks threshold, so
// the needle can be read against "half of what I can afford", "exactly lasting
// to reset" and "burning 1½x too fast" rather than against colour alone.
func writeSpeedTicks(b *strings.Builder, cx, cy, r float64) {
	in, out := r-2.6, r+2.6
	for _, ratio := range speedTicks {
		angle := speedAngle(ratio)
		c, s := math.Cos(angle), math.Sin(angle)
		b.WriteString(fmt.Sprintf(
			`<line data-speed-tick="%.1f" x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="rgb(20,20,22)" stroke-width="0.7" stroke-opacity="0.9"/>`,
			ratio, cx+in*c, cy-in*s, cx+out*c, cy-out*s))
	}
}

// writeNominalPaceMark draws a small pointer outside the arc where the plain
// whole-window pace (100% over the window) sits on the affordable scale. Left
// of the 1x tick means the remaining budget allows faster-than-normal use;
// right of it means even the normal pace is now too fast. It moves as quota and
// time drain, while the ticks stay put.
func writeNominalPaceMark(b *strings.Builder, panel burnChartPanel, cx, cy, r float64) {
	if panel.Unit != "" {
		return // credits have no nominal whole-window pace
	}
	left, known := panel.paceLeft()
	ratio, ok := nominalPaceRatio(panel.UsedPct, left, panel.paceWindowSecs(), known)
	if !ok {
		return
	}
	angle := speedAngle(ratio)
	c, s := math.Cos(angle), math.Sin(angle)
	in, out := r+2.3, r+4.2
	b.WriteString(fmt.Sprintf(
		`<line data-pace-mark="true" data-pace-ratio="%.3f" x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="%s" stroke-width="1.3" stroke-linecap="round" stroke-opacity="0.95"/>`,
		ratio, cx+in*c, cy-in*s, cx+out*c, cy-out*s, svgRGB(panel.Accent)))
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

// writeSmoothArea draws the blurred burn curve as a filled area whose colour
// follows the same speed scale as the bars, via a horizontal gradient with
// one stop per column.
func writeSmoothArea(b *strings.Builder, panel burnChartPanel, plotX, plotW, plotTop, plotBase, usableH, axisMax, unitSecs float64) {
	curve := smoothRates(panel.Intervals, panel.Start, panel.Now, smoothColumns, panel.SmoothSecs)
	if len(curve) == 0 {
		return
	}
	id := fmt.Sprintf("burn-area-%d", int(plotX))
	colW := plotW / float64(len(curve))
	// The filled area closes along the baseline; the stroke is a separate open
	// line over the top edge, lifted only where the curve is above zero so the
	// idle stretches don't get a coloured line along the axis.
	var stops, top, edge strings.Builder
	penDown := false
	colSecs := float64(panel.Now-panel.Start) / float64(len(curve))
	for i, rate := range curve {
		ratio := panel.ratioAt(rate, panel.Start+int64((float64(i)+0.5)*colSecs))
		stops.WriteString(fmt.Sprintf(`<stop offset="%.4f" stop-color="%s"/>`,
			(float64(i)+0.5)/float64(len(curve)), svgRGB(burnColor(ratio))))
		h := clamp(usableH*rate*unitSecs/axisMax, 0, plotBase-plotTop-1)
		x, y := plotX+(float64(i)+0.5)*colW, plotBase-h
		top.WriteString(fmt.Sprintf("L%.2f,%.2f", x, y))
		switch {
		case h < 0.25:
			penDown = false
		case penDown:
			edge.WriteString(fmt.Sprintf("L%.2f,%.2f", x, y))
		default:
			edge.WriteString(fmt.Sprintf("M%.2f,%.2f", x, y))
			penDown = true
		}
	}
	b.WriteString(fmt.Sprintf(
		`<defs><linearGradient id="%s" gradientUnits="userSpaceOnUse" x1="%.2f" y1="0" x2="%.2f" y2="0">%s</linearGradient></defs>`,
		id, plotX, plotX+plotW, stops.String()))
	b.WriteString(fmt.Sprintf(
		`<path data-burn-area="true" d="M%.2f,%.2f%sL%.2f,%.2fZ" fill="url(#%s)" fill-opacity="0.88"/>`,
		plotX, plotBase, top.String(), plotX+plotW, plotBase, id))
	if edge.Len() > 0 {
		b.WriteString(fmt.Sprintf(
			`<path d="%s" fill="none" stroke="url(#%s)" stroke-width="0.7" stroke-linejoin="round" stroke-linecap="round"/>`,
			edge.String(), id))
	}
}

// writeAffordableLine draws the dashed reference pace on top of the burn: the
// affordable pace at each moment of the chart (see affordableAt), sampled one
// value per column. It is clipped to the top of the plot — just before a reset
// almost any speed is affordable — and lifts the pen where no reference exists.
func writeAffordableLine(b *strings.Builder, panel burnChartPanel, plotX, plotW, plotTop, plotBase, usableH, axisMax, unitSecs float64, accent string) {
	colSecs := float64(panel.Now-panel.Start) / smoothColumns
	colW := plotW / smoothColumns
	var path strings.Builder
	penDown := false
	for i := 0; i < smoothColumns; i++ {
		t := panel.Start + int64((float64(i)+0.5)*colSecs)
		afford, ok := panel.affordableAt(t)
		if !ok {
			penDown = false
			continue
		}
		x := plotX + (float64(i)+0.5)*colW
		y := clamp(plotBase-usableH*afford*unitSecs/axisMax, plotTop, plotBase)
		if penDown {
			path.WriteString(fmt.Sprintf("L%.2f,%.2f", x, y))
		} else {
			path.WriteString(fmt.Sprintf("M%.2f,%.2f", x, y))
			penDown = true
		}
	}
	if path.Len() == 0 {
		return
	}
	b.WriteString(fmt.Sprintf(
		`<path data-pace-line="true" d="%s" fill="none" stroke="%s" stroke-width="0.55" stroke-opacity="0.85" stroke-linejoin="round" stroke-dasharray="2 1.5"/>`,
		path.String(), accent))
}
