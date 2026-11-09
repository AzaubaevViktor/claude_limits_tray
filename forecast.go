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
// The clock is the caller's: the dial passes working seconds (see limitPace),
// so the projection and the tolerances are in working time.
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
// consume exactly one quota over the whole window (the nominal pace). It is the
// fallback reference when the reset time is unknown; see affordableBurnRatio.
func recentBurnRatio(ratePerSec, windowSecs float64) float64 {
	if ratePerSec <= 0 || windowSecs <= 0 {
		return 0
	}
	return ratePerSec * windowSecs / 100
}

// burnRatioCap bounds the ratio once the affordable pace is zero (quota already
// empty) so callers can format it; anything past 2x is pegged on the dial anyway.
const burnRatioCap = 99.0

// affordableRate is the fastest steady consumption (quota %/s) that still
// lasts until the reset: the quota left divided by the time left. With plenty
// of quota and little time it is huge (burn freely); with little quota and a
// long wait it is tiny (save hard). ok is false when there is no time left to
// divide by.
func affordableRate(usedPct, remainingSecs float64) (float64, bool) {
	if remainingSecs <= 0 {
		return 0, false
	}
	return math.Max(0, 100-usedPct) / remainingSecs, true
}

// affordableBurnRatio compares the latest measured rate with the pace that can
// still be afforded until the reset: 1 means the remaining quota runs out
// exactly at the reset, below 1 leaves spare, above 1 runs dry early. It still
// describes only the last measured use, never the average since reset. When
// the reset time is unknown it falls back to the whole-window nominal pace.
func affordableBurnRatio(ratePerSec, usedPct, remainingSecs, windowSecs float64, resetKnown bool) float64 {
	if ratePerSec <= 0 {
		return 0
	}
	if !resetKnown {
		return recentBurnRatio(ratePerSec, windowSecs)
	}
	afford, ok := affordableRate(usedPct, remainingSecs)
	if !ok {
		return recentBurnRatio(ratePerSec, windowSecs)
	}
	if afford <= 1e-12 {
		return burnRatioCap
	}
	return math.Min(ratePerSec/afford, burnRatioCap)
}

// nominalPaceRatio places the whole-window nominal pace (100% over the window)
// on the affordable scale: below 1 you can afford faster than normal, above 1
// even the normal pace is too fast for what is left. ok is false when there is
// nothing to compare against.
func nominalPaceRatio(usedPct, remainingSecs, windowSecs float64, resetKnown bool) (float64, bool) {
	if !resetKnown || windowSecs <= 0 {
		return 0, false
	}
	return affordableBurnRatio(100/windowSecs, usedPct, remainingSecs, windowSecs, true), true
}

// quotaDurationText formats a compact duration in thirds of an hour/day. The
// chart uses it for both the wall-clock reset countdown and quota runway, so
// those values remain directly comparable without pretending they mean the
// same thing.
func quotaDurationText(seconds float64, known bool) string {
	if !known {
		return "—"
	}
	if seconds <= 0 {
		return "0H"
	}
	unitSecs, unit := 3600.0, "H"
	if seconds >= 24*60*60 {
		unitSecs, unit = 24*60*60, "D"
	}
	thirds := int(math.Round(seconds / unitSecs * 3))
	if thirds < 1 {
		return "<⅓" + unit
	}
	whole, fraction := thirds/3, thirds%3
	fractionText := ""
	if fraction == 1 {
		fractionText = "⅓"
	} else if fraction == 2 {
		fractionText = "⅔"
	}
	wholeText := ""
	if whole > 0 {
		wholeText = fmt.Sprintf("%d", whole)
	}
	return wholeText + fractionText + unit
}

// budgetRunwayText says how long a remaining budget would last at one explicit
// consumption rate. Hours and days are rounded to thirds to keep chart labels
// compact while still making e.g. 3h20m visible as 3⅓H.
func budgetRunwayText(remaining, ratePerSec float64, known bool) string {
	if !known {
		return "— left"
	}
	if remaining <= 0 {
		return "0H left"
	}
	if ratePerSec <= 1e-9 {
		return "∞ left"
	}
	return quotaDurationText(remaining/ratePerSec, true) + " left"
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

// Pos is the continuous severity, 0 (relaxed) .. 1 (way over budget): purple
// headroom up to 0.33, green finishing near zero, yellow for a small
// overshoot up to 0.66, red beyond. Used to pick the worse of two windows.
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
	// reading, so each session reset re-anchors the baseline.
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

// rateMinDt is the shortest span a measured speed may be based on, and
// rateWindow the window the rolling averages (the credit card's AVG line) are
// taken over. The live speeds are no longer window-based: they measure from
// the last counter change instead (see runUseRate).
const (
	rateWindow = 15 * 60.0
	rateMinDt  = 10.0
)
