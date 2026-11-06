package main

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/getlantern/systray"
)

// The spend sparks cannot live inside the menu-bar icon: its height is the menu
// bar's, so there is no room around the dial to fly into. They are drawn
// instead in a borderless, click-through window centred on the icon and layered
// above the menu bar (systray.ShowSparkOverlay), which lets a burst spill over
// the bar and onto the screen below it.

// overlayPad is how far, in points, a plain spark burst reaches past the icon on
// every side — i.e. how far from the dial an ember can get. A firework instead
// takes its reach from the screen (see fireworkShare).
const overlayPad = 90.0

// fallbackScreenHeight stands in when the window server won't say how tall the
// screen is, so a firework still has somewhere to go.
const fallbackScreenHeight = 900.0

// fireworkShareOptions are the menu's firework sizes, as a share of the screen
// height. The key is what gets persisted.
var fireworkShareOptions = []struct {
	key   string
	label string
	share float64
}{
	{"1/8", "1/8 of the screen", 0.125},
	{"1/4", "1/4 of the screen", 0.25},
	{"1/3", "1/3 of the screen", 1.0 / 3},
	{"1/2", "1/2 of the screen", 0.5},
}

const fireworkShareDefault = "1/4"

// normFireworkShare maps a persisted key onto a known size, falling back to the
// default for an absent or unknown one.
func normFireworkShare(key string) string {
	for _, o := range fireworkShareOptions {
		if o.key == key {
			return key
		}
	}
	return fireworkShareDefault
}

func fireworkShareValue(key string) float64 {
	for _, o := range fireworkShareOptions {
		if o.key == key {
			return o.share
		}
	}
	return fireworkShareValue(fireworkShareDefault)
}

func fireworkShareLabel(key string) string {
	for _, o := range fireworkShareOptions {
		if o.key == key {
			return o.label
		}
	}
	return fireworkShareLabel(fireworkShareDefault)
}

// overlayReach is how far past the icon, in points, this mode throws: a fixed
// margin for the sparks, the chosen slice of the screen for a firework.
func (a *App) overlayReach() float64 {
	if pulseFX != pulseFirework {
		return overlayPad
	}
	h := a.screenHeight
	if h <= 0 {
		h = fallbackScreenHeight
	}
	return math.Max(overlayPad, h*fireworkShareValue(a.fireworkSize))
}

// paintPulseOverlay draws the current frame of the burst around the icon, or
// takes the window down once both sides are quiet. Called from the frame loop.
func (a *App) paintPulseOverlay(now time.Time) {
	svg, w, h, ok := a.pulseOverlaySVG(now)
	if !ok {
		a.clearPulseOverlay()
		return
	}
	systray.ShowSparkOverlay(svg, w, h)
	a.overlayUp = true
}

// clearPulseOverlay hides the window if this process put one up.
func (a *App) clearPulseOverlay() {
	if !a.overlayUp {
		return
	}
	a.overlayUp = false
	systray.HideSparkOverlay()
}

// pulseOverlaySVG renders both sides' overlay frames onto one canvas the size
// of the icon plus overlayPad on every side, in points. ok is false when there
// is nothing to draw — no sparks wanted, no icon size known yet, or both bursts
// finished.
func (a *App) pulseOverlaySVG(now time.Time) (svg []byte, w, h float64, ok bool) {
	iconH := a.appliedIconHeight
	if !pulseFX.throwsSparks() || iconH <= 0 {
		return nil, 0, 0, false
	}
	pad := a.overlayReach()
	scale := iconH / 18 // points per dial unit
	w = dialViewWidth(2)*scale + 2*pad
	h = iconH + 2*pad
	// An ember may just reach the edge of the window; 9 is the icon's own rim.
	reach := 9 + pad/scale

	var b strings.Builder
	b.WriteString(fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.2f %.2f">`,
		w, h, w, h))
	drawn := false
	for i, side := range []struct {
		pulse spendPulse
		err   bool
		color [3]int
	}{
		{a.claudePulse, a.errorOn, a.sideMark[0]},
		{a.codexPulse, a.codexErrorOn, a.sideMark[1]},
	} {
		if side.err { // that dial is blinking an error; no spend to celebrate
			continue
		}
		f := pulseOverlayFrame(side.pulse, now, pulseFX, reach)
		if !f.Active {
			continue
		}
		cx := pad + (float64(i)*(18+dialGap)+dialCX)*scale
		cy := pad + dialCY*scale
		writeOverlayPulse(&b, cx, cy, scale, side.color, f)
		drawn = true
	}
	b.WriteString(`</svg>`)
	if !drawn {
		return nil, 0, 0, false
	}
	return []byte(b.String()), w, h, true
}

// writeOverlayPulse draws one side's shockwave and embers, converting the dial
// units the animation works in into the overlay's points. The colour is the
// dial's pace colour, used as-is: lightening it would wash the embers out
// against a bright desktop.
func writeOverlayPulse(b *strings.Builder, cx, cy, scale float64, c [3]int, f pulseFrame) {
	if f.HaloAlpha > 0.01 && f.Halo > 0 {
		b.WriteString(fmt.Sprintf(
			`<circle cx="%.2f" cy="%.2f" r="%.2f" fill="none" stroke="%s" stroke-width="%.2f" stroke-opacity="%.2f"/>`,
			cx, cy, f.Halo*scale, svgRGB(c), f.HaloWidth*scale, f.HaloAlpha))
	}
	for _, s := range f.Sparks {
		if s.Alpha <= 0.01 {
			continue
		}
		b.WriteString(fmt.Sprintf(
			`<circle cx="%.2f" cy="%.2f" r="%.2f" fill="%s" fill-opacity="%.2f"/>`,
			cx+s.X*scale, cy+s.Y*scale, s.R*scale, svgRGB(c), s.Alpha))
	}
}
