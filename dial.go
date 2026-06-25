package main

import (
	"bytes"
	"image/png"
	"math"

	"github.com/fogleman/gg"
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

// SEGMENTS: edit this table; the icon and the legend both follow.
//
//	metric         arc length            colour (green plenty -> red low)
//	percent_left   % of quota remaining  same  (long green arc = lots left)
//	time_elapsed   time since last reset elapsed (fills + GREENS toward reset:
//	               the less time left in the window, the greener)
var segments = []segment{
	{"outer", "left", "session", "percent_left", true, "Session left"},
	{"inner", "left", "session", "time_elapsed", false, "Session since reset"},
	{"outer", "right", "weekly", "percent_left", true, "Weekly left"},
	{"inner", "right", "weekly", "time_elapsed", false, "Weekly since reset"},
}

func legendRows() [][2]string {
	rows := make([][2]string, 0, len(segments)+1)
	for _, s := range segments {
		rows = append(rows, [2]string{ringLabel[s.ring] + " · " + sideLabel[s.side], s.label})
	}
	rows = append(rows, [2]string{"Colour", "green plenty → red low"})
	return rows
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

// phi converts a "clockwise from the top" angle (degrees) to gg's angle (rad).
func phi(deg float64) float64 { return deg*math.Pi/180 - math.Pi/2 }

func setRGB(dc *gg.Context, c [3]int, alpha float64) {
	dc.SetRGBA(float64(c[0])/255, float64(c[1])/255, float64(c[2])/255, alpha)
}

func strokeArc(dc *gg.Context, ox, r, startDeg, endDeg float64) {
	dc.DrawArc(ox, dialCY, r, phi(startDeg), phi(endDeg))
	dc.Stroke()
}

// renderDial draws the dial and returns PNG bytes. markColor is the centre
// spark colour; pass hasMark=false to omit it (error blink "off" frame).
func renderDial(limits map[string]Limit, markColor [3]int, hasMark bool) []byte {
	return renderDialPx(limits, markColor, hasMark, pxSize)
}

func renderDialPx(limits map[string]Limit, markColor [3]int, hasMark bool, px int) []byte {
	scale := float64(px) / 18.0
	dc := gg.NewContext(px, px)
	dc.Scale(scale, scale)
	// gg applies line width in device pixels (it ignores the Scale transform),
	// so scale it ourselves to match the SVG's viewBox-relative stroke.
	dc.SetLineWidth(dialSW * scale)
	dc.SetLineCap(gg.LineCapRound)
	span := 180.0 - 2*endGap

	// faint tracks, one per ring/side, so the centre gap stays empty
	for _, sign := range sideSign {
		ox := dialCX + sign*halfShift
		for _, r := range rings {
			setRGB(dc, dialTrack, 0.45)
			strokeArc(dc, ox, r, sign*endGap, sign*(180-endGap))
		}
	}

	for _, seg := range segments {
		length, health := metricFor(seg.metric, limits[seg.source])
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
		setRGB(dc, healthColor(health), alpha)
		strokeArc(dc, ox, r, sign*endGap, sign*(endGap+length*span))
	}

	if hasMark {
		drawMark(dc, markColor, scale)
	}

	var buf bytes.Buffer
	_ = png.Encode(&buf, dc.Image())
	return buf.Bytes()
}

func drawMark(dc *gg.Context, c [3]int, scale float64) {
	const rays = 12
	r0, r1 := 0.35, 2.15
	setRGB(dc, c, 1.0)
	dc.SetLineWidth(0.82 * scale)
	for k := 0; k < rays; k++ {
		ang := float64(k) * 2 * math.Pi / rays
		dc.DrawLine(dialCX+r0*math.Cos(ang), dialCY+r0*math.Sin(ang),
			dialCX+r1*math.Cos(ang), dialCY+r1*math.Sin(ang))
		dc.Stroke()
	}
	dc.SetLineWidth(dialSW * scale)
}

var blankLimits = map[string]Limit{
	"session": {100, 1},
	"weekly":  {100, 1},
}
