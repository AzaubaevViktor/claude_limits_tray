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
	for _, at := range []float64{0, 0.3, pulseLen() - 0.01} {
		if _, ok := p.progress(now.Add(time.Duration(at * float64(time.Second)))); !ok {
			t.Errorf("burst should still be playing at %.2fs", at)
		}
	}
	if _, ok := p.progress(now.Add(time.Duration(pulseLen() * float64(time.Second)))); ok {
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
	if icon(pulseSparks, pulseLen()+0.01).Active || over(pulseSparks, pulseLen()+0.01).Active {
		t.Error("the frame after the burst should draw nothing")
	}
}

// The whole point of the overlay is that the burst leaves the icon: embers have
// to get past its rim (9 dial units) without overshooting the window, shake and
// all.
func TestSparksFlyOutPastTheIcon(t *testing.T) {
	now := time.Now()
	p := spendPulse{}.arm(now, pulseRefDelta)
	const reach = 25.0
	var far, prevMean float64
	for _, secs := range []float64{0.1, 0.5, 1.4} {
		f := pulseOverlayFrame(p, now.Add(time.Duration(secs*float64(time.Second))), pulseSparks, reach)
		mean := 0.0
		for _, s := range f.Sparks {
			d := math.Hypot(s.X, s.Y)
			if d < sparkRim-wobbleAmp-0.01 {
				t.Errorf("spark at %.2f started inside the rim", d)
			}
			if d > reach+0.01 {
				t.Errorf("spark at %.2f flew past the overlay window", d)
			}
			mean += d / float64(len(f.Sparks))
			far = math.Max(far, d)
		}
		if mean < prevMean {
			t.Error("sparks should only travel outward")
		}
		prevMean = mean
	}
	if far <= 9 {
		t.Errorf("sparks should leave the icon, got %.2f", far)
	}
}

// The shells have to go off, and what they throw has to go off again: the
// particle count should step up at each of the three stages.
func TestShellsBurstInThreeStages(t *testing.T) {
	const reach = 90.0
	whole := len(pulseSparkle(7, 1, pulseStages[0].splitLo-0.05, reach))
	split := len(pulseSparkle(7, 1, pulseStages[1].splitLo-0.02, reach))
	scattered := len(pulseSparkle(7, 1, 0.9, reach))
	if !(whole < split && split < scattered) {
		t.Errorf("want a step up per stage, got %d then %d then %d", whole, split, scattered)
	}
	early := pulseSparkle(7, 1, pulseStages[0].splitLo-0.05, reach)
	late := pulseSparkle(7, 1, 0.9, reach)
	var fattest, finest float64 = 0, math.MaxFloat64
	for _, s := range early {
		fattest = math.Max(fattest, s.R)
	}
	for _, s := range late {
		finest = math.Min(finest, s.R)
	}
	if finest >= fattest {
		t.Errorf("fragments (%.2f) should be smaller than the shells that threw them (%.2f)", finest, fattest)
	}
	// Whenever something goes off, what it throws must still respect the window.
	for _, at := range []float64{pulseStages[0].splitLo + 0.01, 0.6, 0.99} {
		for _, s := range pulseSparkle(7, 1, at, reach) {
			if d := math.Hypot(s.X, s.Y); d > reach+1e-9 {
				t.Errorf("fragment at %.2f flew past the window at t=%.2f", d, at)
			}
		}
	}
}

// Each leg of the flight is a throw, not a drift: most of its travel has to
// happen in the first quarter, so the particle visibly shoots out, creeps to a
// stop and then goes off.
func TestEachLegLaunchesHardThenCoasts(t *testing.T) {
	if early := pulseEase(0.25); early < 0.65 {
		t.Errorf("a quarter into a leg only %.2f of it is covered — too slow off the mark", early)
	}
	if pulseEase(0) != 0 || math.Abs(pulseEase(1)-1) > 1e-9 {
		t.Errorf("a leg should run 0..1, got %.3f..%.3f", pulseEase(0), pulseEase(1))
	}
	// Across a whole burst that shows up as a kick: the swarm's outward speed
	// picks up again after the splits instead of decaying all the way down.
	const reach = 240.0
	mean := func(t float64) float64 {
		ps := pulseSparkle(11, 1, t, reach)
		m := 0.0
		for _, s := range ps {
			m += math.Hypot(s.X, s.Y) / float64(len(ps))
		}
		return m
	}
	step := func(t float64) float64 { return mean(t+0.05) - mean(t) }
	slowest, kick := step(0.15), 0.0
	for t := 0.2; t < 0.8; t += 0.05 {
		slowest = math.Min(slowest, step(t))
		kick = math.Max(kick, step(t))
	}
	if kick < 1.5*slowest {
		t.Errorf("the splits should re-launch the swarm: slowest %.1f, fastest %.1f", slowest, kick)
	}
}

// The wave front must not arrive as one ring: particles differ in speed, in
// drag and in when they go off.
func TestBurstsFrayRatherThanMarchInStep(t *testing.T) {
	const reach = 240.0
	var splits []float64
	for i := 1; i <= 34; i++ {
		if at, ok := emberSplitAt(7, particle{id: i}); ok {
			splits = append(splits, at)
		}
	}
	if len(splits) < 5 {
		t.Fatalf("expected a good few shells, got %d", len(splits))
	}
	lo, hi := splits[0], splits[0]
	for _, v := range splits {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	if hi-lo < 0.1 {
		t.Errorf("shells go off too much in step: %.2f..%.2f", lo, hi)
	}
	// ... and at the end of the burst they are spread over a wide band, not
	// sitting on one circle.
	near, far := math.MaxFloat64, 0.0
	for _, s := range pulseSparkle(7, 1, 0.9, reach) {
		d := math.Hypot(s.X, s.Y)
		near, far = math.Min(near, d), math.Max(far, d)
	}
	if far-near < 0.3*far {
		t.Errorf("the front is too tight: %.1f..%.1f", near, far)
	}
}

// "Shaking" means the ember's own offset keeps changing direction over the
// flight — a steady drift would just be a different trajectory.
func TestEmbersShakeAlongTheWay(t *testing.T) {
	var prev float64
	flips := 0
	for i := 0; i <= 40; i++ {
		x, _ := emberShake(99, 3, float64(i)/40, 1)
		if i > 0 && (x > 0) != (prev > 0) {
			flips++
		}
		prev = x
	}
	if flips < 4 {
		t.Errorf("the shake should swing back and forth, got %d direction changes", flips)
	}
	if x, y := emberShake(99, 3, 0.5, 1); math.Hypot(x, y) > wobbleAmp+1e-9 {
		t.Errorf("the shake vector %.2f should stay within the budget %.2f", math.Hypot(x, y), wobbleAmp)
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
	if a.pulseRunning(now.Add(time.Duration(2 * pulseLen() * float64(time.Second)))) {
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
