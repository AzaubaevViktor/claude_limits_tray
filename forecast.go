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
// Between two polls the session climbs by Δs (% of the 5-hour quota) and the
// week climbs by Δw (% of the weekly quota); their ratio k = Δw/Δs is the price,
// in weekly-%, of 1% of a session. A full session (100% of the 5-hour limit)
// therefore costs k*100 weekly %. We keep the single largest-spend sample (the
// biggest Δs with Δw>0): a big jump carries the least quantisation noise, so it
// is the most trustworthy estimate, and it only sharpens as bigger spends land.
type WeeklyCalibrator struct {
	havePrev           bool
	prevSess, prevWeek float64
	bestDs, bestDw     float64 // the largest-spend sample kept so far
}

// calibMinDs is the smallest session jump (in %) we trust as a calibration
// sample; below it Δw is mostly rounding noise. defaultBlockPct is the seed
// price (weekly % per full 5-hour session) shown before anything is measured —
// ~10 blocks per week — so the dial has separators from the first paint and
// real spends only refine it.
const (
	calibMinDs      = 1.0
	defaultBlockPct = 10.0
)

// Observe feeds the latest poll's session/weekly utilisation (both 0..100). A
// window reset (either counter dropping) is skipped — the next poll re-seeds
// prev. The best sample survives resets: the price is a property of usage, not
// of the window.
func (c *WeeklyCalibrator) Observe(sess, week float64) {
	prevS, prevW, had := c.prevSess, c.prevWeek, c.havePrev
	c.prevSess, c.prevWeek, c.havePrev = sess, week, true
	if !had {
		return
	}
	ds, dw := sess-prevS, week-prevW
	if ds < calibMinDs || dw <= 0 { // a reset, idle, or too small to trust
		return
	}
	if ds > c.bestDs { // largest spend wins — least quantisation noise
		c.bestDs, c.bestDw = ds, dw
	}
}

// BlockPct is the weekly % consumed by one full 5-hour session, and whether it
// was measured. Before any sample lands it returns defaultBlockPct with
// measured=false, so callers always have a usable price to show.
func (c *WeeklyCalibrator) BlockPct() (float64, bool) {
	if c.bestDs <= 0 || c.bestDw <= 0 {
		return defaultBlockPct, false
	}
	return clamp(c.bestDw/c.bestDs*100, 0, 100), true
}

// Snapshot/Restore persist the best sample across restarts (in the config file).
func (c *WeeklyCalibrator) Snapshot() (float64, float64) { return c.bestDs, c.bestDw }
func (c *WeeklyCalibrator) Restore(ds, dw float64) {
	if ds > 0 && dw > 0 {
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
	cutoff := now - rateWindow
	base := r.history[0]
	for _, s := range r.history {
		if s.t >= cutoff {
			base = s
			break
		}
	}
	dt := now - base.t
	if dt < rateMinDt {
		return 0, 0, false
	}
	return math.Max(0, (sess-base.sess)/dt), math.Max(0, (week-base.week)/dt), true
}
