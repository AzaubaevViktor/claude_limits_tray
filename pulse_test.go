package main

import (
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

func TestPulseFrameFadesOutAndRespectsTheMode(t *testing.T) {
	now := time.Now()
	p := spendPulse{}.arm(now, pulseRefDelta)
	at := func(mode pulseMode, secs float64) pulseFrame {
		return pulseFrameAt(p, now.Add(time.Duration(secs*float64(time.Second))), mode)
	}
	if f := at(pulseOff, 0.2); f.Active {
		t.Error("pulseOff should draw nothing")
	}
	if f := at(pulseGlow, 0.2); len(f.Sparks) != 0 {
		t.Error("pulseGlow should not throw sparks")
	}
	early, late := at(pulseSparks, 0.15), at(pulseSparks, 0.9)
	if !early.Active || !late.Active {
		t.Fatal("frames inside the burst should be active")
	}
	if len(early.Sparks) == 0 {
		t.Fatal("a full-strength burst should throw sparks")
	}
	if late.HaloAlpha >= early.HaloAlpha || late.Halo <= early.Halo {
		t.Errorf("the halo should grow and fade: %+v then %+v", early, late)
	}
	if late.MarkScale >= early.MarkScale || early.MarkScale <= 1 {
		t.Errorf("the centre dot should swell then settle back: %.2f then %.2f",
			early.MarkScale, late.MarkScale)
	}
	if f := at(pulseSparks, pulseDur+0.01); f.Active {
		t.Error("the frame after the burst should draw nothing")
	}
}

// Sparks fly outward into the side padding, and must stay inside the unpadded
// 18-unit height — the menu bar gives no vertical room to spare.
func TestSparksFlyOutwardAndStayInsideTheIconBox(t *testing.T) {
	now := time.Now()
	p := spendPulse{}.arm(now, pulseRefDelta)
	var maxX, prevNear float64
	for _, secs := range []float64{0.1, 0.4, 0.8} {
		f := pulseFrameAt(p, now.Add(time.Duration(secs*float64(time.Second))), pulseSparks)
		near := math.MaxFloat64
		for _, s := range f.Sparks {
			d := math.Hypot(s.X, s.Y)
			if d < sparkRim-0.01 {
				t.Errorf("spark at %.2f started inside the rim", d)
			}
			if math.Abs(s.Y)+s.R > 9 {
				t.Errorf("spark at y=%.2f (r=%.2f) leaves the icon box", s.Y, s.R)
			}
			if math.Abs(s.X)+s.R > 9+sparkPad {
				t.Errorf("spark at x=%.2f leaves the padded box", s.X)
			}
			near = math.Min(near, d)
			maxX = math.Max(maxX, math.Abs(s.X))
		}
		if prevNear > 0 && near < prevNear {
			t.Error("sparks should only travel outward")
		}
		prevNear = near
	}
	if maxX <= 9 {
		t.Errorf("sparks should reach past the dial edge, got %.2f", maxX)
	}
}

// A burst must look identical on every frame rendered from the same seed, and
// differ between bursts — the ember layout carries no state of its own.
func TestSparkLayoutIsStablePerBurst(t *testing.T) {
	a := pulseSparkle(42, 1, 0.5)
	if got := pulseSparkle(42, 1, 0.5); len(got) != len(a) || got[0] != a[0] {
		t.Error("the same seed and progress should give the same sparks")
	}
	if got := pulseSparkle(43, 1, 0.5); len(got) == len(a) && got[0] == a[0] {
		t.Error("a different seed should give a different burst")
	}
	if weak, strong := pulseSparkle(42, 0.1, 0.5), a; len(weak) >= len(strong) {
		t.Errorf("a small spend should throw fewer sparks: %d vs %d", len(weak), len(strong))
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
		pulse: pulseFrameAt(burst, now.Add(300*time.Millisecond), pulseSparks)}
	svg := string(renderDialSVG([]dialRenderInput{in}))
	if !strings.Contains(svg, `data-pulse="halo"`) {
		t.Error("the halo should be drawn")
	}
	if !strings.Contains(svg, `data-pulse="spark"`) {
		t.Error("the sparks should be drawn")
	}
	quiet := string(renderDialSVG([]dialRenderInput{{limits: blankLimits, markColor: dialGreen, hasMark: true}}))
	if strings.Contains(quiet, "data-pulse") {
		t.Error("an idle dial should draw no pulse")
	}
}
