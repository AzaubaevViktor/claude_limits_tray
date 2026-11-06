package main

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestSpendPulseRunsForItsDurationOnly(t *testing.T) {
	now := time.Now()
	p := spendPulse{}.arm(now, pulseRefDelta)
	if p.Mag != 1 {
		t.Errorf("a reference-size step should be a full-strength burst, got %.2f", p.Mag)
	}
	for _, at := range []float64{0, 0.3, pulseDur - 0.01} {
		if _, ok := p.progress(now.Add(time.Duration(at * float64(time.Second)))); !ok {
			t.Errorf("burst should still be playing at %.2fs", at)
		}
	}
	if _, ok := p.progress(now.Add(time.Duration(pulseDur * float64(time.Second)))); ok {
		t.Error("burst should be over once its duration has elapsed")
	}
	if _, ok := (spendPulse{}).progress(now); ok {
		t.Error("the zero pulse should never play")
	}
}

// Two windows ticking up on the same poll are one spend, not two: the second
// step re-arms the running burst (keeping its start) at the stronger magnitude.
func TestSpendPulseMergesStepsFromTheSamePoll(t *testing.T) {
	now := time.Now()
	first := spendPulse{}.arm(now, 0.2)
	merged := first.arm(now.Add(50*time.Millisecond), 1.5)
	if !merged.At.Equal(first.At) || merged.Seed != first.Seed {
		t.Error("a step within the merge window should keep the running burst")
	}
	if merged.Mag <= first.Mag {
		t.Errorf("the merged burst should take the stronger magnitude, got %.2f", merged.Mag)
	}
	later := merged.arm(now.Add(500*time.Millisecond), 0.2)
	if later.At.Equal(merged.At) {
		t.Error("a step after the merge window should start a fresh burst")
	}
}

func TestPulseFramesFadeOutAndRespectTheMode(t *testing.T) {
	now := time.Now()
	p := spendPulse{}.arm(now, pulseRefDelta)
	const reach = 20.0
	icon := func(mode pulseMode, secs float64) pulseFrame {
		return pulseIconFrame(p, now.Add(time.Duration(secs*float64(time.Second))), mode)
	}
	over := func(mode pulseMode, secs float64) pulseFrame {
		return pulseOverlayFrame(p, now.Add(time.Duration(secs*float64(time.Second))), mode, reach)
	}
	if icon(pulseOff, 0.2).Active || over(pulseOff, 0.2).Active {
		t.Error("pulseOff should draw nothing")
	}
	if !icon(pulseGlow, 0.2).Active {
		t.Error("pulseGlow should still flash the dial itself")
	}
	if over(pulseGlow, 0.2).Active {
		t.Error("pulseGlow should not open the overlay")
	}
	early, late := icon(pulseSparks, 0.15), icon(pulseSparks, 0.9)
	if late.MarkScale >= early.MarkScale || early.MarkScale <= 1 {
		t.Errorf("the centre dot should swell then settle back: %.2f then %.2f",
			early.MarkScale, late.MarkScale)
	}
	if late.HaloAlpha >= early.HaloAlpha {
		t.Error("the in-icon flare should fade")
	}
	oEarly, oLate := over(pulseSparks, 0.15), over(pulseSparks, 0.9)
	if len(oEarly.Sparks) == 0 {
		t.Fatal("a full-strength burst should throw sparks")
	}
	if oLate.Halo <= oEarly.Halo || oLate.HaloAlpha >= oEarly.HaloAlpha {
		t.Errorf("the shockwave should grow and fade: %+v then %+v", oEarly, oLate)
	}
	if icon(pulseSparks, pulseDur+0.01).Active || over(pulseSparks, pulseDur+0.01).Active {
		t.Error("the frame after the burst should draw nothing")
	}
}

// The whole point of the overlay is that the burst leaves the icon: embers have
// to get past its rim (9 dial units) without overshooting the window.
func TestSparksFlyOutPastTheIcon(t *testing.T) {
	now := time.Now()
	p := spendPulse{}.arm(now, pulseRefDelta)
	const reach = 25.0
	var far, prevNear float64
	for _, secs := range []float64{0.1, 0.4, 0.9} {
		f := pulseOverlayFrame(p, now.Add(time.Duration(secs*float64(time.Second))), pulseSparks, reach)
		near := math.MaxFloat64
		for _, s := range f.Sparks {
			d := math.Hypot(s.X, s.Y)
			if d < sparkRim-0.01 {
				t.Errorf("spark at %.2f started inside the rim", d)
			}
			if d > reach+0.01 {
				t.Errorf("spark at %.2f flew past the overlay window", d)
			}
			near = math.Min(near, d)
			far = math.Max(far, d)
		}
		if prevNear > 0 && near < prevNear {
			t.Error("sparks should only travel outward")
		}
		prevNear = near
	}
	if far <= 9 {
		t.Errorf("sparks should leave the icon, got %.2f", far)
	}
}

// A burst must look identical on every frame rendered from the same seed, and
// differ between bursts — the ember layout carries no state of its own.
func TestSparkLayoutIsStablePerBurst(t *testing.T) {
	a := pulseSparkle(42, 1, 0.5, 20)
	if got := pulseSparkle(42, 1, 0.5, 20); len(got) != len(a) || got[0] != a[0] {
		t.Error("the same seed and progress should give the same sparks")
	}
	if got := pulseSparkle(43, 1, 0.5, 20); len(got) == len(a) && got[0] == a[0] {
		t.Error("a different seed should give a different burst")
	}
	if weak := pulseSparkle(42, 0.1, 0.5, 20); len(weak) >= len(a) {
		t.Errorf("a small spend should throw fewer sparks: %d vs %d", len(weak), len(a))
	}
}

func TestPulseModeRoundTripsThroughState(t *testing.T) {
	defer func(m pulseMode) { pulseFX = m }(pulseFX)
	for _, m := range []pulseMode{pulseOff, pulseGlow, pulseSparks} {
		pulseFX = m
		s := pulseModeString()
		pulseFX = pulseOff
		setPulseModeFromString(s)
		if pulseFX != m {
			t.Errorf("%q did not round-trip", s)
		}
	}
	pulseFX = pulseGlow
	setPulseModeFromString("nonsense")
	if pulseFX != pulseGlow {
		t.Error("an unknown setting should leave the mode alone")
	}
}

// Only the quota counters pulse: a credit top-up or spend is not quota burn.
func TestOnlyQuotaSeriesPulse(t *testing.T) {
	for _, tc := range []struct{ series, side string }{
		{seriesClaudeSession, "claude"},
		{seriesClaudeWeekly, "claude"},
		{seriesCodexPrimary, "codex"},
		{seriesCodexSecondary, "codex"},
		{seriesClaudeCredits, ""},
		{seriesCodexCredits, ""},
	} {
		if got := pulseSideFor(tc.series); got != tc.side {
			t.Errorf("%s -> %q, want %q", tc.series, got, tc.side)
		}
	}
}

// The first reading in a process only seeds the comparison (a restart must not
// flash for spend from the previous run), a step up fires, and a reset — where
// the counter drops — does not.
func TestQuotaStepsFireThePulse(t *testing.T) {
	a := newApp()
	a.notePctStep(seriesClaudeWeekly, 12)
	if _, ok := a.claudePulse.progress(time.Now()); ok {
		t.Error("the first reading should not pulse")
	}
	a.notePctStep(seriesClaudeWeekly, 12.5)
	if _, ok := a.claudePulse.progress(time.Now()); !ok {
		t.Fatal("a step up should pulse")
	}
	if _, ok := a.codexPulse.progress(time.Now()); ok {
		t.Error("a Claude step should not pulse the Codex dial")
	}
	a.claudePulse = spendPulse{}
	a.notePctStep(seriesClaudeWeekly, 0.3) // weekly reset
	a.notePctStep(seriesClaudeWeekly, 0.3) // flat poll
	if _, ok := a.claudePulse.progress(time.Now()); ok {
		t.Error("a reset or a flat reading should not pulse")
	}
}

// The menu's "Test it now" row has to play a burst on both dials without any
// spend to go on, with Codex a beat behind — and the frame loop must stay awake
// until that later burst has had its turn.
func TestPulseTestFiresBothDials(t *testing.T) {
	a := newApp()
	a.firePulseTest()
	now := time.Now()
	if _, ok := a.claudePulse.progress(now); !ok {
		t.Error("the Claude dial should burst immediately")
	}
	if _, ok := a.codexPulse.progress(now); ok {
		t.Error("the Codex dial should wait out the stagger")
	}
	if !a.pulseRunning(now) {
		t.Error("the frame loop should be awake while a burst is pending")
	}
	later := now.Add(pulseTestStagger + 100*time.Millisecond)
	if _, ok := a.codexPulse.progress(later); !ok {
		t.Error("the Codex dial should burst after the stagger")
	}
	if a.pulseRunning(now.Add(time.Duration(2 * pulseDur * float64(time.Second)))) {
		t.Error("the loop should go quiet once both bursts are over")
	}
}

func TestPulseFrameReachesTheSVG(t *testing.T) {
	defer func(m pulseMode) { pulseFX = m }(pulseFX)
	pulseFX = pulseSparks
	now := time.Now()
	burst := spendPulse{}.arm(now, pulseRefDelta)
	in := dialRenderInput{limits: blankLimits, markColor: dialGreen, hasMark: true,
		pulse: pulseIconFrame(burst, now.Add(300*time.Millisecond), pulseSparks)}
	svg := string(renderDialSVG([]dialRenderInput{in}))
	if !strings.Contains(svg, `data-pulse="halo"`) {
		t.Error("the in-icon flare should be drawn")
	}
	quiet := string(renderDialSVG([]dialRenderInput{{limits: blankLimits, markColor: dialGreen, hasMark: true}}))
	if strings.Contains(quiet, "data-pulse") {
		t.Error("an idle dial should draw no pulse")
	}
}

// The overlay canvas has to be the icon plus overlayPad on every side, with the
// two dial centres landing where the real icon draws them, or the sparks fly
// out of the wrong place.
func TestPulseOverlayCanvasFramesTheIcon(t *testing.T) {
	defer func(m pulseMode) { pulseFX = m }(pulseFX)
	pulseFX = pulseSparks
	a := newApp()
	a.appliedIconHeight = 18 // one point per dial unit, so the maths is readable
	now := time.Now()
	if _, _, _, ok := a.pulseOverlaySVG(now); ok {
		t.Error("an idle app should draw no overlay")
	}
	a.firePulseTest()
	svg, w, h, ok := a.pulseOverlaySVG(now.Add(200 * time.Millisecond))
	if !ok {
		t.Fatal("a running burst should draw an overlay")
	}
	if w != dialViewWidth(2)+2*overlayPad || h != 18+2*overlayPad {
		t.Errorf("overlay canvas %.1fx%.1f does not frame the icon", w, h)
	}
	if !strings.Contains(string(svg), fmt.Sprintf(`viewBox="0 0 %.2f %.2f"`, w, h)) {
		t.Errorf("overlay viewBox missing: %s", svg)
	}
	pulseFX = pulseGlow
	if _, _, _, ok := a.pulseOverlaySVG(now.Add(200 * time.Millisecond)); ok {
		t.Error("the overlay belongs to the sparks mode alone")
	}
}

// A side showing an error is blinking red; it has no spend to celebrate.
func TestPulseOverlaySkipsErroringSides(t *testing.T) {
	defer func(m pulseMode) { pulseFX = m }(pulseFX)
	pulseFX = pulseSparks
	a := newApp()
	a.appliedIconHeight = 18
	now := time.Now()
	a.firePulseTest()
	a.errorOn = true
	if _, _, _, ok := a.pulseOverlaySVG(now.Add(100 * time.Millisecond)); ok {
		t.Error("the erroring side should not spark")
	}
	a.codexErrorOn = false
	if _, _, _, ok := a.pulseOverlaySVG(now.Add(pulseTestStagger + 100*time.Millisecond)); !ok {
		t.Error("the healthy side should still spark")
	}
}
