package main

import (
	"fmt"
	"math"
	"strings"
)

// Limit is the per-window input to the dial.
type Limit struct {
	Pct      float64 // used 0..100
	TimeLeft float64 // fraction of window remaining 0..1
}

type segment struct {
	ring, side, source, metric string
	bright                     bool
	label                      string
}

// Dial geometry (in an 18x18 coordinate space; rendered scaled up for retina).
const (
	dialCX, dialCY = 9.0, 9.0
	dialSW         = 1.7
	halfShift      = 0.9
	endGap         = 5.0
	pxSize         = 44 // rendered pixel size; scaled to the menu bar by systray
)

var rings = map[string]float64{"outer": 7.1, "inner": 4.3}
var sideSign = map[string]float64{"left": -1, "right": +1}
var ringLabel = map[string]string{"outer": "Outer ring", "inner": "Inner ring"}
var sideLabel = map[string]string{"left": "left", "right": "right"}

var dialGreen = [3]int{48, 209, 88}
var dialYellow = [3]int{255, 204, 0}
var dialRed = [3]int{255, 69, 58}
var dialTrack = [3]int{150, 150, 150}

// timeFill selects how the inner (time) ring fills. The colour is the same
// either way — greener the less time is left in the window — only the arc
// length differs. User-toggled from the menu and persisted.
type fillMode int

const (
	fillElapsed   fillMode = iota // arc grows as the window elapses (default)
	fillRemaining                 // arc shrinks as the window elapses
)

var timeFill = fillElapsed

func timeFillString() string {
	if timeFill == fillRemaining {
		return "remaining"
	}
	return "elapsed"
}

func setTimeFillFromString(s string) {
	switch s {
	case "remaining":
		timeFill = fillRemaining
	case "elapsed":
		timeFill = fillElapsed
	}
}

// SEGMENTS: edit this table; the icon and the legend both follow.
//
//	metric         arc length            colour (green plenty -> red low)
//	percent_left   % of quota remaining  same  (long green arc = lots left)
//	time           time within window    GREENS toward reset (the less time
//	               left, the greener); the arc fills with elapsed or remaining
//	               time per the timeFill toggle, but the colour is unchanged
var segments = []segment{
	{"outer", "left", "session", "percent_left", true, "Session left"},
	{"inner", "left", "session", "time", false, "Session time"},
	{"outer", "right", "weekly", "percent_left", true, "Weekly left"},
	{"inner", "right", "weekly", "time", false, "Weekly time"},
}

func legendRows() [][2]string {
	rows := make([][2]string, 0, len(segments)+1)
	for _, s := range segments {
		label := s.label
		if s.metric == "time" {
			label = timeSegLabel(s.source)
		}
		rows = append(rows, [2]string{ringLabel[s.ring] + " · " + sideLabel[s.side], label})
	}
	rows = append(rows, [2]string{"Colour", "green plenty → red low"})
	return rows
}

func timeSegLabel(source string) string {
	name := map[string]string{"session": "Session", "weekly": "Weekly"}[source]
	if name == "" {
		name = source
	}
	if timeFill == fillRemaining {
		return name + " time left (fills with time left)"
	}
	return name + " since reset (fills with elapsed)"
}

// metric returns (arc length, health) in 0..1.
func metricFor(name string, l Limit) (float64, float64) {
	pctLeft := clamp(1-l.Pct/100, 0, 1)
	tLeft := clamp(l.TimeLeft, 0, 1)
	switch name {
	case "percent_left":
		return pctLeft, pctLeft
	case "percent_used":
		return 1 - pctLeft, pctLeft
	case "time_left":
		return tLeft, tLeft
	case "time_elapsed":
		return 1 - tLeft, 1 - tLeft
	case "time":
		// Colour always greens as the window drains (redder with more time
		// left); only the arc length follows the fill toggle.
		health := 1 - tLeft
		if timeFill == fillRemaining {
			return tLeft, health
		}
		return 1 - tLeft, health
	}
	return 0, 0
}

func healthColor(health float64) [3]int {
	health = clamp(health, 0, 1)
	var a, b [3]int
	var t float64
	if health >= 0.5 {
		a, b, t = dialYellow, dialGreen, (health-0.5)/0.5
	} else {
		a, b, t = dialRed, dialYellow, health/0.5
	}
	var out [3]int
	for i := 0; i < 3; i++ {
		out[i] = int(math.Round(float64(a[i]) + (float64(b[i])-float64(a[i]))*t))
	}
	return out
}

// phi converts a "clockwise from the top" angle (degrees) to SVG's angle (rad).
func phi(deg float64) float64 { return deg*math.Pi/180 - math.Pi/2 }

// renderDial draws the dial and returns SVG bytes. markColor is the centre
// spark colour; pass hasMark=false to omit it (error blink "off" frame).
func renderDial(limits map[string]Limit, markColor [3]int, hasMark bool) []byte {
	return renderDialSVG([]dialRenderInput{{limits: limits, markColor: markColor, hasMark: hasMark}})
}

func renderDualDial(claudeLimits map[string]Limit, claudeMark [3]int, claudeHasMark bool,
	codexLimits map[string]Limit, codexMark [3]int, codexHasMark bool) []byte {
	return renderDialSVG([]dialRenderInput{
		{limits: claudeLimits, markColor: claudeMark, hasMark: claudeHasMark},
		{limits: codexLimits, markColor: codexMark, hasMark: codexHasMark},
	})
}

type dialRenderInput struct {
	limits    map[string]Limit
	markColor [3]int
	hasMark   bool
}

func renderDialSVG(inputs []dialRenderInput) []byte {
	if len(inputs) == 0 {
		inputs = []dialRenderInput{{limits: blankLimits}}
	}
	const gap = 2.0
	width := 18.0*float64(len(inputs)) + gap*float64(len(inputs)-1)
	var b strings.Builder
	b.WriteString(fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %.2f 18">`,
		pxSize*len(inputs), pxSize, width))
	for i, input := range inputs {
		renderDialGroup(&b, input, float64(i)*(18+gap))
	}
	b.WriteString(`</svg>`)
	return []byte(b.String())
}

func renderDialGroup(b *strings.Builder, input dialRenderInput, xoff float64) {
	span := 180.0 - 2*endGap

	// faint tracks, one per ring/side, so the centre gap stays empty
	for _, sign := range sideSign {
		ox := xoff + dialCX + sign*halfShift
		for _, r := range rings {
			writeArc(b, ox, r, sign*endGap, sign*(180-endGap), dialTrack, 0.45, dialSW)
		}
	}

	for _, seg := range segments {
		length, health := metricFor(seg.metric, input.limits[seg.source])
		length = clamp(length, 0, 1)
		if length <= 0.004 {
			continue
		}
		sign := sideSign[seg.side]
		r := rings[seg.ring]
		ox := dialCX + sign*halfShift
		alpha := 0.85
		if seg.bright {
			alpha = 1.0
		}
		writeArc(b, xoff+ox, r, sign*endGap, sign*(endGap+length*span),
			healthColor(health), alpha, dialSW)
	}

	if input.hasMark {
		writeMark(b, xoff, input.markColor)
	}
}

func writeArc(b *strings.Builder, ox, r, startDeg, endDeg float64, c [3]int, alpha, width float64) {
	sx, sy := arcPoint(ox, r, startDeg)
	ex, ey := arcPoint(ox, r, endDeg)
	large := 0
	if math.Abs(endDeg-startDeg) > 180 {
		large = 1
	}
	sweep := 0
	if endDeg > startDeg {
		sweep = 1
	}
	b.WriteString(fmt.Sprintf(
		`<path d="M%.2f,%.2f A%.2f,%.2f 0 %d %d %.2f,%.2f" fill="none" stroke="%s" stroke-width="%.2f" stroke-opacity="%.2f" stroke-linecap="round"/>`,
		sx, sy, r, r, large, sweep, ex, ey, svgRGB(c), width, alpha))
}

func arcPoint(ox, r, deg float64) (float64, float64) {
	p := phi(deg)
	return ox + r*math.Cos(p), dialCY + r*math.Sin(p)
}

func svgRGB(c [3]int) string {
	return fmt.Sprintf("rgb(%d,%d,%d)", c[0], c[1], c[2])
}

func writeMark(b *strings.Builder, xoff float64, c [3]int) {
	const rays = 12
	r0, r1 := 0.35, 2.15
	for k := 0; k < rays; k++ {
		ang := float64(k) * 2 * math.Pi / rays
		x0 := xoff + dialCX + r0*math.Cos(ang)
		y0 := dialCY + r0*math.Sin(ang)
		x1 := xoff + dialCX + r1*math.Cos(ang)
		y1 := dialCY + r1*math.Sin(ang)
		b.WriteString(fmt.Sprintf(
			`<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="%s" stroke-width="0.82" stroke-linecap="round"/>`,
			x0, y0, x1, y1, svgRGB(c)))
	}
}

var blankLimits = map[string]Limit{
	"session": {100, 1},
	"weekly":  {100, 1},
}
