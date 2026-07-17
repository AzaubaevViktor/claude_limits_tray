package main

import (
	"fmt"
	"math"
)

// Forecast projects the recent consumption rate forward to a window's reset.
type Forecast struct {
	Category  string  // purple | green | yellow | red
	Spare     float64 // % of quota spare at the reset (negative = overshoot)
	BreakSecs float64
	HasBreak  bool
}

// projectForecast: rate is % of quota per second; ttr is seconds to the reset.
//
//	purple  > 10% of quota would still be spare at the reset
//	green   0..10% spare (you'd use almost exactly your quota)
//	yellow  you'd overshoot, but a break <= 1h makes it last
//	red     overshoot needing a break > 1h
func projectForecast(pct, rate, ttr float64, ttrKnown bool) Forecast {
	remaining := math.Max(0, 100-pct)
	if !ttrKnown || rate <= 1e-9 {
		return Forecast{"purple", remaining, 0, false}
	}
	spare := remaining - rate*ttr
	if spare > 10 {
		return Forecast{"purple", spare, 0, false}
	}
	if spare >= 0 {
		return Forecast{"green", spare, 0, false}
	}
	brk := ttr - remaining/rate // > 0 when overshooting
	cat := "yellow"
	if brk > 3600 {
		cat = "red"
	}
	return Forecast{cat, spare, brk, true}
}

// spareText is the projected % of quota still spare at the reset for a single
// rate (negative = you'd overshoot), as a compact "12%" / "-8%" label.
func spareText(pct, rate, ttr float64, ttrKnown bool) string {
	return fmt.Sprintf("%.0f%%", projectForecast(pct, rate, ttr, ttrKnown).Spare)
}

var burnRed = [3]int{255, 69, 58}
var dialGray = [3]int{142, 142, 147} // idle centre — no tokens being spent right now

// gradient stops along the continuous severity axis (pos 0..1).
var stops = []struct {
	p float64
	c [3]int
}{
	{0.00, [3]int{175, 82, 222}}, // purple — lots of headroom
	{0.33, [3]int{48, 209, 88}},  // green  — finishing near zero
	{0.66, [3]int{255, 204, 0}},  // yellow — small overshoot
	{1.00, burnRed},              // red    — large overshoot
}

// Pos is the continuous severity, 0 (relaxed) .. 1 (way over budget).
func (f Forecast) Pos() float64 {
	if !f.HasBreak {
		return 0.33 * (1 - clamp(f.Spare/30, 0, 1))
	}
	if f.BreakSecs <= 3600 {
		return 0.33 + 0.33*(f.BreakSecs/3600)
	}
	over := (f.BreakSecs - 3600) / (3 * 3600)
	return 0.66 + 0.34*clamp(over, 0, 1)
}

func gradient(pos float64) [3]int {
	pos = clamp(pos, 0, 1)
	for i := 0; i+1 < len(stops); i++ {
		p0, c0, p1, c1 := stops[i].p, stops[i].c, stops[i+1].p, stops[i+1].c
		if pos <= p1 {
			t := 0.0
			if p1 > p0 {
				t = (pos - p0) / (p1 - p0)
			}
			var out [3]int
			for j := 0; j < 3; j++ {
				out[j] = int(math.Round(float64(c0[j]) + (float64(c1[j])-float64(c0[j]))*t))
			}
			return out
		}
	}
	return burnRed
}

func (f Forecast) Color() [3]int { return gradient(f.Pos()) }

// speedColor colours the centre dot by burn speed: grey when nothing is being
// spent right now, green at a sustainable pace, ramping green→yellow→red as the
// projected spend would exhaust the session or weekly window before it resets.
// active is whether any tokens are being consumed at the moment.
func speedColor(session, weekly Forecast, active bool) [3]int {
	if !active {
		return dialGray
	}
	worst := session
	if weekly.Pos() > worst.Pos() {
		worst = weekly
	}
	// The centre never advertises head-room: collapse the relaxed purple half of
	// the severity axis onto green, so it only ever reads green→yellow→red.
	return gradient(clamp(worst.Pos(), 0.33, 1))
}

func (f Forecast) Text() string {
	if f.HasBreak {
		return fmt.Sprintf("over — pause %s to last", fmtRemaining(f.BreakSecs, true))
	}
	tight := ""
	if f.Category != "purple" {
		tight = " (tight)"
	}
	return fmt.Sprintf("%.0f%% spare at reset%s", f.Spare, tight)
}

// WeeklyCalibrator learns how many weekly-% one 1% of the 5-hour session costs,
// so the weekly dial can be sliced into "one full 5-hour session" blocks.
//
// The measurement accumulates from a baseline taken at each window reset, NOT
// from poll-to-poll deltas: across a whole 5-hour window the session may climb
// 0→60% while the week climbs a few %, and Δweek/Δsession over that large span
// is robust. Tiny per-poll deltas are dominated by the 1% rounding of the
// utilisation figures (e.g. Δs=4, Δw=1 reads as 25%/session when the truth is
// ~10%), which is exactly the noise we avoid. We keep the largest-basis sample
// (biggest Δsession with Δweek>0) and it only sharpens as a window runs longer.
type WeeklyCalibrator struct {
	haveBase           bool
	baseSess, baseWeek float64
	bestDs, bestDw     float64 // the largest-basis sample kept so far
}

// calibMinDs is the smallest session span (in %, since the window opened) we
// trust as a calibration sample; below it Δweek is mostly 1% rounding noise.
const calibMinDs = 10.0

// Observe feeds the latest poll's session/weekly utilisation (both 0..100). On
// the first call, or whenever either counter drops (a window reset), it re-takes
// the baseline. Otherwise it samples the span since the baseline. The best
// sample survives resets: the price is a property of usage, not of the window.
func (c *WeeklyCalibrator) Observe(sess, week float64) {
	if !c.haveBase || sess < c.baseSess || week < c.baseWeek {
		c.baseSess, c.baseWeek, c.haveBase = sess, week, true
		return
	}
	ds, dw := sess-c.baseSess, week-c.baseWeek
	if ds < calibMinDs || dw <= 0 { // not enough span yet, or no weekly movement
		return
	}
	if ds > c.bestDs { // largest basis wins — least quantisation noise
		c.bestDs, c.bestDw = ds, dw
	}
}

// BlockPct is the weekly % consumed by one full 5-hour session, and whether it
// has been measured. There is no synthetic default: until a real spend lands it
// returns (0, false) and callers draw nothing — the value is only ever computed
// from observed consumption.
func (c *WeeklyCalibrator) BlockPct() (float64, bool) {
	if c.bestDs <= 0 || c.bestDw <= 0 {
		return 0, false
	}
	return clamp(c.bestDw/c.bestDs*100, 0, 100), true
}

// Snapshot/Restore persist the best sample across restarts (in the config file).
// Restore drops a sample whose basis is below the trust floor — this also clears
// stale tiny-basis values written by older noisier builds.
func (c *WeeklyCalibrator) Snapshot() (float64, float64) { return c.bestDs, c.bestDw }
func (c *WeeklyCalibrator) Restore(ds, dw float64) {
	if ds >= calibMinDs && dw > 0 {
		c.bestDs, c.bestDw = ds, dw
	}
}

// RateTracker derives a consumption rate over the last WINDOW seconds.
type rateSample struct{ t, sess, week float64 }

type RateTracker struct {
	history []rateSample
}

const (
	rateWindow = 300.0
	rateMinDt  = 10.0
	rateMaxLen = 64
)

// Update returns (sessRate, weekRate, ok). ok is false until there is enough
// history (>= rateMinDt seconds) — callers keep the previous rate meanwhile.
func (r *RateTracker) Update(now, sess, week float64) (float64, float64, bool) {
	if n := len(r.history); n > 0 {
		last := r.history[n-1]
		if sess < last.sess-1 || week < last.week-1 {
			r.history = r.history[:0] // a window reset; restart the measurement
		}
	}
	r.history = append(r.history, rateSample{now, sess, week})
	if len(r.history) > rateMaxLen {
		r.history = r.history[len(r.history)-rateMaxLen:]
	}
	// Base the rate on the oldest sample still inside the window, so a burst is
	// smoothed over ~5 min. But when the poll gap is itself longer than the
	// window (slow/manual refresh, or a grown backoff interval), that leaves only
	// the just-added sample in range — measuring over dt=0 would wrongly read 0.
	// Fall back to the previous sample so we always span at least the last gap.
	cutoff := now - rateWindow
	n := len(r.history)
	base := r.history[n-1]
	if n >= 2 {
		base = r.history[n-2]
		for _, s := range r.history[:n-1] {
			if s.t >= cutoff {
				base = s
				break
			}
		}
	}
	dt := now - base.t
	if dt < rateMinDt {
		return 0, 0, false
	}
	return math.Max(0, (sess-base.sess)/dt), math.Max(0, (week-base.week)/dt), true
}
