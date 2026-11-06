package main

import (
	"math"
	"time"
)

// Spend pulse: whenever a provider's quota % ticks up, its dial plays a short
// burst — the centre dot swells and flashes, a soft halo rolls out past the
// quota ring and, with sparks enabled, a handful of embers fly off the rim into
// the padding the icon reserves on each side of the dial. Everything here is
// pure geometry on a 0..1 progress, so the renderer (dial.go) and the frame
// loop (app.go) stay dumb.

type pulseMode int

const (
	pulseOff    pulseMode = iota // no animation at all
	pulseGlow                    // dot + halo, icon keeps its width
	pulseSparks                  // ... plus embers, which need the side padding
)

// pulseFX is the user's choice, menu-toggled and persisted (see setPulseMode).
var pulseFX = pulseSparks

const (
	pulseDur       = 1.1  // seconds one burst lasts
	pulseFrameSecs = 0.07 // frame interval while a burst is running
	pulseRefDelta  = 1.5  // quota % in one step that gives a full-strength burst
	pulseMinDelta  = 0.01 // smaller steps are rounding noise, not spend
	pulseMergeSecs = 0.25 // a step this soon after another just re-arms it

	// pulseTestStagger delays the Codex side of the menu's test burst, so the
	// two dials read as two events rather than one wide flash.
	pulseTestStagger = 220 * time.Millisecond

	// sparkPad is the canvas the embers need beyond the 18-unit dial box, on
	// each side. Only width is padded: the menu bar fixes the icon height, so
	// vertical room would have to come out of the dial itself.
	sparkPad   = 2.6
	sparkRim   = 8.4 // embers start just outside the credits ring
	sparkYRoom = 8.5 // ... and must stay inside the unpadded 18-unit height
	sparkFan   = 58.0
	haloRim    = 8.9  // the shockwave leaves the dial just outside the credits ring
	haloFan    = 42.0 // ... as two crescents this wide around the horizontal axis
	sparkMin   = 3    // embers per burst at the faintest spend
	sparkMax   = 8    // ... and at pulseRefDelta or above
)

// spendPulse is one in-flight burst. The zero value is "nothing playing".
type spendPulse struct {
	At   time.Time
	Mag  float64 // 0..1 strength, from the % spent in the step that fired it
	Seed int64
}

// arm starts (or re-arms) a burst for a step of delta quota %. Steps landing
// within pulseMergeSecs of each other are one burst at the strongest delta, so
// two windows ticking up on the same poll don't double-fire.
func (p spendPulse) arm(now time.Time, delta float64) spendPulse {
	mag := clamp(delta/pulseRefDelta, 0.15, 1)
	if !p.At.IsZero() && now.Sub(p.At).Seconds() < pulseMergeSecs {
		return spendPulse{At: p.At, Mag: math.Max(p.Mag, mag), Seed: p.Seed}
	}
	return spendPulse{At: now, Mag: mag, Seed: now.UnixNano()}
}

// progress is how far through the burst now is, and whether it is still playing.
func (p spendPulse) progress(now time.Time) (float64, bool) {
	if p.At.IsZero() {
		return 0, false
	}
	t := now.Sub(p.At).Seconds() / pulseDur
	if t < 0 || t >= 1 {
		return 0, false
	}
	return t, true
}

// spark is one ember, positioned relative to the dial centre.
type spark struct{ X, Y, R, Alpha float64 }

// pulseFrame is one rendered moment of a burst.
type pulseFrame struct {
	Active    bool
	MarkScale float64 // multiplier on the centre dot radius
	MarkFlash float64 // 0..1 blend of the dot toward white
	Halo      float64 // shockwave radius in dial units; 0 = no shockwave
	HaloWidth float64
	HaloAlpha float64
	HaloFan   float64 // half-span in degrees around the horizontal axis; 0 = full ring
	Sparks    []spark
}

// pulseFrameAt renders p at now under mode. An inactive burst (or pulseOff)
// gives the zero frame, which draws nothing.
func pulseFrameAt(p spendPulse, now time.Time, mode pulseMode) pulseFrame {
	if mode == pulseOff {
		return pulseFrame{}
	}
	t, ok := p.progress(now)
	if !ok {
		return pulseFrame{}
	}
	mag := clamp(p.Mag, 0, 1)
	bump := pulseBump(t)
	out := pulseEase(t)
	f := pulseFrame{
		Active:    true,
		MarkScale: 1 + 0.3*mag*bump,
		MarkFlash: 0.55 * bump,
		HaloAlpha: 0.7 * mag * (1 - t) * (1 - t),
	}
	if mode == pulseSparks {
		// Room outside the dial: the shockwave rides out past the rim as two
		// crescents on the sides, where the padding is, and the embers follow.
		f.Halo = haloRim + (9+sparkPad*0.9-haloRim)*out
		f.HaloWidth = 0.35 + 1.0*(1-out)
		f.HaloFan = haloFan
		f.Sparks = pulseSparkle(p.Seed, mag, t)
		return f
	}
	// Without the padding there is nowhere to expand to, so the shockwave is a
	// thin full ring at the edge of the icon box that simply flares and fades.
	f.Halo = haloRim - 0.3
	f.HaloWidth = 0.2 + 0.5*(1-out)
	return f
}

// pulseBump is the dot's envelope: a fast attack over the first tenth of the
// burst, then a smooth decay to nothing.
func pulseBump(t float64) float64 {
	if t < 0.1 {
		return t / 0.1
	}
	return (1 - t) / 0.9
}

// pulseEase decelerates outward travel, so the halo and the embers leap off the
// rim and coast to a stop.
func pulseEase(t float64) float64 { return 1 - (1-t)*(1-t) }

// pulseSparkle places the embers for a burst. They are thrown into a fan around
// the horizontal axis (left and right alternately) because that is where the
// icon has room; each one's reach is capped so it cannot cross the top or bottom
// edge of the unpadded dial box, which naturally makes the steeper ones short.
func pulseSparkle(seed int64, mag, t float64) []spark {
	n := sparkMin + iround(float64(sparkMax-sparkMin)*mag)
	reach := 9 + sparkPad*0.9
	fade := clamp(t/0.12, 0, 1) * math.Pow(1-t, 1.5)
	out := make([]spark, 0, n)
	for i := 0; i < n; i++ {
		side := 1.0
		if i%2 == 1 {
			side = -1
		}
		deg := (pulseRand(seed, i, 0)*2 - 1) * sparkFan
		speed := 0.65 + 0.35*pulseRand(seed, i, 1)
		// Embers must read at menu-bar size (the whole dial is ~19 pt), so they
		// are chunky in dial units — anything smaller turns to sub-pixel dust.
		size := 0.45 + 0.35*pulseRand(seed, i, 2)
		ph := deg * math.Pi / 180
		cos, sin := math.Cos(ph)*side, math.Sin(ph)
		far := reach
		if y := math.Abs(sin); y > 1e-3 {
			far = math.Min(far, (sparkYRoom-size)/y)
		}
		if far <= sparkRim+0.3 { // too steep to have anywhere to go
			continue
		}
		d := sparkRim + (far-sparkRim)*pulseEase(t)*speed
		out = append(out, spark{X: d * cos, Y: d * sin, R: size, Alpha: clamp(1.3*mag*fade, 0, 1)})
	}
	return out
}

// pulseRand is a deterministic 0..1 draw for ember i's k-th attribute, so a
// burst looks the same on every frame without carrying per-ember state.
func pulseRand(seed int64, i, k int) float64 {
	x := uint64(seed)*0x9E3779B97F4A7C15 + uint64(i+1)*0xBF58476D1CE4E5B9 + uint64(k+1)*0x94D049BB133111EB
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return float64(x>>11) / float64(uint64(1)<<53)
}

// pulseCanvasPad is the extra width each dial gets on both sides, so the embers
// have somewhere to fly. It depends on the mode alone, never on whether a burst
// is playing — the icon must not change width mid-animation.
func pulseCanvasPad() float64 {
	if pulseFX == pulseSparks {
		return sparkPad
	}
	return 0
}

// pulseModeOrder is the menu order, with the row label for each mode.
var pulseModeOrder = []struct {
	mode  pulseMode
	label string
}{
	{pulseOff, "Off"},
	{pulseGlow, "Pulse the dial"},
	{pulseSparks, "Pulse + sparks (wider icon)"},
}

var pulseModeNames = map[pulseMode]string{
	pulseOff:    "off",
	pulseGlow:   "pulse",
	pulseSparks: "pulse + sparks",
}

func pulseModeString() string { return pulseModeNames[pulseFX] }

func setPulseModeFromString(s string) {
	for m, name := range pulseModeNames {
		if name == s {
			pulseFX = m
			return
		}
	}
}
