package main

import (
	"fmt"
	"math"
	"time"
)

// quotaPace is the categorical, since-reset pace of a quota window. Unknown is
// deliberately not ordered as a danger level: it means there is not enough
// clock history to make a claim yet.
type quotaPace uint8

const (
	paceUnknown quotaPace = iota
	pacePurple
	paceGreen
	paceAmber
	paceRed
)

// quotaPaceFor mirrors the Claude Code statusline's limit_color rule. It
// projects the average pace since the window opened across the whole window.
// During the first 10% it normally withholds an opinion because an early burst
// is noisy, but a reading with at least 30 minutes and 5% of real spend is
// already strong enough evidence to surface an early warning. If the quota
// would run out, waitOKSecs separates a tolerable amber gap before reset from a
// red one.
func quotaPaceFor(pctUsed, remainingSecs, windowSecs, waitOKSecs float64) quotaPace {
	if windowSecs <= 0 || remainingSecs <= 0 || remainingSecs >= windowSecs {
		return paceUnknown
	}
	elapsed := windowSecs - remainingSecs
	const (
		minEarlyEvidenceSecs = 30 * 60
		minEarlySpendPct     = 5
	)
	if elapsed/windowSecs < 0.10 && (elapsed < minEarlyEvidenceSecs || pctUsed < minEarlySpendPct) {
		return paceUnknown
	}
	if pctUsed <= 0 {
		return pacePurple
	}
	if pctUsed < 100 {
		exhaustAt := elapsed * 100 / pctUsed
		if exhaustAt >= windowSecs {
			projected := pctUsed * windowSecs / elapsed
			if projected <= 90 {
				return pacePurple
			}
			return paceGreen
		}
		if windowSecs-exhaustAt <= math.Max(0, waitOKSecs) {
			return paceAmber
		}
		return paceRed
	}
	if remainingSecs <= math.Max(0, waitOKSecs) {
		return paceAmber
	}
	return paceRed
}

// worstQuotaPace returns the most dangerous known pace. Unknown is "no
// opinion", so it wins only when every input is unknown.
func worstQuotaPace(paces ...quotaPace) quotaPace {
	worst := paceUnknown
	for _, pace := range paces {
		if pace != paceUnknown && (worst == paceUnknown || pace > worst) {
			worst = pace
		}
	}
	return worst
}

// recentBurnRatio compares the latest measured rate with the rate that would
// consume exactly one quota over the whole window. It intentionally accepts no
// elapsed-window input: the chart speedometer must describe the last measured
// use, never the average since reset.
func recentBurnRatio(ratePerSec, windowSecs float64) float64 {
	if ratePerSec <= 0 || windowSecs <= 0 {
		return 0
	}
	return ratePerSec * windowSecs / 100
}

// projectedQuotaEnd returns when the remaining quota reaches zero if the most
// recently measured rate holds. A zero/unknown rate has no finite projection.
func projectedQuotaEnd(usedPct, ratePerSec float64, now time.Time) (time.Time, bool) {
	if usedPct >= 100 {
		return now, true
	}
	if ratePerSec <= 1e-9 {
		return time.Time{}, false
	}
	secs := math.Max(0, 100-usedPct) / ratePerSec
	return now.Add(time.Duration(secs * float64(time.Second))), true
}

// recentRateText follows the chart's time scale: hourly for sub-day charts and
// daily for day-scale charts.
func recentRateText(ratePerSec, chartSpanSecs float64, known bool) string {
	if !known {
		return "—"
	}
	unitSecs, unit := 3600.0, "h"
	if chartSpanSecs >= 24*60*60 {
		unitSecs, unit = 24*60*60, "d"
	}
	return fmt.Sprintf("%.1f%%/%s", ratePerSec*unitSecs, unit)
}

// recentMeasurementText says how wide the actual measurement interval was.
// Last-use age answers a separate question: a two-minute sample may itself
// have happened five minutes ago.
func recentMeasurementText(ratePerSec, measuredSpan float64, usedAt, now time.Time) string {
	if usedAt.IsZero() {
		return "measured —"
	}
	age := math.Max(0, now.Sub(usedAt).Seconds())
	if ratePerSec <= 1e-9 {
		return fmt.Sprintf("idle · last use %s ago", fmtSpan(age))
	}
	return fmt.Sprintf("measured over %s · last use %s ago", spanLabel(measuredSpan), fmtSpan(age))
}

// quotaEndText formats the finite exhaustion time from the latest rate. If it
// lands after a known reset, the useful answer is that the current quota lasts
// to reset rather than a misleading time in the next window.
func quotaEndText(usedPct, ratePerSec float64, resetAt, now time.Time) string {
	end, ok := projectedQuotaEnd(usedPct, ratePerSec, now)
	if !ok {
		return "empty —"
	}
	if !resetAt.IsZero() && !end.Before(resetAt) {
		return "lasts to reset"
	}
	if !end.After(now) {
		return "empty now"
	}
	clock := end.Format("15:04")
	if !sameDay(end, now) {
		clock = end.Format("Mon 15:04")
		if end.Sub(now) >= 6*24*time.Hour {
			clock = end.Format("Jan 2 15:04")
		}
	}
	return fmt.Sprintf("empty %s · %s", fmtRemaining(end.Sub(now).Seconds(), true), clock)
}

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
var dialGray = [3]int{142, 142, 147} // unknown pace — not enough window history yet

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
// The measurement accumulates from a baseline taken at each window reset, NOT
// from poll-to-poll deltas: across a whole 5-hour window the session may climb
// 0→60% while the week climbs a few %, and Δweek/Δsession over that large span
// is robust. Tiny per-poll deltas are dominated by the 1% rounding of the
// utilisation figures (e.g. Δs=4, Δw=1 reads as 25%/session when the truth is
// ~10%), which is exactly the noise we avoid. A retained sample bridges resets
// until the current window has accumulated three weekly percentage points.
type WeeklyCalibrator struct {
	haveBase           bool
	baseSess, baseWeek float64
	lastSess, lastWeek float64 // previous reading, to detect a window reset
	haveLast           bool
	bestDs, bestDw     float64
	bestAt             time.Time
}

const (
	// calibMinDs is an absolute floor for the session span. In addition, a new
	// sample must cover calibMinWeekSteps whole weekly percentage points.
	calibMinDs        = 10.0
	calibMinWeekSteps = 3.0
	calibMaxAge       = 24 * time.Hour
)

// Observe feeds the latest poll's session/weekly utilisation (both 0..100). On
// the first call, or whenever either counter drops (a window reset), it re-takes
// the baseline. Otherwise it samples the span since the baseline. The best
// sample survives a reset for up to a day, giving the next window time to build
// enough resolution before replacing it.
func (c *WeeklyCalibrator) Observe(sess, week float64) {
	c.ObserveAt(time.Now(), sess, week)
}

// ObserveAt is Observe with an explicit clock, for deterministic expiry tests.
func (c *WeeklyCalibrator) ObserveAt(now time.Time, sess, week float64) {
	// A window reset is a DROP from the previous reading, not a value below the
	// baseline: the baseline is the window's low point (~0), so the 5-hour session
	// wrapping 100→0 never dips below it and the reset goes unnoticed — leaving the
	// baseline pinned while Δweek accrues across many sessions and Δsession only
	// tracks the current position, hugely inflating the ratio. Compare to the last
	// reading (as RateTracker does) so each session reset re-anchors the baseline.
	reset := !c.haveBase || sess < c.lastSess-1 || week < c.lastWeek-1
	c.lastSess, c.lastWeek, c.haveLast = sess, week, true
	if reset {
		c.baseSess, c.baseWeek, c.haveBase = sess, week, true
		return
	}
	// A saturated session (utilisation pinned at the 100% cap) is useless as a
	// sample endpoint: the session counter stops climbing while the weekly keeps
	// rising, so Δweek/Δsession over-counts and inflates the block size (a window
	// where 1.5 sessions' worth of work spilled past the cap reads as one session
	// costing 15%/wk when the truth is 10%/wk). Skip it — the largest sub-100
	// reading already carries the same span with an accurate Δsession.
	if sess >= 100 {
		return
	}
	ds, dw := sess-c.baseSess, week-c.baseWeek
	if ds < c.minFreshDs(now) || dw < calibMinWeekSteps {
		return
	}
	// One full session cannot cost more than the whole week: you would hit the
	// weekly cap before finishing it. A sample with dw > ds implies BlockPct > 100
	// and is corrupt (usually a missed reset) — reject rather than clamp, so a bad
	// value never sticks as "best".
	if dw > ds {
		return
	}
	// A sufficiently resolved sample from the current window is preferable to
	// an older, wider one: it reflects the current model/account price. As this
	// window grows, each later endpoint sharpens the same measurement.
	c.bestDs, c.bestDw, c.bestAt = ds, dw, now
}

// minFreshDs asks a replacement to span at least three expected 1%-weekly
// steps. For example, a 10%-weekly session needs about 30% session spend. The
// direct dw check in Observe guards against an outdated estimate being too
// optimistic.
func (c *WeeklyCalibrator) minFreshDs(now time.Time) float64 {
	minDs := calibMinDs
	if blockPct, ok := c.BlockPctAt(now); ok {
		minDs = math.Max(minDs, calibMinWeekSteps*100/blockPct)
	}
	return minDs
}

// BlockPct is the weekly % consumed by one full 5-hour session, and whether it
// has been measured. There is no synthetic default: until a real spend lands it
// returns (0, false) and callers draw nothing — the value is only ever computed
// from observed consumption.
func (c *WeeklyCalibrator) BlockPct() (float64, bool) {
	return c.BlockPctAt(time.Now())
}

// BlockPctAt is BlockPct with an explicit clock, for deterministic expiry tests.
func (c *WeeklyCalibrator) BlockPctAt(now time.Time) (float64, bool) {
	if c.bestDs <= 0 || c.bestDw <= 0 || c.bestAt.IsZero() || now.Sub(c.bestAt) >= calibMaxAge {
		return 0, false
	}
	return clamp(c.bestDw/c.bestDs*100, 0, 100), true
}

// Snapshot/Restore persist the best sample across restarts (in the config file).
func (c *WeeklyCalibrator) Snapshot() (float64, float64) { return c.bestDs, c.bestDw }
func (c *WeeklyCalibrator) SnapshotAt() float64 {
	if c.bestAt.IsZero() {
		return 0
	}
	return float64(c.bestAt.Unix())
}

func (c *WeeklyCalibrator) Restore(ds, dw float64) {
	c.RestoreAt(ds, dw, time.Now())
}

// RestoreAt also restores sample age. A legacy saturated sample is retained as
// a temporary fallback, but Observe never refreshes it and the one-day TTL (or
// a sufficiently resolved fresh sample) removes its influence. A sample with
// dw > ds (BlockPct > 100, impossible — see ObserveAt) is dropped outright so a
// corrupt persisted value from the missed-reset bug does not survive a restart.
func (c *WeeklyCalibrator) RestoreAt(ds, dw float64, at time.Time) {
	if ds >= calibMinDs && dw > 0 && dw <= ds && !at.IsZero() {
		c.bestDs, c.bestDw, c.bestAt = ds, dw, at
	}
}

// RateTracker derives a consumption rate over the last WINDOW seconds.
type rateSample struct{ t, sess, week float64 }

type RateTracker struct {
	history []rateSample
	lastDt  float64 // span (s) the most recent successful rate was measured over
}

// Span is how many seconds the last computed rate was measured over — the real
// window behind the "Last …" label, which can exceed rateWindow when polls are
// sparse. 0 until a rate has been computed.
func (r *RateTracker) Span() float64 { return r.lastDt }

const (
	rateWindow = 15 * 60.0
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
	// smoothed over ~15 min. But when the poll gap is itself longer than the
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
	r.lastDt = dt
	return math.Max(0, (sess-base.sess)/dt), math.Max(0, (week-base.week)/dt), true
}
