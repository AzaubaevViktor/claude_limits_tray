package main

import (
	"math"
	"time"
)

// Spend pulse: whenever a provider's quota % ticks up, its dial plays a short
// burst. Inside the menu-bar icon the centre dot flashes and swells and a thin
// ring flares at the edge of the icon box; with sparks enabled the rest of the
// burst happens in a window drawn around the icon (see overlay.go) — a
// shockwave rolling outward past the menu bar and the embers it throws, which
// is the only way anything can leave the icon's own rectangle. Everything here
// is pure geometry on a 0..1 progress, so the renderers and the frame loop
// (app.go) stay dumb.

type pulseMode int

const (
	pulseOff    pulseMode = iota // no animation at all
	pulseGlow                    // the dot and the edge flare, inside the icon
	pulseSparks                  // ... plus the shockwave and embers, outside it
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

	iconHaloR = 8.6 // the in-icon flare, just inside the 18-unit icon box
	sparkRim  = 8.4 // embers leave from the rim of the dial
	sparkMin  = 4   // embers per burst at the faintest spend
	sparkMax  = 11  // ... and at pulseRefDelta or above
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

// spark is one ember, positioned relative to the dial centre in dial units.
type spark struct{ X, Y, R, Alpha float64 }

// pulseFrame is one rendered moment of a burst, in dial units.
type pulseFrame struct {
	Active    bool
	MarkScale float64 // multiplier on the centre dot radius
	MarkFlash float64 // 0..1 blend of the dot toward white
	Halo      float64 // ring radius; 0 = no ring
	HaloWidth float64
	HaloAlpha float64
	Sparks    []spark
}

// pulseIconFrame is the part of the burst that stays inside the menu-bar icon:
// the centre dot and a thin ring flaring at the edge of the box. Both pulse
// modes get it, so the dial still reacts when the overlay is switched off.
func pulseIconFrame(p spendPulse, now time.Time, mode pulseMode) pulseFrame {
	if mode == pulseOff {
		return pulseFrame{}
	}
	t, ok := p.progress(now)
	if !ok {
		return pulseFrame{}
	}
	mag := clamp(p.Mag, 0, 1)
	bump, out := pulseBump(t), pulseEase(t)
	return pulseFrame{
		Active:    true,
		MarkScale: 1 + 0.3*mag*bump,
		MarkFlash: 0.55 * bump,
		Halo:      iconHaloR,
		HaloWidth: 0.2 + 0.55*(1-out),
		HaloAlpha: 0.7 * mag * (1 - t) * (1 - t),
	}
}

// pulseOverlayFrame is the part that leaves the icon: a shockwave ring rolling
// out from the rim to reach (in dial units, where 9 is the icon's own edge) and
// the embers chasing it. Nothing to draw unless the sparks are on.
func pulseOverlayFrame(p spendPulse, now time.Time, mode pulseMode, reach float64) pulseFrame {
	if mode != pulseSparks || reach <= 9 {
		return pulseFrame{}
	}
	t, ok := p.progress(now)
	if !ok {
		return pulseFrame{}
	}
	mag := clamp(p.Mag, 0, 1)
	out := pulseEase(t)
	return pulseFrame{
		Active:    true,
		Halo:      sparkRim + (reach-sparkRim)*out,
		HaloWidth: 0.35 + 1.2*(1-out),
		HaloAlpha: 0.65 * mag * (1 - t) * (1 - t),
		Sparks:    pulseSparkle(p.Seed, mag, t, reach),
	}
}

// pulseBump is the dot's envelope: a fast attack over the first tenth of the
// burst, then a smooth decay to nothing.
func pulseBump(t float64) float64 {
	if t < 0.1 {
		return t / 0.1
	}
	return (1 - t) / 0.9
}

// pulseEase decelerates outward travel, so the shockwave and the embers leap off
// the rim and coast to a stop.
func pulseEase(t float64) float64 { return 1 - (1-t)*(1-t) }

// pulseSparkle places the embers for a burst: thrown in every direction off the
// rim, each at its own speed, none travelling past reach.
func pulseSparkle(seed int64, mag, t, reach float64) []spark {
	n := sparkMin + iround(float64(sparkMax-sparkMin)*mag)
	fade := clamp(t/0.12, 0, 1) * math.Pow(1-t, 1.5)
	out := make([]spark, 0, n)
	for i := 0; i < n; i++ {
		ph := pulseRand(seed, i, 0) * 2 * math.Pi
		speed := 0.55 + 0.45*pulseRand(seed, i, 1)
		// Embers must read against the desktop as well as the menu bar, so they
		// are chunky — anything smaller turns to dust at menu-bar scale.
		size := 0.45 + 0.4*pulseRand(seed, i, 2)
		d := sparkRim + (reach-sparkRim)*pulseEase(t)*speed
		out = append(out, spark{
			X: d * math.Cos(ph), Y: d * math.Sin(ph), R: size,
			Alpha: clamp(1.3*mag*fade, 0, 1),
		})
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

// pulseModeOrder is the menu order, with the row label for each mode.
var pulseModeOrder = []struct {
	mode  pulseMode
	label string
}{
	{pulseOff, "Off"},
	{pulseGlow, "Pulse the dial"},
	{pulseSparks, "Pulse + sparks (outside the icon)"},
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
