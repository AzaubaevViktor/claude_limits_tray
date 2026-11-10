package main

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// The quota strip sits above the burn charts and carries the two readings the
// cards used to bury in 5-point type: when each window resets, and how fast it
// is being spent. One row per real quota window — the charts below show the
// same window over several horizons, so repeating the countdown on every card
// only made it smaller.

const (
	stripPad      = 8.0
	stripHeadBase = 11.0 // provider-name baseline
	stripRowBase  = 28.0 // first row's text baseline
	stripRowStep  = 18.0
	stripBottom   = 8.0  // below the last row's baseline
	stripTimeX    = 30.0 // countdown column, from the side's left edge
	stripBarAt    = 0.44 // speed bar column, as a share of the side's width

	stripCountdownSize = 13.0 // the row's headline: time left in the window
)

// stripStampSizes are the sizes the reset stamp is tried at, largest first; the
// last one is used even if it overruns, since a cramped stamp still beats none.
var stripStampSizes = []float64{7.5, 7, 6.5, 6}

// stripHeight is the image height for the taller of the two sides.
func stripHeight(rows int) float64 {
	if rows < 1 {
		rows = 1
	}
	return stripRowBase + stripRowStep*float64(rows-1) + stripBottom
}

// stripRow is one quota window on the strip.
type stripRow struct {
	Label string // "5H", "7D", "CREDITS"
	Panel burnChartPanel
}

// stripSide is one provider's column.
type stripSide struct {
	Provider string
	Accent   [3]int
	Rows     []stripRow
}

// renderQuotaStrip draws both providers' windows side by side, and reports the
// image height it chose for the row count.
func renderQuotaStrip(left, right stripSide, now time.Time) ([]byte, float64) {
	const gap = 8.0
	sideW := (pairedChartWidth - 2*chartMargin - gap) / 2
	h := stripHeight(max(len(left.Rows), len(right.Rows)))
	imgH := h + 2*chartMargin
	var b strings.Builder
	b.WriteString(fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f">`,
		pairedChartWidth, imgH, pairedChartWidth, imgH))
	b.WriteString(chartGradientDefs)
	// Both sides are drawn card-relative and shifted into the margin together,
	// the same way the burn cards are (see renderPairedBurnChart).
	b.WriteString(fmt.Sprintf(`<g transform="translate(%.2f,%.2f)">`, chartMargin, chartMargin))
	writeStripSide(&b, left, 0, sideW, h, now)
	writeStripSide(&b, right, sideW+gap, sideW, h, now)
	b.WriteString(`</g></svg>`)
	return []byte(b.String()), imgH
}

func writeStripSide(b *strings.Builder, side stripSide, x, width, height float64, now time.Time) {
	accent := svgRGB(side.Accent)
	b.WriteString(fmt.Sprintf(
		`<rect x="%.2f" y="0.5" width="%.2f" height="%.2f" rx="7" fill="%s" stroke="%s" stroke-width="0.8" stroke-opacity="0.48"/>`,
		x+0.35, width-0.7, height-1, svgRGB(chartPalette.Panel), accent))
	b.WriteString(fmt.Sprintf(
		`<text x="%.2f" y="%.2f" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="8" font-weight="700" letter-spacing="0.2" fill="%s">%s</text>`,
		x+stripPad, stripHeadBase, accent, svgTextEscape.Replace(strings.ToUpper(side.Provider))))
	// Column captions, parked over the fields they name.
	for _, cap := range []struct {
		text   string
		x      float64
		anchor string
	}{
		{"RESETS IN", x + stripTimeX + 24, "start"},
		{"SPEED VS PACE", x + width*stripBarAt, "start"},
		{"LEFT · AVG", x + width - stripPad, "end"},
	} {
		b.WriteString(fmt.Sprintf(
			`<text x="%.2f" y="%.2f" text-anchor="%s" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="6" font-weight="600" letter-spacing="0.2" fill="%s">%s</text>`,
			cap.x, stripHeadBase, cap.anchor, svgRGB(chartPalette.Caption), cap.text))
	}
	for i, row := range side.Rows {
		writeStripRow(b, row, x, width, stripRowBase+stripRowStep*float64(i), now)
	}
}

// writeStripRow lays one window out in fixed columns: what it is, how long is
// left of it, how fast it is going, and how long that rate would last.
func writeStripRow(b *strings.Builder, row stripRow, x, width, y float64, now time.Time) {
	panel := row.Panel
	b.WriteString(fmt.Sprintf(
		`<text data-strip-row="%s" x="%.2f" y="%.2f" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="8" font-weight="700" fill="%s">%s</text>`,
		svgTextEscape.Replace(row.Label), x+stripPad, y, svgRGB(chartPalette.Label), svgTextEscape.Replace(row.Label)))

	left, resetKnown := panel.paceLeft()
	if panel.Unit == "" {
		left, resetKnown = float64(panel.ResetAt-panel.Now), panel.ResetAt > panel.Now
	}
	countdown := fmtRemaining(left, resetKnown)
	timeX := x + stripTimeX
	b.WriteString(fmt.Sprintf(
		`<text data-strip-reset="%s" x="%.2f" y="%.2f" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="%.1f" font-weight="700" fill="%s">%s</text>`,
		svgTextEscape.Replace(countdown), timeX, y+1, stripCountdownSize, svgRGB(chartPalette.Text), svgTextEscape.Replace(countdown)))
	// The weekday and clock of the reset follow the countdown: it says how long
	// is left, this says until when — adding "6d 23h" to a wristwatch is
	// arithmetic the row can do once. It is centred in whatever the countdown
	// leaves between itself and the speed bar, rather than hugging either, and
	// takes the largest size that still fits there (see stripStampSizes).
	barX := x + width*stripBarAt
	if clock := fmtResetClockCompact(now, now.Add(time.Duration(left)*time.Second), resetKnown); clock != "" {
		gapLeft := timeX + textWidth(countdown, stripCountdownSize) + 4
		gapRight := barX - 5
		for _, size := range stripStampSizes {
			if textWidth(clock, size) > gapRight-gapLeft && size != stripStampSizes[len(stripStampSizes)-1] {
				continue
			}
			b.WriteString(fmt.Sprintf(
				`<text data-strip-clock="%s" x="%.2f" y="%.2f" text-anchor="middle" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="%.1f" font-weight="600" fill="%s">%s</text>`,
				svgTextEscape.Replace(clock), (gapLeft+gapRight)/2, y+1, size, svgRGB(chartPalette.Dim), svgTextEscape.Replace(clock)))
			break
		}
	}

	ratio, ratioKnown := stripSpeed(panel)
	writeSpeedBar(b, panel, ratio, ratioKnown, barX, y-4)
	speedText := "—"
	color := dialGray
	if ratioKnown {
		speedText, color = speedRatioText(ratio), burnColor(ratio)
	}
	b.WriteString(fmt.Sprintf(
		`<text x="%.2f" y="%.2f" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="8.5" font-weight="700" fill="%s">%s</text>`,
		barX+speedBarW+5, y, svgRGB(color), svgTextEscape.Replace(speedText)))

	// What is left, read two ways: at the rate of the last quarter hour, and at
	// the average since the window opened. The first answers "if I keep going
	// like this", the second "if the rest of the window looks like the start".
	runway := strings.TrimSuffix(budgetRunwayText(panel.remaining(), panel.RecentRate, panel.RateKnown), " left")
	average := strings.TrimSuffix(budgetRunwayText(panel.remaining(), panel.AverageRate, panel.AverageKnown), " left")
	avgX := x + width - stripPad
	b.WriteString(fmt.Sprintf(
		`<text data-strip-avg="%s" x="%.2f" y="%.2f" text-anchor="end" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="8" font-weight="700" fill="%s">%s</text>`,
		svgTextEscape.Replace(average), avgX, y, svgRGB(averageRunwayColor), svgTextEscape.Replace(average)))
	nowX := avgX - textWidth(average, 8) - 4
	b.WriteString(fmt.Sprintf(
		`<text x="%.2f" y="%.2f" text-anchor="end" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="8" font-weight="600" fill="%s">·</text>`,
		nowX, y, svgRGB(chartPalette.Faint)))
	b.WriteString(fmt.Sprintf(
		`<text data-strip-runway="%s" x="%.2f" y="%.2f" text-anchor="end" font-family="-apple-system,Helvetica,Arial,sans-serif" font-size="8" font-weight="700" fill="%s">%s</text>`,
		svgTextEscape.Replace(runway), nowX-4, y, svgRGB(currentRunwayColor), svgTextEscape.Replace(runway)))
}

// stripSpeed is the panel's burn ratio and whether there is anything real
// behind it — an unmeasured rate or a window with no pace to measure against
// leaves the bar grey rather than parked at zero.
func stripSpeed(panel burnChartPanel) (float64, bool) {
	if !panel.RateKnown {
		return 0, false
	}
	return panelRateRatio(panel, panel.RecentRate)
}

// speedRatioText keeps the multiplier short: two significant figures below 10x,
// and no decimals once the reading is off the scale anyway.
func speedRatioText(ratio float64) string {
	if ratio >= 10 {
		return fmt.Sprintf("%.0f×", math.Min(ratio, 99))
	}
	return fmt.Sprintf("%.1f×", ratio)
}

const (
	speedBarW = 54.0
	speedBarH = 5.5
)

// writeSpeedBar is the flattened speedometer: the same 0–2x affordable-pace
// scale, with the current speed as a marker, the average since the window
// opened as a hollow ring behind it, the plain whole-window pace as an accent
// pip below, and the acceleration as a thin bar above. x,y is the track's left
// edge and vertical centre.
func writeSpeedBar(b *strings.Builder, panel burnChartPanel, ratio float64, known bool, x, y float64) {
	top := y - speedBarH/2
	b.WriteString(fmt.Sprintf(
		`<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" rx="%.2f" fill="%s"/>`,
		x, top, speedBarW, speedBarH, speedBarH/2, svgRGB(chartPalette.Track)))
	b.WriteString(fmt.Sprintf(
		`<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" rx="%.2f" fill="url(#burn-speed)" opacity="0.95"/>`,
		x, top, speedBarW, speedBarH, speedBarH/2))
	posOf := func(r float64) float64 { return x + speedBarW*clamp(r/2, 0, 1) }
	for _, tick := range speedTicks {
		b.WriteString(fmt.Sprintf(
			`<line data-speed-tick="%.1f" x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="rgb(20,20,22)" stroke-width="0.7" stroke-opacity="0.9"/>`,
			tick, posOf(tick), top, posOf(tick), top+speedBarH))
	}
	writeStripPaceMark(b, panel, posOf, top+speedBarH)
	writeStripAvgMark(b, panel, posOf, y)
	writeStripTrend(b, panel, x, top-3.4)
	if !known {
		return
	}
	mx := posOf(ratio)
	b.WriteString(fmt.Sprintf(
		`<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="rgb(20,20,22)" stroke-width="3.2" stroke-linecap="round"/>`,
		mx, top-1.4, mx, top+speedBarH+1.4))
	b.WriteString(fmt.Sprintf(
		`<line data-speed-needle="true" data-speed-ratio="%.3f" x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="%s" stroke-width="1.7" stroke-linecap="round"/>`,
		ratio, mx, top-1.4, mx, top+speedBarH+1.4, svgRGB(burnColor(ratio))))
}

// writeStripAvgMark rings where the average since the window opened sits on
// the same scale, so the needle can be read against how the window has
// actually been spent so far: a needle right of the ring means this run is
// faster than the window's own average, left of it means the window is
// recovering. It is drawn in the colour the AVG runway text uses.
func writeStripAvgMark(b *strings.Builder, panel burnChartPanel, posOf func(float64) float64, cy float64) {
	if !panel.AverageKnown || panel.AverageRate <= 0 {
		return
	}
	ratio, ok := panelRateRatio(panel, panel.AverageRate)
	if !ok {
		return
	}
	px, top := posOf(ratio), cy-speedBarH/2
	// Inside the track's own height, so it reads as a second tick rather than
	// competing with the needle, which overhangs it.
	b.WriteString(fmt.Sprintf(
		`<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="rgb(20,20,22)" stroke-width="2.4" stroke-opacity="0.85"/>`,
		px, top, px, top+speedBarH))
	b.WriteString(fmt.Sprintf(
		`<line data-avg-mark="true" data-avg-ratio="%.3f" x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="%s" stroke-width="1.1"/>`,
		ratio, px, top, px, top+speedBarH, svgRGB(averageRunwayColor)))
}

// writeStripPaceMark pips where the plain whole-window pace sits on the
// affordable scale: left of the 1x tick means the budget left allows more than
// nominal use, right of it means even nominal use is now too fast.
func writeStripPaceMark(b *strings.Builder, panel burnChartPanel, posOf func(float64) float64, barBottom float64) {
	if panel.Unit != "" {
		return // credits have no nominal whole-window pace
	}
	left, known := panel.paceLeft()
	ratio, ok := nominalPaceRatio(panel.UsedPct, left, panel.paceWindowSecs(), known)
	if !ok {
		return
	}
	px := posOf(ratio)
	b.WriteString(fmt.Sprintf(
		`<line data-pace-mark="true" data-pace-ratio="%.3f" x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="%s" stroke-width="1.3" stroke-linecap="round" stroke-opacity="0.95"/>`,
		ratio, px, barBottom+1.1, px, barBottom+3.2, svgRGB(panel.Accent)))
}

// writeStripTrend is the 5m-against-15m reading: a thin track whose centre is
// "unchanged", filled rightwards while the burn accelerates and leftwards as it
// eases off.
func writeStripTrend(b *strings.Builder, panel burnChartPanel, x, cy float64) {
	const h = 2.0
	b.WriteString(fmt.Sprintf(
		`<rect data-trend-bar="true" x="%.2f" y="%.2f" width="%.2f" height="%.2f" rx="%.2f" fill="url(#burn-trend)" opacity="0.35"/>`,
		x, cy-h/2, speedBarW, h, h/2))
	if !panel.TrendKnown {
		return
	}
	trend := clamp(panel.Trend, -1, 1)
	from, to := math.Min(0, trend), math.Max(0, trend)
	if math.Abs(trend) < 0.04 {
		from, to = -0.04, 0.04
	}
	x0 := x + speedBarW*(from+1)/2
	x1 := x + speedBarW*(to+1)/2
	b.WriteString(fmt.Sprintf(
		`<rect data-trend-value="true" data-trend="%.3f" x="%.2f" y="%.2f" width="%.2f" height="%.2f" rx="%.2f" fill="%s"/>`,
		trend, x0, cy-h/2, math.Max(0.8, x1-x0), h, h/2, svgRGB(trendColor(trend))))
}

// textWidth estimates how wide a bold string renders at a font size, so a label
// can be parked just past a number whose width isn't known until it is
// formatted. Digits in the system font are tabular; the rest is an average,
// rounded up a little so a label lands beside its number rather than on it.
func textWidth(s string, size float64) float64 {
	w := 0.0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			w += 0.60
		case r == '.' || r == ':' || r == ' ' || r == '·':
			w += 0.32
		case r == '%' || r == 'W' || r == 'M':
			w += 0.92
		default:
			w += 0.62
		}
	}
	return w * size
}
