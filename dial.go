package main

import (
	"fmt"
	"math"
	"strings"
)

// Limit is the per-window input to the dial.
type Limit struct {
	Pct        float64 // used 0..100
	TimeLeft   float64 // fraction of window remaining 0..1
	WindowSecs float64 // whole window; 0 means its clock is unknown
	WaitOKSecs float64 // tolerable gap between exhaustion and reset (pace seconds)
	// PaceLeftSecs and PaceWindowSecs are the working-hours clock the pace
	// colour is projected on: working seconds to the reset and in the whole
	// window. When PaceWindowSecs is 0 the wall clock (TimeLeft, WindowSecs)
	// is used instead.
	PaceLeftSecs   float64
	PaceWindowSecs float64
	BlockPct       float64 // weekly % per 5-hour-session block; 0 = no separators
	TimeTicks      int     // segments to split the time ring into (hours/days); <2 = none
}

// maxWeekTicks caps how many 5-hour-block separators the weekly semicircle can
// show, so a cheap-per-session estimate can't draw an unreadable comb. It is
// applied where BlockPct is produced (weeklyBlockPct in app.go), not here.
const maxWeekTicks = 40

type segment struct {
	ring, side, source, metric string
	bright                     bool
	label                      string
}

// Dial geometry (in an 18x18 coordinate space; rendered scaled up for retina).
// The two quota/time rings sit inside a thin credits ring at the very edge
// (creditR), which is only drawn when the provider has a credit budget.
const (
	dialCX, dialCY = 9.0, 9.0
	dialSW         = 1.7
	halfShift      = 0.8
	endGap         = 5.0
	pxSize         = 44   // rendered pixel size; scaled to the menu bar by systray
	creditR        = 8.45 // thin outer credits ring radius
	creditSW       = 0.6  // ... and stroke width
)

var rings = map[string]float64{"outer": 6.3, "inner": 3.8}
var sideSign = map[string]float64{"left": -1, "right": +1}
var ringLabel = map[string]string{"outer": "Outer ring", "inner": "Inner ring"}
var sideLabel = map[string]string{"left": "left", "right": "right"}

var dialGreen = [3]int{48, 209, 88}
var dialYellow = [3]int{255, 204, 0}
var dialRed = [3]int{255, 69, 58}
var dialPurple = [3]int{191, 64, 255} // vivid headroom colour; stays distinct at menu-bar size
var dialTrack = [3]int{150, 150, 150}
var dialUnavail = [3]int{200, 200, 200} // session budget unreachable because the weekly cap runs out first
var dialWhite = [3]int{255, 255, 255}

// creditRing is the thin outermost ring: the credit balance projected against
// the time to the next reset. The whole circle is the time to reset; the lit
// arc (from the top, clockwise) is how much of it the balance covers at the
// recent burn rate, coloured red → yellow → green as the cover grows; the
// lighter head of that arc is the next 24 hours' spend. No ring is drawn when
// the provider has no credit budget; a plain grey track means the budget exists
// but cannot be projected yet (no reset time or balance); a red track means the
// balance is exhausted.
type creditRing struct {
	Show      bool
	Projected bool
	Empty     bool
	Cover     float64 // 0..1 share of the time to reset the balance covers
	DaySlice  float64 // 0..1 share of the ring one day occupies (<= Cover)
}

// lerpColor blends a->b as t runs 0..1.
func lerpColor(a, b [3]int, t float64) [3]int {
	t = clamp(t, 0, 1)
	var out [3]int
	for i := 0; i < 3; i++ {
		out[i] = int(math.Round(float64(a[i]) + (float64(b[i])-float64(a[i]))*t))
	}
	return out
}

func quotaPaceColor(pace quotaPace) [3]int {
	switch pace {
	case pacePurple:
		return dialPurple
	case paceGreen:
		return dialGreen
	case paceAmber:
		return dialYellow
	case paceRed:
		return dialRed
	default:
		return dialGray
	}
}

// limitPace is the window's pace category, projected over working hours when
// the Limit carries that clock (see Limit.PaceWindowSecs) so an evening's
// spend is measured against the working time left, not 24h days.
func limitPace(l Limit, limitLeft float64) quotaPace {
	left, window := clamp(l.TimeLeft, 0, 1)*l.WindowSecs, l.WindowSecs
	if l.PaceWindowSecs > 0 {
		left, window = l.PaceLeftSecs, l.PaceWindowSecs
	}
	return quotaPaceFor(100*(1-clamp(limitLeft, 0, 1)), left, window, l.WaitOKSecs)
}

// effectiveLimitLeft applies the weekly cap to the short/session window. It is
// shared by rendering and cache keys so a colour transition always invalidates
// the icon even when the rounded arc lengths have not changed yet.
func effectiveLimitLeft(limits map[string]Limit, source string) float64 {
	l := limits[source]
	left := clamp(1-l.Pct/100, 0, 1)
	if source == "session" {
		if weekly, ok := limits["weekly"]; ok && weekly.BlockPct > 0 {
			weeklyLeft := clamp(1-weekly.Pct/100, 0, 1)
			left = math.Min(left, clamp(weeklyLeft/(weekly.BlockPct/100), 0, 1))
		}
	}
	return left
}

func dialPaceKey(limits map[string]Limit) string {
	return fmt.Sprintf("%d-%d",
		limitPace(limits["session"], effectiveLimitLeft(limits, "session")),
		limitPace(limits["weekly"], effectiveLimitLeft(limits, "weekly")),
	)
}

// aggregateDialPace is the centre dot's opinion: the worse of the two outer
// quota windows, with an unknown window ignored unless neither is known yet.
func aggregateDialPace(limits map[string]Limit) quotaPace {
	return worstQuotaPace(
		limitPace(limits["session"], effectiveLimitLeft(limits, "session")),
		limitPace(limits["weekly"], effectiveLimitLeft(limits, "weekly")),
	)
}

// timeFill selects how the inner (time) ring fills. The colour is the same
// either way — greener the less time is left in the window — only the arc
// length differs. User-toggled from the menu and persisted.
type fillMode int

const (
	fillElapsed   fillMode = iota // arc grows as the window elapses (default)
	fillRemaining                 // arc shrinks as the window elapses
)

var timeFill = fillElapsed

func timeFillString() string {
	if timeFill == fillRemaining {
		return "remaining"
	}
	return "elapsed"
}

func setTimeFillFromString(s string) {
	switch s {
	case "remaining":
		timeFill = fillRemaining
	case "elapsed":
		timeFill = fillElapsed
	}
}

// SEGMENTS: edit this table; the icon and the legend both follow.
//
//	metric         arc length            colour
//	percent_left   % of quota spendable  PACE category (quotaPaceColor via
//	               (session: capped by   limitPace): purple/green/yellow/red by
//	               the weekly, see       the since-reset projection, grey while
//	               effectiveLimitLeft)   learning; a light-grey tail marks the
//	                                     part the weekly cap makes unspendable
//	time           time within window    GREENS toward reset (the less time
//	               left, the greener); the arc fills with elapsed or remaining
//	               time per the timeFill toggle, but the colour is unchanged
var segments = []segment{
	{"outer", "left", "session", "percent_left", true, "Session left"},
	{"inner", "left", "session", "time", false, "Session time"},
	{"outer", "right", "weekly", "percent_left", true, "Weekly left"},
	{"inner", "right", "weekly", "time", false, "Weekly time"},
}

// legendRows: one row per dial segment, then four rows explaining the colour
// systems (outer pace, inner time, centre aggregate, credits edge ring) and one
// for the spend pulse.
func legendRows() [][2]string {
	rows := make([][2]string, 0, len(segments)+5)
	for _, s := range segments {
		label := s.label
		if s.metric == "time" {
			label = timeSegLabel(s.source)
		}
		rows = append(rows, [2]string{ringLabel[s.ring] + " · " + sideLabel[s.side], label})
	}
	rows = append(rows,
		[2]string{"Outer colour", "since-reset pace over working hours — purple: under · green: on · yellow: short gap · red: long gap · grey: learning · light-grey tail: unspendable (weekly cap)"},
		[2]string{"Inner colour", "time left — greener toward the reset"},
		[2]string{"Centre dot", "worst outer pace — grey while both windows are still learning"},
		[2]string{"Thin edge ring", "credits: share of the time to reset the balance covers at the recent rate · lighter head = next day's spend · grey: not projectable · red: exhausted"},
		[2]string{"Spend pulse", pulseLegendText()},
	)
	return rows
}

// pulseLegendText describes what the dial does when quota % is spent, per the
// current Spend pulse setting.
func pulseLegendText() string {
	switch pulseFX {
	case pulseOff:
		return "off — the dial stays still as quota is spent"
	case pulseGlow:
		return "the centre dot flashes and a halo rolls out each time the quota % ticks up"
	default:
		return "the centre dot flashes, a halo rolls out and sparks fly off the rim each time the quota % ticks up"
	}
}

func timeSegLabel(source string) string {
	name := map[string]string{"session": "Session", "weekly": "Weekly"}[source]
	if name == "" {
		name = source
	}
	if timeFill == fillRemaining {
		return name + " time left (fills with time left)"
	}
	return name + " since reset (fills with elapsed)"
}

// hourTicks/dayTicks derive how many equal time segments a window splits into —
// by hours (session) or days (weekly) — clamped so an odd window can't comb the
// dial. <2 means no internal ticks.
func hourTicks(windowSecs float64) int { return timeTicks(windowSecs / 3600) }
func dayTicks(windowSecs float64) int  { return timeTicks(windowSecs / 86400) }
func timeTicks(n float64) int          { return int(clamp(math.Round(n), 0, 24)) }

// metric returns (arc length, health) in 0..1.
func metricFor(name string, l Limit) (float64, float64) {
	pctLeft := clamp(1-l.Pct/100, 0, 1)
	tLeft := clamp(l.TimeLeft, 0, 1)
	switch name {
	case "percent_left":
		return pctLeft, pctLeft
	case "percent_used":
		return 1 - pctLeft, pctLeft
	case "time_left":
		return tLeft, tLeft
	case "time_elapsed":
		return 1 - tLeft, 1 - tLeft
	case "time":
		// Colour always greens as the window drains (redder with more time
		// left); only the arc length follows the fill toggle.
		health := 1 - tLeft
		if timeFill == fillRemaining {
			return tLeft, health
		}
		return 1 - tLeft, health
	}
	return 0, 0
}

func healthColor(health float64) [3]int {
	health = clamp(health, 0, 1)
	if health >= 0.5 {
		return lerpColor(dialYellow, dialGreen, (health-0.5)/0.5)
	}
	return lerpColor(dialRed, dialYellow, health/0.5)
}

// segColor picks a segment's colour: the outer quota ring reads the statusline
// pace projection (purple/green/amber/red/unknown); every other ring uses the
// health gradient.
// limitLeft is the effective quota fraction left — for the session ring this is
// the weekly-capped usable amount, not the raw incoming %, so the colour tells
// the truth (red when the weekly leaves almost nothing spendable).
func segColor(seg segment, l Limit, limitLeft, health float64) [3]int {
	if seg.metric == "percent_left" {
		return quotaPaceColor(limitPace(l, limitLeft))
	}
	return healthColor(health)
}

// phi converts a "clockwise from the top" angle (degrees) to SVG's angle (rad).
func phi(deg float64) float64 { return deg*math.Pi/180 - math.Pi/2 }

// renderDial draws the dial and returns SVG bytes. markColor is the centre pace
// colour; pass hasMark=false to omit it (error blink "off" frame).
func renderDial(limits map[string]Limit, markColor [3]int, hasMark bool) []byte {
	return renderDialSVG([]dialRenderInput{{limits: limits, markColor: markColor, hasMark: hasMark}})
}

func renderDualDial(claudeLimits map[string]Limit, claudeMark [3]int, claudeHasMark bool,
	codexLimits map[string]Limit, codexMark [3]int, codexHasMark bool) []byte {
	return renderDialSVG([]dialRenderInput{
		{limits: claudeLimits, markColor: claudeMark, hasMark: claudeHasMark},
		{limits: codexLimits, markColor: codexMark, hasMark: codexHasMark},
	})
}

type dialRenderInput struct {
	limits    map[string]Limit
	markColor [3]int
	hasMark   bool
	// full draws a single window as one big full circle instead of the two
	// semicircles — used when only one limit remains (e.g. Codex weekly-only).
	// source names which limits key that circle shows ("session"/"weekly").
	full   bool
	source string
	// credits is the thin edge ring; zero value (Show false) draws nothing.
	credits creditRing
	// pulse is the spend animation for this side; zero value draws nothing.
	pulse pulseFrame
}

func renderDialSVG(inputs []dialRenderInput) []byte {
	if len(inputs) == 0 {
		inputs = []dialRenderInput{{limits: blankLimits}}
	}
	// pad reserves room around each dial for the spend sparks; the gap between
	// two dials grows with it so neighbouring bursts don't collide. Both depend
	// on the pulse mode only, so the icon keeps its width while one plays.
	pad := pulseCanvasPad()
	gap := 2.0 + pad
	n := float64(len(inputs))
	width := 2*pad + 18.0*n + gap*(n-1)
	var b strings.Builder
	b.WriteString(fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %.2f 18">`,
		iround(float64(pxSize)*width/18), pxSize, width))
	for i, input := range inputs {
		renderDialGroup(&b, input, pad+float64(i)*(18+gap))
	}
	b.WriteString(`</svg>`)
	return []byte(b.String())
}

func renderDialGroup(b *strings.Builder, input dialRenderInput, xoff float64) {
	if input.full {
		renderFullGroup(b, input, xoff)
		return
	}
	span := 180.0 - 2*endGap

	// faint tracks, one per ring/side, so the centre gap stays empty
	for _, sign := range sideSign {
		ox := xoff + dialCX + sign*halfShift
		for _, r := range rings {
			writeArc(b, ox, r, sign*endGap, sign*(180-endGap), dialTrack, 0.45, dialSW)
		}
	}

	for _, seg := range segments {
		l := input.limits[seg.source]
		length, health := metricFor(seg.metric, l)
		length = clamp(length, 0, 1)
		if length <= 0.004 {
			continue
		}
		sign := sideSign[seg.side]
		r := rings[seg.ring]
		ox := dialCX + sign*halfShift
		alpha := 0.85
		if seg.bright {
			alpha = 1.0
		}
		// The session quota is really capped by the weekly ("main") quota: one full
		// session costs BlockPct% of the weekly, so the weekly's remaining budget
		// only covers (weeklyLeft / BlockPct) of a session. When that is less than
		// the session budget left, the excess can never be spent — the weekly runs
		// out first. Lay the whole session-left arc down in light grey and paint
		// only the usable head in colour over it, so the tail reads as unavailable.
		if seg.source == "session" && seg.metric == "percent_left" {
			if wl, ok := input.limits["weekly"]; ok && wl.BlockPct > 0 {
				if usable := effectiveLimitLeft(input.limits, "session"); usable < length {
					writeArc(b, xoff+ox, r, sign*endGap, sign*(endGap+length*span),
						dialUnavail, 0.9, dialSW)
					length = usable
				}
			}
		}
		if length <= 0.004 {
			continue
		}
		// length now carries any weekly cap, so it doubles as the effective
		// quota-left the colour reads from — the ring reddens once the usable
		// slice shrinks, even while the raw session % still looks healthy.
		writeArc(b, xoff+ox, r, sign*endGap, sign*(endGap+length*span),
			segColor(seg, l, length, health), alpha, dialSW)
	}

	// Thin separators slicing the weekly (right) semicircle into "one full
	// 5-hour session" blocks. They sit on the outer weekly ring (the quota arc),
	// one every BlockPct of the weekly quota; notches over the lit arc read as
	// "this many full sessions of weekly budget left".
	if wl, ok := input.limits["weekly"]; ok {
		writeBlockTicks(b, xoff, wl.BlockPct)
	}

	// Deterministic time ticks on the inner (time) rings, from the known window
	// lengths: the session/left semicircle by hours, the weekly/right by days.
	if sl, ok := input.limits["session"]; ok {
		writeRingTicks(b, xoff, sideSign["left"], rings["inner"], sl.TimeTicks)
	}
	if wl, ok := input.limits["weekly"]; ok {
		writeRingTicks(b, xoff, sideSign["right"], rings["inner"], wl.TimeTicks)
	}

	writeCreditRing(b, xoff+dialCX, input.credits)

	writePulse(b, xoff+dialCX, input.markColor, input.pulse)

	if input.hasMark {
		writeMark(b, xoff, input.markColor, input.pulse)
	}
}

// renderFullGroup draws one window as a single full circle: the outer quota
// ring (coloured by projected pace), the inner time ring, their ticks, and the
// centre pace dot. Used when only one limit remains (input.source names it).
func renderFullGroup(b *strings.Builder, input dialRenderInput, xoff float64) {
	ox := xoff + dialCX
	l := input.limits[input.source]

	for _, r := range rings { // faint full-circle tracks
		writeCircle(b, ox, r, dialTrack, 0.45)
	}

	limitLeft := effectiveLimitLeft(input.limits, input.source)
	writeSweep(b, ox, rings["outer"], limitLeft,
		quotaPaceColor(limitPace(l, limitLeft)), 1.0)

	tlen, thealth := metricFor("time", l)
	writeSweep(b, ox, rings["inner"], tlen, healthColor(thealth), 0.85)

	writeFullBlockTicks(b, ox, l.BlockPct)
	writeFullRingTicks(b, ox, rings["inner"], l.TimeTicks)
	writeCreditRing(b, ox, input.credits)
	writePulse(b, ox, input.markColor, input.pulse)

	if input.hasMark {
		writeMark(b, xoff, input.markColor, input.pulse)
	}
}

// writeCircle strokes a full ring (no fill) at radius r.
func writeCircle(b *strings.Builder, ox, r float64, c [3]int, alpha float64) {
	writeCircleTagged(b, "", ox, r, c, alpha, dialSW)
}

func writeCircleTagged(b *strings.Builder, attr string, ox, r float64, c [3]int, alpha, width float64) {
	b.WriteString(fmt.Sprintf(
		`<circle%s cx="%.2f" cy="%.2f" r="%.2f" fill="none" stroke="%s" stroke-width="%.2f" stroke-opacity="%.2f"/>`,
		attr, ox, dialCY, r, svgRGB(c), width, alpha))
}

// writeSweep draws frac (0..1) of a full circle, starting at the top and going
// clockwise. A full fraction closes into a circle, since a 360° arc collapses
// to a point.
func writeSweep(b *strings.Builder, ox, r, frac float64, c [3]int, alpha float64) {
	writeSweepTagged(b, "", ox, r, frac, c, alpha, dialSW)
}

func writeSweepTagged(b *strings.Builder, attr string, ox, r, frac float64, c [3]int, alpha, width float64) {
	frac = clamp(frac, 0, 1)
	if frac <= 0.004 {
		return
	}
	if frac >= 0.999 {
		writeCircleTagged(b, attr, ox, r, c, alpha, width)
		return
	}
	writeArcTagged(b, attr, ox, r, 0, frac*360, c, alpha, width)
}

// tagAttr renders an optional data-* marker so tests (and curious readers of
// the SVG) can tell parts of the drawing apart; the tagged writers take the
// finished attribute, so each caller picks its own marker name.
func tagAttr(kind, tag string) string {
	if tag == "" {
		return ""
	}
	return ` data-` + kind + `="` + tag + `"`
}

// writeCreditRing draws the thin credits ring (see creditRing). The ring is
// drawn butt-capped so the day slice sits flush on the cover arc.
func writeCreditRing(b *strings.Builder, ox float64, r creditRing) {
	if !r.Show {
		return
	}
	track, alpha := dialTrack, 0.45
	if r.Empty {
		track, alpha = dialRed, 0.85
	}
	writeCircleTagged(b, tagAttr("credit", "track"), ox, creditR, track, alpha, creditSW)
	if !r.Projected || r.Empty {
		return
	}
	cover := healthColor(r.Cover)
	writeSweepTagged(b, tagAttr("credit", "cover"), ox, creditR, r.Cover, cover, 1.0, creditSW)
	if r.DaySlice > 0.004 {
		writeSweepTagged(b, tagAttr("credit", "day"), ox, creditR, math.Min(r.DaySlice, r.Cover),
			lerpColor(cover, dialWhite, 0.45), 1.0, creditSW)
	}
}

// writeFullRingTicks notches a full-circle ring into n equal segments (n ticks)
// — the whole-circle analogue of writeRingTicks.
func writeFullRingTicks(b *strings.Builder, ox, r float64, n int) {
	if n < 2 {
		return
	}
	writeFullTicks(b, ox, r, func(i int) float64 { return float64(i) / float64(n) * 360 }, n)
}

// writeFullBlockTicks draws a notch at each blockPct boundary of the quota
// around the full outer ring; blockPct <= 0 draws nothing.
func writeFullBlockTicks(b *strings.Builder, ox, blockPct float64) {
	if blockPct <= 0 {
		return
	}
	var degs []float64
	for p := blockPct; p < 100-1e-6; p += blockPct {
		degs = append(degs, p/100*360)
	}
	writeFullTicks(b, ox, rings["outer"], func(i int) float64 { return degs[i] }, len(degs))
}

// writeFullTicks stamps n radial notches across a full-circle ring at the angles
// deg(i) returns (clockwise from the top).
func writeFullTicks(b *strings.Builder, ox, r float64, deg func(int) float64, n int) {
	in, out := r-dialSW/2-0.1, r+dialSW/2+0.1
	for i := 0; i < n; i++ {
		ph := phi(deg(i))
		c, s := math.Cos(ph), math.Sin(ph)
		b.WriteString(fmt.Sprintf(
			`<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="rgb(15,15,15)" stroke-width="0.45" stroke-opacity="0.9"/>`,
			ox+in*c, dialCY+in*s, ox+out*c, dialCY+out*s))
	}
}

// capDeg is the angular overshoot, in degrees, that a round stroke linecap adds
// past an arc endpoint at radius r: the cap reaches half the stroke width beyond
// the path end. Ticks must span this so the first/last visual segments (which
// include the bulging caps) match the interior ones — otherwise the ends read
// fatter than the centre.
func capDeg(r float64) float64 { return (dialSW / 2) / r * 180 / math.Pi }

// writeBlockTicks draws radial notches across the outer weekly ring, one at each
// blockPct boundary of the weekly quota (right semicircle). blockPct <= 0 draws
// nothing.
func writeBlockTicks(b *strings.Builder, xoff, blockPct float64) {
	if blockPct <= 0 {
		return
	}
	const sign = +1.0 // weekly lives on the right side
	r := rings["outer"]
	cap := capDeg(r)
	start := endGap - cap
	span := 180.0 - 2*endGap + 2*cap // visible span, caps included
	ox := xoff + dialCX + sign*halfShift
	in, out := r-dialSW/2-0.1, r+dialSW/2+0.1
	for p := blockPct; p < 100-1e-6; p += blockPct {
		ph := phi(sign * (start + p/100*span))
		c, s := math.Cos(ph), math.Sin(ph)
		b.WriteString(fmt.Sprintf(
			`<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="rgb(15,15,15)" stroke-width="0.45" stroke-opacity="0.9"/>`,
			ox+in*c, dialCY+in*s, ox+out*c, dialCY+out*s))
	}
}

// writeRingTicks notches a ring's semicircle into n equal segments (n-1 internal
// ticks) on the given side — used for the deterministic hour/day time marks. The
// span includes the round-cap overshoot at both ends so all n visual segments
// come out equal.
func writeRingTicks(b *strings.Builder, xoff, sign, r float64, n int) {
	if n < 2 {
		return
	}
	cap := capDeg(r)
	start := endGap - cap
	span := 180.0 - 2*endGap + 2*cap // visible span, caps included
	ox := xoff + dialCX + sign*halfShift
	in, out := r-dialSW/2-0.1, r+dialSW/2+0.1
	for i := 1; i < n; i++ {
		ph := phi(sign * (start + float64(i)/float64(n)*span))
		c, s := math.Cos(ph), math.Sin(ph)
		b.WriteString(fmt.Sprintf(
			`<line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="rgb(15,15,15)" stroke-width="0.45" stroke-opacity="0.9"/>`,
			ox+in*c, dialCY+in*s, ox+out*c, dialCY+out*s))
	}
}

func writeArc(b *strings.Builder, ox, r, startDeg, endDeg float64, c [3]int, alpha, width float64) {
	writeArcTagged(b, "", ox, r, startDeg, endDeg, c, alpha, width)
}

func writeArcTagged(b *strings.Builder, attr string, ox, r, startDeg, endDeg float64, c [3]int, alpha, width float64) {
	sx, sy := arcPoint(ox, r, startDeg)
	ex, ey := arcPoint(ox, r, endDeg)
	large := 0
	if math.Abs(endDeg-startDeg) > 180 {
		large = 1
	}
	sweep := 0
	if endDeg > startDeg {
		sweep = 1
	}
	cap := "round"
	if strings.Contains(attr, `data-credit=`) {
		cap = "butt" // the credits ring parts sit flush against each other
	}
	b.WriteString(fmt.Sprintf(
		`<path%s d="M%.2f,%.2f A%.2f,%.2f 0 %d %d %.2f,%.2f" fill="none" stroke="%s" stroke-width="%.2f" stroke-opacity="%.2f" stroke-linecap="%s"/>`,
		attr, sx, sy, r, r, large, sweep, ex, ey, svgRGB(c), width, alpha, cap))
}

func arcPoint(ox, r, deg float64) (float64, float64) {
	p := phi(deg)
	return ox + r*math.Cos(p), dialCY + r*math.Sin(p)
}

func svgRGB(c [3]int) string {
	return fmt.Sprintf("rgb(%d,%d,%d)", c[0], c[1], c[2])
}

// writeMark draws the centre pace dot: a filled circle matching the worst known
// outer quota window. A spend pulse swells it and flashes it toward white.
func writeMark(b *strings.Builder, xoff float64, c [3]int, p pulseFrame) {
	r, fill := 2.0, c
	if p.Active {
		r *= p.MarkScale
		fill = lerpColor(c, dialWhite, p.MarkFlash)
	}
	b.WriteString(fmt.Sprintf(
		`<circle cx="%.2f" cy="%.2f" r="%.2f" fill="%s"/>`,
		xoff+dialCX, dialCY, r, svgRGB(fill)))
}

// writePulse draws the spend burst around a dial centred at ox: the halo
// rolling out past the quota ring, then the embers in the side padding. Both
// take a lightened pace colour — white alone would vanish on a light menu bar.
func writePulse(b *strings.Builder, ox float64, c [3]int, p pulseFrame) {
	if !p.Active {
		return
	}
	glow := lerpColor(c, dialWhite, 0.15)
	if p.HaloAlpha > 0.01 && p.Halo > 0 {
		halo := tagAttr("pulse", "halo")
		if p.HaloFan <= 0 {
			writeCircleTagged(b, halo, ox, p.Halo, glow, p.HaloAlpha, p.HaloWidth)
		} else {
			for _, mid := range []float64{90, 270} { // the right and left sides
				writeArcTagged(b, halo, ox, p.Halo, mid-p.HaloFan, mid+p.HaloFan,
					glow, p.HaloAlpha, p.HaloWidth)
			}
		}
	}
	for _, s := range p.Sparks {
		if s.Alpha <= 0.01 {
			continue
		}
		b.WriteString(fmt.Sprintf(
			`<circle data-pulse="spark" cx="%.2f" cy="%.2f" r="%.2f" fill="%s" fill-opacity="%.2f"/>`,
			ox+s.X, dialCY+s.Y, s.R, svgRGB(glow), s.Alpha))
	}
}

var blankLimits = map[string]Limit{
	"session": {Pct: 100, TimeLeft: 1},
	"weekly":  {Pct: 100, TimeLeft: 1},
}
