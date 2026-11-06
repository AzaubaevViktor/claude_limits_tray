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
	pulseOff      pulseMode = iota // no animation at all
	pulseGlow                      // the dot and the edge flare, inside the icon
	pulseSparks                    // ... plus a shockwave and embers, outside it
	pulseFirework                  // ... thrown across a slice of the screen
)

// throwsSparks is true for the modes that open the overlay window.
func (m pulseMode) throwsSparks() bool { return m == pulseSparks || m == pulseFirework }

// pulseFX is the user's choice, menu-toggled and persisted (see setPulseMode).
var pulseFX = pulseSparks

const (
	pulseDur       = 1.8  // seconds a spark burst lasts
	fireworkDur    = 3.4  // ... and a firework, which has much further to travel
	pulseFrameSecs = 0.04 // frame interval while a burst is running
	pulseRefDelta  = 1.5  // quota % in one step that gives a full-strength burst
	pulseMinDelta  = 0.01 // smaller steps are rounding noise, not spend
	pulseMergeSecs = 0.25 // a step this soon after another just re-arms it

	// pulseTestStagger delays the Codex side of the menu's test burst, so the
	// two dials read as two events rather than one wide flash.
	pulseTestStagger = 220 * time.Millisecond

	iconHaloR = 8.6 // the in-icon flare, just inside the 18-unit icon box
	sparkRim  = 8.4 // embers leave from the rim of the dial
	sparkMin  = 10  // embers per burst at the faintest spend
	sparkMax  = 34  // ... and at pulseRefDelta or above

	// Embers shake as they fly. The wobble has to stay well under half the
	// frame rate or it aliases into drift instead of a shake.
	wobbleAmp = 1.3 // dial units of shake at full flight
	wobbleHz  = 5.0 // base shake rate; each ember varies around it

	// haloShare is how far along the embers' flight the shockwave gets before
	// it gives up — a ring sweeping the whole way out reads as a flying saucer.
	haloShare = 0.3

	// Some embers are shells: they fly out fat, then burst — and their
	// fragments burst again — scattering from wherever each got to.
	shellShare  = 0.5  // share of the embers that are shells rather than plain
	shellFatten = 1.5  // how much bigger a shell is than a plain ember
	fallShare   = 0.18 // share of the reach the whole burst sags by at the end

	// fragmentCone keeps what a shell throws heading outward, within this many
	// degrees of the way the shell itself was going — otherwise half the
	// fragments fly back over the dial and the burst reads as a mess.
	fragmentCone = 72.0

	// sparkReachRef is the reach a plain spark burst has, in dial units; sizes
	// are scaled up from there as the throw gets longer.
	sparkReachRef = 95.0
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

// pulseLen is how long one burst lasts, in seconds: a firework has a great deal
// further to travel, so it is given longer to do it.
func pulseLen() float64 {
	if pulseFX == pulseFirework {
		return fireworkDur
	}
	return pulseDur
}

// progress is how far through the burst now is, and whether it is still playing.
func (p spendPulse) progress(now time.Time) (float64, bool) {
	if p.At.IsZero() {
		return 0, false
	}
	t := now.Sub(p.At).Seconds() / pulseLen()
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
	if !mode.throwsSparks() || reach <= 9 {
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
		Halo:      sparkRim + (reach-sparkRim)*haloShare*out,
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

// pulseStages are the generations of a firework: a shell goes off somewhere in
// the first window, each of its fragments again in the second, and what those
// throw are the last, smallest particles. splitLo/splitHi are shares of the
// time the particle has left, so a late parent still gets to burst.
var pulseStages = []struct {
	splitLo, splitHi float64
	children         int
	shrink           float64
}{
	{0.24, 0.40, 4, 0.70},
	{0.40, 0.58, 3, 0.70},
}

// particle is one lit dot in the burst — the shell itself at gen 0, then what
// it threw. They are regenerated from the seed every frame, never carried over.
type particle struct {
	id   int
	gen  int
	born float64 // burst progress when it appeared
	x, y float64 // ... and where
	size float64
}

// pulseSparkle places every particle of a burst at t: the embers thrown off the
// rim, and for the shells among them the fragments and sub-fragments their
// splits produced. Travel is budgeted so that nothing — however late it is
// born, however hard it shakes, however far the burst has sagged — leaves the
// window at reach.
func pulseSparkle(seed int64, mag, t, reach float64) []spark {
	n := sparkMin + iround(float64(sparkMax-sparkMin)*mag)
	// The further the burst is thrown, the fatter its particles have to be:
	// across half a screen a one-point dot is nothing to look at.
	fat := clamp(math.Sqrt(reach/sparkReachRef), 1, 2.5)
	out := make([]spark, 0, 4*n)
	for i := 0; i < n; i++ {
		p := particle{id: i + 1, size: (0.7 + 0.6*pulseRand(seed, i, 2)) * fat}
		if emberSplits(seed, p.id, p.gen) {
			p.size *= shellFatten
		}
		out = walkEmber(out, seed, p, mag, t, reach)
	}
	return out
}

// walkEmber draws p where it has got to, or — once its split is due — hands over
// to the children it threw from that spot.
func walkEmber(out []spark, seed int64, p particle, mag, t, reach float64) []spark {
	if split, ok := emberSplitAt(seed, p); ok && t >= split {
		sx, sy := emberAt(seed, p, split, reach)
		st := pulseStages[p.gen]
		for k := 0; k < st.children; k++ {
			out = walkEmber(out, seed, particle{
				id: p.id*37 + k + 1, gen: p.gen + 1, born: split,
				x: sx, y: sy, size: p.size * st.shrink,
			}, mag, t, reach)
		}
		return out
	}
	alpha := clamp(1.4*mag*emberFade(t, p.born), 0, 1)
	if alpha <= 0.01 {
		return out
	}
	x, y := emberAt(seed, p, t, reach)
	jx, jy := emberShake(seed, p.id, t, 1/float64(p.gen+1))
	return append(out, spark{X: x + jx, Y: y + jy + emberFall(t, reach), R: p.size, Alpha: alpha})
}

// emberAt is where p has drifted to by at: outward from where it was born, in
// its own direction, at its own speed, into whatever room is left between it
// and the edge of the window.
func emberAt(seed int64, p particle, at, reach float64) (x, y float64) {
	ph := emberHeading(seed, p)
	speed := 0.5 + 0.5*pulseRand(seed, p.id, 1)
	if p.gen == 0 { // the first generation starts on the rim, not at the centre
		p.x, p.y = sparkRim*math.Cos(ph), sparkRim*math.Sin(ph)
	}
	age := clamp((at-p.born)/math.Max(1-p.born, 1e-6), 0, 1)
	room := math.Max(0, reach*(1-fallShare)-wobbleAmp-math.Hypot(p.x, p.y))
	d := room * pulseEase(age) * speed
	return p.x + d*math.Cos(ph), p.y + d*math.Sin(ph)
}

// emberHeading is the direction a particle travels: anywhere at all for an
// ember leaving the rim, but within a cone of its parent's outward direction
// for a fragment, so every generation keeps moving away from the icon.
func emberHeading(seed int64, p particle) float64 {
	r := pulseRand(seed, p.id, 0)
	if p.gen == 0 {
		return r * 2 * math.Pi
	}
	return math.Atan2(p.y, p.x) + (2*r-1)*fragmentCone*math.Pi/180
}

// emberSplits reports whether a particle bursts at all: only some of the
// first-generation embers are shells, but everything a shell throws goes on to
// burst in its turn, until the stages run out.
func emberSplits(seed int64, id, gen int) bool {
	if gen >= len(pulseStages) {
		return false
	}
	return gen > 0 || pulseRand(seed, id, 6) < shellShare
}

// emberSplitAt is the burst progress at which p goes off, if it ever does.
func emberSplitAt(seed int64, p particle) (float64, bool) {
	if !emberSplits(seed, p.id, p.gen) {
		return 0, false
	}
	st := pulseStages[p.gen]
	life := 1 - p.born
	return p.born + life*(st.splitLo+(st.splitHi-st.splitLo)*pulseRand(seed, p.id, 7)), true
}

// emberFade lights a particle as it appears and lets it linger, going out only
// near the end of the burst.
func emberFade(t, born float64) float64 {
	return clamp((t-born)/0.06, 0, 1) * math.Pow(clamp(1-t, 0, 1), 0.8)
}

// emberFall is how far the whole burst has sagged by t — fireworks drop.
func emberFall(t, reach float64) float64 { return fallShare * reach * t * t }

// emberShake is particle i's wobble at t: two detuned oscillations that build up
// as it gets clear of the rim, so nothing vibrates while still sitting on the
// dial. scale trims the amplitude (fragments shake less than whole embers).
func emberShake(seed int64, i int, t, scale float64) (x, y float64) {
	amp := scale * wobbleAmp * (0.25 + 0.75*pulseEase(t))
	hz := wobbleHz * (0.7 + 0.6*pulseRand(seed, i, 3))
	phx := pulseRand(seed, i, 4) * 2 * math.Pi
	phy := pulseRand(seed, i, 5) * 2 * math.Pi
	turns := 2 * math.Pi * hz * t * pulseLen()
	// Split over the two axes so the shake vector itself never exceeds amp.
	amp /= math.Sqrt2
	return amp * math.Sin(turns+phx), amp * math.Sin(0.83*turns+phy)
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
	{pulseFirework, "Firework (across the screen)"},
}

var pulseModeNames = map[pulseMode]string{
	pulseOff:      "off",
	pulseGlow:     "pulse",
	pulseSparks:   "pulse + sparks",
	pulseFirework: "firework",
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
