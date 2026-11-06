package main

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/getlantern/systray"
)

const (
	profileEvery   = 60
	cacheTTL       = 600.0
	saveEverySecs  = 3600.0 // steady-state throttle; exit/quit/SIGINT force a flush
	scopedPoolSize = 4

	// Adaptive polling. The usage endpoint rate-limits well below once-per-30s
	// (and shares that budget with Claude Code itself), so we start slow and
	// self-tune: every 429 grows the steady-state interval, a run of clean
	// successes shrinks it back toward the floor.
	basePollSecs      = 120.0 // floor / starting interval
	maxPollSecs       = 600.0 // ceiling (also the backoff cap)
	pollGrowth        = 1.5   // multiply on 429, divide after a success streak
	okStreakToSpeedUp = 3     // clean successes before we poll faster again

	// Menu-bar icon height (points). Default 19, adjustable ±5.
	defaultIconHeight = 19.0
	minIconHeight     = 14.0
	maxIconHeight     = 24.0
)

var planNames = map[string]string{
	"default_claude_max_5x":  "Claude Max 5×",
	"default_claude_max_20x": "Claude Max 20×",
	"claude_max":             "Claude Max",
	"claude_pro":             "Claude Pro",
}

type pollResult struct {
	source               pollSource
	raw                  []byte
	usage                *Usage
	prof                 *Profile
	claudeAuthGeneration uint64
	codexRaw             []byte
	codexUsage           *CodexUsage
	codexErr             error
	err                  error
}

type pollSource uint8

const (
	pollNone pollSource = iota
	pollClaude
	pollCodex
)

func (s pollSource) String() string {
	if s == pollCodex {
		return "Codex"
	}
	return "Claude"
}

type hourItem struct {
	item *systray.MenuItem
	min  int
}

type App struct {
	mu     sync.Mutex
	client *Client
	codex  *CodexClient
	rates  *RateTracker
	cRates *RateTracker
	calib  *WeeklyCalibrator // learns weekly-% per 5-hour session, for dial blocks
	// per-model calibrators, keyed by model display name (the "Weekly by model"
	// rows): each learns weekly-% per session for its own scoped weekly limit.
	calibByModel map[string]*WeeklyCalibrator
	// per-series log of raw utilisation readings, for the burn-history charts.
	// Keyed by seriesClaude*/seriesCodex*; persisted in the state file.
	history map[string][]histSample
	// last quota reading seen in THIS process, per series: a step up fires the
	// side's spend pulse. Not persisted — a restart must not flash the dial for
	// spend that happened while the app was down.
	lastPct map[string]float64
	// in-flight spend bursts, one per dial side, and whether the last painted
	// icon carried a burst frame (so the loop can paint one clean frame once it
	// has finished).
	claudePulse, codexPulse spendPulse
	pulsePainted            bool
	// overlayUp tracks the spark window around the icon, and sideMark the pace
	// colour each dial was last painted in — the sparks fly in that colour.
	overlayUp bool
	sideMark  [2][3]int

	// runtime state
	profile          *Profile
	pollCount        int
	errorOn          bool
	codexErrorOn     bool
	blink            bool
	cached           bool
	lastOk           time.Time
	lastFail         time.Time
	lastFailMsg      string
	failCount        int
	pollInterval     float64 // adaptive steady-state poll interval (seconds)
	okStreak         int     // consecutive successes since the last 429
	nextPoll         time.Time
	nextCodexPoll    time.Time
	nextAllowedEpoch float64
	lastSave         time.Time
	inflight         bool
	reqStart         time.Time
	reqSource        pollSource
	lastReqSource    pollSource
	results          chan pollResult
	iconKey          string
	// Incremented when the user explicitly reloads Claude Code credentials, so
	// a request already in flight cannot restore the previous account's data.
	claudeAuthGeneration uint64

	// menu-bar icon height in points (manual setting, persisted). dynIconOff
	// disables the burn-driven auto-size (which is on by default — opt-out);
	// appliedIconHeight is the height currently pushed to systray.
	iconHeight float64
	dynIconOff bool
	// weekSmooth is the 7d history row's blur width key (see
	// weekSmoothOptions; "off" = plain bars). The 1h and 5h rows are always
	// smoothed relative to their horizon. Persisted.
	weekSmooth        string
	appliedIconHeight float64

	// working hours (local minutes from midnight) and last data
	workStart    int
	workEnd      int
	lastUsageRaw []byte
	lastUsage    *Usage
	sessRate     float64
	weekRate     float64
	rateSpan     float64 // seconds the recent Claude rate was measured over

	lastCodexRaw     []byte
	lastCodex        *CodexUsage
	lastCodexOk      time.Time
	lastCodexFail    time.Time
	lastCodexFailMsg string
	codexFailCount   int
	codexPrimaryRate float64
	codexSecondRate  float64
	codexRateSpan    float64 // seconds the recent Codex rate was measured over

	statePath string

	// menu items
	claudeParent                               *systray.MenuItem
	account, claudeSwitchAuth, plan            *systray.MenuItem
	claudeRefreshNow                           *systray.MenuItem
	session, sessionReset, weekly, weeklyReset *systray.MenuItem
	weeklyBlocks, weeklyBlocksBasis            *systray.MenuItem
	claudeCredits, claudeCreditsRunway         *systray.MenuItem
	claudeResets                               *systray.MenuItem
	burn, burnSession, burnWeekly              *systray.MenuItem
	speed, speedLeft, speedAvg, speedAvgLeft   *systray.MenuItem
	histHeader                                 *systray.MenuItem
	histHour, histSession, histWeek            *systray.MenuItem
	histCredits                                *systray.MenuItem
	scopedParent                               *systray.MenuItem
	scoped                                     []*systray.MenuItem
	codexParent                                *systray.MenuItem
	codexAccount, codexSwitchAuth, codexPlan   *systray.MenuItem
	codexPrimary, codexPrimaryReset            *systray.MenuItem
	codexSecondary, codexSecondaryReset        *systray.MenuItem
	codexBurn, codexBurnPrimary                *systray.MenuItem
	codexBurnSecondary                         *systray.MenuItem
	codexSpeed, codexSpeedLeft                 *systray.MenuItem
	codexSpeedAvg, codexSpeedAvgLeft           *systray.MenuItem
	codexCredits, codexCreditsRunway           *systray.MenuItem
	codexResets                                *systray.MenuItem
	codexScopedParent                          *systray.MenuItem
	codexScoped                                []*systray.MenuItem
	legendParent                               *systray.MenuItem
	legendItems                                []*systray.MenuItem
	timeFillParent                             *systray.MenuItem
	weekSmoothParent                           *systray.MenuItem
	weekSmoothItems                            []*systray.MenuItem
	timeFillRemaining, timeFillElapsed         *systray.MenuItem
	pulseParent                                *systray.MenuItem
	pulseItems                                 []*systray.MenuItem
	pulseTest                                  *systray.MenuItem
	iconSizeParent                             *systray.MenuItem
	iconSizeItems                              []hourItem
	dynIconItem                                *systray.MenuItem
	work                                       *systray.MenuItem
	startItems, endItems                       []hourItem
	status, nextReset                          *systray.MenuItem
	claudeOk, claudeFail, codexOk, codexFail   *systray.MenuItem
	// base block titles ("Claude · Max 20×"); the nearest-reset suffix is
	// appended by refreshResetRows, which only touches systray when the
	// composed text changes.
	claudeTitle, codexTitle           string
	claudeTitleShown, codexTitleShown string
	nextResetShown                    string
}

func newApp() *App {
	a := &App{
		client:       &Client{},
		codex:        &CodexClient{},
		rates:        &RateTracker{},
		cRates:       &RateTracker{},
		calib:        &WeeklyCalibrator{},
		calibByModel: map[string]*WeeklyCalibrator{},
		history:      map[string][]histSample{},
		lastPct:      map[string]float64{},
		results:      make(chan pollResult, 1),
		workStart:    9 * 60,
		workEnd:      18 * 60,
		weekSmooth:   weekSmoothDefault,
		iconHeight:   defaultIconHeight,
		pollInterval: basePollSecs,
		lastSave:     time.Time{},
		statePath:    defaultStatePath(),
	}
	a.loadState()
	return a
}

func defaultStatePath() string {
	dir := "."
	if exe, err := os.Executable(); err == nil {
		dir = filepath.Dir(exe)
	}
	return filepath.Join(dir, ".poll-state.json")
}

// --- lifecycle --------------------------------------------------------------

func (a *App) onReady() {
	systray.SetTitle("…")
	systray.SetTooltip("Claude and Codex limits")
	a.applyIconHeight()
	a.buildMenu()
	a.renderHistory()
	if a.lastUsage != nil {
		a.apply()
	} else {
		a.renderIcon()
	}
	if a.profile != nil {
		a.renderProfile() // restore account/plan immediately
	}
	a.renderCodex()
	go a.tickLoop()
	go a.blinkLoop()
	go a.pulseLoop()
	go a.signalLoop()
}

func (a *App) onExit() {
	a.mu.Lock()
	a.saveState(true)
	a.mu.Unlock()
}

func (a *App) tickLoop() {
	for range time.Tick(time.Second) {
		a.mu.Lock()
		a.drain()
		if !a.inflight {
			a.startDueRequest()
		}
		a.updateStatus()
		a.mu.Unlock()
	}
}

func (a *App) blinkLoop() {
	for range time.Tick(500 * time.Millisecond) {
		a.mu.Lock()
		if a.errorOn || a.codexErrorOn {
			a.blink = !a.blink
			a.renderIcon()
		}
		a.mu.Unlock()
	}
}

// pulseLoop animates the spend bursts. It only paints while a burst is running,
// plus one frame after the last one ends to clear the halo and embers.
func (a *App) pulseLoop() {
	for range time.Tick(time.Duration(pulseFrameSecs * float64(time.Second))) {
		a.mu.Lock()
		now := time.Now()
		if a.pulseRunning(now) {
			a.pulsePainted = true
			a.renderIcon()
			a.paintPulseOverlay(now)
		} else if a.pulsePainted {
			a.pulsePainted = false
			a.renderIcon()
			a.clearPulseOverlay()
		}
		a.mu.Unlock()
	}
}

// firePulseTest plays a full-strength burst on both dials, Codex a beat behind
// Claude, so the menu's "Test it" row shows what spend looks like without
// waiting for the next poll to find some.
func (a *App) firePulseTest() {
	now := time.Now()
	a.claudePulse = spendPulse{At: now, Mag: 1, Seed: now.UnixNano()}
	a.codexPulse = spendPulse{At: now.Add(pulseTestStagger), Mag: 1, Seed: now.UnixNano() + 1}
	a.renderIcon()
}

// pulseRunning reports whether either side has a burst to draw right now.
func (a *App) pulseRunning(now time.Time) bool {
	if pulseFX == pulseOff {
		return false
	}
	_, c := a.claudePulse.progress(now)
	_, x := a.codexPulse.progress(now)
	// A burst armed for a moment still ahead (the staggered test) has to keep
	// the loop awake until it starts.
	return c || x || now.Before(a.codexPulse.At)
}

// --- polling ----------------------------------------------------------------

func (a *App) nextDueRequest(now time.Time) pollSource {
	if a.inflight {
		return pollNone
	}
	claudeDue := !now.Before(a.nextPoll)
	codexDue := !now.Before(a.nextCodexPoll)
	switch {
	case claudeDue && codexDue:
		// Alternate when both are waiting so a permanently failing provider can
		// never monopolise the single request slot.
		if a.lastReqSource == pollClaude {
			return pollCodex
		}
		return pollClaude
	case claudeDue:
		return pollClaude
	case codexDue:
		return pollCodex
	default:
		return pollNone
	}
}

func (a *App) startDueRequest() {
	if source := a.nextDueRequest(time.Now()); source != pollNone {
		a.startRequest(source)
	}
}

// startRequest starts exactly one provider request. Claude and Codex have
// independent schedules, but share this single in-flight slot so their HTTP
// calls never overlap.
func (a *App) startRequest(source pollSource) {
	if a.inflight || source == pollNone {
		return
	}
	a.inflight = true
	a.reqStart = time.Now()
	a.reqSource = source
	a.lastReqSource = source
	results := a.results

	if source == pollCodex {
		codex := a.codex
		go func() {
			raw, usage, err := codex.Usage()
			results <- pollResult{
				source:     pollCodex,
				codexRaw:   raw,
				codexUsage: usage,
				codexErr:   err,
			}
		}()
		return
	}

	a.pollCount++
	wantProfile := a.profile == nil || a.pollCount%profileEvery == 0
	client := a.client
	claudeAuthGeneration := a.claudeAuthGeneration
	go func() {
		raw, usage, err := client.Usage()
		var prof *Profile
		if err == nil && wantProfile {
			prof, _ = client.Profile()
		}
		results <- pollResult{
			source:               pollClaude,
			raw:                  raw,
			usage:                usage,
			prof:                 prof,
			claudeAuthGeneration: claudeAuthGeneration,
			err:                  err,
		}
	}()
}

func (a *App) drain() {
	select {
	case r := <-a.results:
		a.inflight = false
		a.reqSource = pollNone
		if r.source == pollCodex {
			a.noteCodex(r)
		} else if r.claudeAuthGeneration != a.claudeAuthGeneration {
			// The Claude half belongs to the login active before the user clicked
			// "Use current Claude login". Discard it and request the newly selected
			// Claude account on the next tick.
			a.nextPoll = time.Time{}
			a.nextAllowedEpoch = 0
		} else if r.err != nil {
			a.noteFail(r.err)
		} else {
			a.onSuccess(r)
		}
		// A rotated refresh token must be saved promptly — losing it would force
		// a re-bootstrap (Codex keychain prompt / Claude re-read) next launch.
		codexDirty := a.codex.TakeRefreshDirty()
		codexAccountDirty := a.codex.TakeAccountDirty()
		claudeDirty := a.client.TakeRefreshDirty()
		if codexDirty || codexAccountDirty || claudeDirty {
			a.saveState(true)
		}
	default:
	}
}

// refreshClaudeTokenNow forces a self-refresh of the Claude token (and the
// keychain write-back) without waiting for the current token to expire — a
// way to exercise the refresh path on demand. The network call runs off the
// lock so the tick loop keeps going; the outcome lands in the status row and
// triggers an immediate poll.
func (a *App) refreshClaudeTokenNow() {
	a.status.SetTitle("⟳ refreshing Claude token…")
	client := a.client
	go func() {
		err := client.ForceRefresh()
		a.mu.Lock()
		defer a.mu.Unlock()
		if err != nil {
			a.lastFail = time.Now()
			a.lastFailMsg = describe(err)
			a.status.SetTitle("⚠ " + a.lastFailMsg)
			return
		}
		a.status.SetTitle("✓ Claude token refreshed")
		a.saveState(true) // persist the rotated refresh token promptly
		a.nextPoll = time.Time{}
		a.nextAllowedEpoch = 0
		if !a.inflight {
			a.startRequest(pollClaude)
		}
	}()
}

func (a *App) useCurrentClaudeLogin() {
	if err := a.client.UseCurrentAuth(); err != nil {
		a.lastFail = time.Now()
		a.lastFailMsg = describe(err)
		a.status.SetTitle("⚠ " + a.lastFailMsg)
		return
	}

	a.claudeAuthGeneration++
	a.profile = nil
	a.lastUsageRaw = nil
	a.lastUsage = nil
	a.lastOk = time.Time{}
	a.lastFailMsg = ""
	a.sessRate = 0
	a.weekRate = 0
	a.rates = &RateTracker{}
	a.calib = &WeeklyCalibrator{}
	a.calibByModel = map[string]*WeeklyCalibrator{}
	delete(a.history, seriesClaudeSession)
	delete(a.history, seriesClaudeWeekly)
	delete(a.history, seriesClaudeCredits)
	a.errorOn = false
	a.cached = false
	a.failCount = 0
	a.renderClaudeEmpty("requesting…")
	a.renderHistory()
	a.renderIcon()
	a.saveState(true)
	a.nextPoll = time.Time{}
	a.nextAllowedEpoch = 0
	if !a.inflight {
		a.startRequest(pollClaude)
	}
}

func (a *App) noteCodex(r pollResult) {
	if r.codexErr != nil {
		a.lastCodexFail = time.Now()
		a.lastCodexFailMsg = describe(r.codexErr)
		a.codexFailCount++
		status, retryAfter, resetAt := 0, 0, ""
		if ae, ok := r.codexErr.(*APIError); ok {
			status, retryAfter, resetAt = ae.Status, ae.RetryAfter, ae.ResetAt
		}
		a.scheduleCodexNext(backoffSeconds(a.codexFailCount, status, retryAfter,
			resetAt, rand.Float64()*0.25))
		// Keep the last good dial while it's still fresh; otherwise blink red.
		fresh := !a.lastCodexOk.IsZero() && time.Since(a.lastCodexOk).Seconds() < cacheTTL
		a.codexErrorOn = !fresh
		a.renderCodex()
		a.renderIcon()
		return
	}
	if r.codexUsage == nil {
		return
	}
	a.lastCodexOk = time.Now()
	a.codexFailCount = 0
	a.scheduleCodexNext(basePollSecs)
	a.codexErrorOn = false
	if primary, secondary, ok := codexPercents(r.codexUsage); ok {
		if p, s, ok := a.cRates.Update(nowSecs(), primary, secondary); ok {
			a.codexPrimaryRate, a.codexSecondRate = p, s
			a.codexRateSpan = a.cRates.Span()
		}
		a.recordHistory(seriesCodexPrimary, primary)
		a.recordHistory(seriesCodexSecondary, secondary)
	}
	a.recordCodexCreditSpend(codexCredits(a.lastCodex), codexCredits(r.codexUsage))
	a.lastCodexRaw = r.codexRaw
	a.lastCodex = r.codexUsage
	a.renderCodex()
	a.renderIcon()
}

func (a *App) useCurrentCodexLogin() {
	if _, err := a.codex.UseCurrentAuth(); err != nil {
		a.lastCodexFail = time.Now()
		a.lastCodexFailMsg = describe(err)
		a.renderCodex()
		a.saveState(true)
		return
	}
	a.lastCodexRaw = nil
	a.lastCodex = nil
	a.lastCodexOk = time.Time{}
	a.lastCodexFailMsg = ""
	a.codexPrimaryRate = 0
	a.codexSecondRate = 0
	a.cRates = &RateTracker{}
	delete(a.history, seriesCodexPrimary)
	delete(a.history, seriesCodexSecondary)
	delete(a.history, seriesCodexCredits)
	a.codexErrorOn = false
	a.renderCodex()
	a.renderIcon()
	a.saveState(true)
	a.nextCodexPoll = time.Time{}
	if !a.inflight {
		a.startRequest(pollCodex)
	}
}

func (a *App) onSuccess(r pollResult) {
	a.errorOn = false
	a.cached = false
	a.failCount = 0
	a.lastOk = time.Now()
	// A run of clean successes earns a faster poll, down to the floor.
	a.okStreak++
	if a.okStreak >= okStreakToSpeedUp && a.pollInterval > basePollSecs {
		a.pollInterval = math.Max(basePollSecs, a.pollInterval/pollGrowth)
		a.okStreak = 0
	}
	a.scheduleNext(a.pollInterval)

	if s, w, ok := a.rates.Update(nowSecs(), r.usage.FiveHour.Utilization,
		r.usage.SevenDay.Utilization); ok {
		a.sessRate, a.weekRate = s, w
		a.rateSpan = a.rates.Span()
	}
	if r.usage.HasSession() {
		// Session-based bookkeeping is meaningless on a weekly-only account.
		a.calib.Observe(r.usage.FiveHour.Utilization, r.usage.SevenDay.Utilization)
		a.recordHistory(seriesClaudeSession, r.usage.FiveHour.Utilization)
	}
	a.recordHistory(seriesClaudeWeekly, r.usage.SevenDay.Utilization)
	if b := claudeCredits(r.usage); b.Known && b.UsedKnown {
		a.recordHistory(seriesClaudeCredits, b.Used)
	}
	for _, l := range r.usage.Limits {
		if l.Kind != "weekly_scoped" || l.Scope == nil {
			continue
		}
		name := scopedModelName(l)
		if skipCalibModel(name) || !r.usage.HasSession() {
			continue
		}
		a.modelCalib(name).Observe(r.usage.FiveHour.Utilization, l.Percent)
	}
	a.lastUsageRaw = r.raw
	a.lastUsage = r.usage
	if r.prof != nil {
		a.profile = r.prof
	}
	a.apply()
	a.renderProfile()
	a.saveState(false)
}

func (a *App) noteFail(err error) {
	a.lastFail = time.Now()
	a.lastFailMsg = describe(err)
	a.failCount++
	a.okStreak = 0
	status, retryAfter, resetAt := 0, 0, ""
	if ae, ok := err.(*APIError); ok {
		status, retryAfter, resetAt = ae.Status, ae.RetryAfter, ae.ResetAt
	}
	// A 429 means we're polling too fast — grow the steady-state interval so we
	// keep the slower pace after recovery instead of snapping back to the floor.
	if status == 429 && !isRefreshError(err) {
		a.pollInterval = math.Min(maxPollSecs, a.pollInterval*pollGrowth)
	}
	backoff := backoffSeconds(a.failCount, status, retryAfter, resetAt, rand.Float64()*0.25)
	a.scheduleNext(backoff)

	fresh := !a.lastOk.IsZero() && time.Since(a.lastOk).Seconds() < cacheTTL
	if fresh {
		a.errorOn = false // keep the last good dial on screen
		a.cached = true
	} else {
		a.cached = false
		a.showError(fmt.Sprintf("%s · retry %ds", a.lastFailMsg, int(backoff)))
	}
	a.saveState(false)
}

// isRefreshError reports a failure of the token-refresh endpoint rather than
// the usage API, so its 429 doesn't slow down usage polling.
func isRefreshError(err error) bool {
	ae, ok := err.(*APIError)
	return ok && ae.Refresh
}

func describe(err error) string {
	ae, ok := err.(*APIError)
	if !ok {
		return err.Error()
	}
	switch {
	case ae.Refresh:
		return ae.Msg
	case ae.Status == 429:
		return "Rate-limited (429)"
	case ae.Status == 401 || ae.Status == 403:
		return "Auth expired — open Claude Code to refresh the token"
	case ae.Status != 0:
		return fmt.Sprintf("Usage API error %d", ae.Status)
	}
	return ae.Msg
}

// --- rendering --------------------------------------------------------------

func (a *App) apply() {
	u := a.lastUsage
	if u == nil {
		return
	}
	sessPct, weekPct := u.FiveHour.Utilization, u.SevenDay.Utilization
	sessLeft, sessKnown := secondsUntil(u.FiveHour.ResetsAt)
	weekLeft, weekKnown := secondsUntil(u.SevenDay.ResetsAt)

	now := time.Now()
	sessWork, sw := workingSecondsUntil(u.FiveHour.ResetsAt, a.workStart, a.workEnd, now)
	weekWork, ww := workingSecondsUntil(u.SevenDay.ResetsAt, a.workStart, a.workEnd, now)
	sessFcRate := forecastRate(a.sessRate, sessPct, sessLeft, fiveHourSecs, sessKnown)
	weekFcRate := forecastRate(a.weekRate, weekPct, weekLeft, weekSecs, weekKnown)
	sessFc := projectForecast(sessPct, sessFcRate, sessWork, sw)
	weekFc := projectForecast(weekPct, weekFcRate, weekWork, ww)
	worst := sessFc
	if weekFc.Pos() > worst.Pos() || !u.HasSession() {
		worst = weekFc
	}

	a.renderIcon()
	systray.SetTitle("")
	a.claudeResets.SetTitle(claudeResetsText(u))

	weekReset, weekResetOk := parseISO(u.SevenDay.ResetsAt)
	if !u.HasSession() {
		a.renderClaudeWeeklyOnly(weekPct, weekLeft, weekKnown, weekWork, ww, weekFc, sinceWeekRate(weekPct, weekLeft, weekKnown))
		a.weekly.SetTitle(fmt.Sprintf("📅 Weekly:  %.0f%% left", 100-weekPct))
		a.weekly.SetIcon(renderPctBar(weekPct, fmt.Sprintf("Weekly %.0f%%", weekPct)))
		a.weeklyReset.SetTitle("      resets in " +
			withClock(fmtRemaining(weekLeft, weekKnown), weekReset, weekResetOk))
		a.renderClaudeCredits()
		a.renderScoped(u.Limits)
		a.renderHistory()
		a.status.SetTitle(fmt.Sprintf("✓ week %.0f%% left · no 5h limit", 100-weekPct))
		return
	}
	a.setClaudeSessionRows(true)
	sessReset, sessResetOk := parseISO(u.FiveHour.ResetsAt)
	a.session.SetTitle("🕐 Session (5h):  " + a.sessionLeftText(sessPct, weekPct))
	a.session.SetIcon(renderPctBar(sessPct, fmt.Sprintf("Session %.0f%%", sessPct)))
	a.sessionReset.SetTitle("      resets in " +
		withClock(fmtRemaining(sessLeft, sessKnown), sessReset, sessResetOk))
	a.weekly.SetTitle(fmt.Sprintf("📅 Weekly:  %.0f%% left", 100-weekPct))
	a.weekly.SetIcon(renderPctBar(weekPct, fmt.Sprintf("Weekly %.0f%%", weekPct)))
	// The reset line was overlong once the 5h-block summary was appended; it now
	// spans three rows: the reset clock, the blocks-left figure, and its basis.
	a.weeklyReset.SetTitle("      resets in " +
		withClock(fmtRemaining(weekLeft, weekKnown), weekReset, weekResetOk))
	blocks, basis := a.weekBlocksLines(weekPct)
	a.weeklyBlocks.SetTitle("      " + blocks)
	a.weeklyBlocksBasis.SetTitle("      " + basis)

	avgSessH, asOk := avgRate(sessPct, sessLeft, fiveHourSecs, 3600, sessKnown)
	avgWeekD, awOk := avgRate(weekPct, weekLeft, weekSecs, 86400, weekKnown)
	sinceSess, _ := avgRate(sessPct, sessLeft, fiveHourSecs, 1, sessKnown)
	sinceWeek, _ := avgRate(weekPct, weekLeft, weekSecs, 1, weekKnown)
	a.speed.SetTitle(fmt.Sprintf("⚡ Last %s:  sess %.1f%%/h · week %.1f%%/d",
		spanLabel(a.rateSpan), a.sessRate*3600, a.weekRate*86400))
	a.speedLeft.SetTitle(fmt.Sprintf("      ↳ spare at reset:  sess %s · week %s",
		spareText(sessPct, a.sessRate, sessWork, sw),
		spareText(weekPct, a.weekRate, weekWork, ww)))
	a.speedAvg.SetTitle(fmt.Sprintf("⚡ Since reset:  sess %s%%/h · week %s%%/d",
		fmtRate(avgSessH, asOk), fmtRate(avgWeekD, awOk)))
	a.speedAvgLeft.SetTitle(fmt.Sprintf("      ↳ spare at reset:  sess %s · week %s",
		spareText(sessPct, sinceSess, sessWork, sw),
		spareText(weekPct, sinceWeek, weekWork, ww)))
	a.burn.SetTitle("Forecast @ rate · " + worst.Category)
	a.burnSession.SetTitle("Session (5h):  " + sessFc.Text())
	a.burnWeekly.SetTitle("Weekly:  " + weekFc.Text())

	a.renderClaudeCredits()
	a.renderScoped(u.Limits)
	a.renderHistory()
	a.status.SetTitle(fmt.Sprintf("✓ sess %.0f%% left · week %.0f%% left",
		100-sessPct, 100-weekPct))
}

// sinceWeekRate is the average weekly consumption since the window opened,
// in %/s (0 when not measurable).
func sinceWeekRate(weekPct, weekLeft float64, known bool) float64 {
	r, _ := avgRate(weekPct, weekLeft, weekSecs, 1, known)
	return r
}

// setClaudeSessionRows shows or hides every menu row that only makes sense
// with a 5-hour window: the session line and its reset, the 5h-block
// calibration lines, and the session forecast.
func (a *App) setClaudeSessionRows(show bool) {
	for _, it := range []*systray.MenuItem{a.session, a.sessionReset,
		a.weeklyBlocks, a.weeklyBlocksBasis, a.burnSession} {
		if it == nil {
			continue
		}
		if show {
			it.Show()
		} else {
			it.Hide()
		}
	}
}

// renderClaudeWeeklyOnly fills the Claude block for an account whose usage
// response carries no 5-hour window: session rows are hidden and the speed,
// spare and forecast lines speak only about the weekly limit.
func (a *App) renderClaudeWeeklyOnly(weekPct, weekLeft float64, weekKnown bool,
	weekWork float64, ww bool, weekFc Forecast, sinceWeek float64) {
	a.setClaudeSessionRows(false)
	avgWeekD, awOk := avgRate(weekPct, weekLeft, weekSecs, 86400, weekKnown)
	a.speed.SetTitle(fmt.Sprintf("⚡ Last %s:  week %.1f%%/d", spanLabel(a.rateSpan), a.weekRate*86400))
	a.speedLeft.SetTitle("      ↳ spare at reset:  week " + spareText(weekPct, a.weekRate, weekWork, ww))
	a.speedAvg.SetTitle("⚡ Since reset:  week " + fmtRate(avgWeekD, awOk) + "%/d")
	a.speedAvgLeft.SetTitle("      ↳ spare at reset:  week " + spareText(weekPct, sinceWeek, weekWork, ww))
	a.burn.SetTitle("Forecast @ rate · " + weekFc.Category)
	a.burnWeekly.SetTitle("Weekly:  " + weekFc.Text())
}

func (a *App) renderCodex() {
	if a.codexParent == nil {
		return
	}
	u := a.lastCodex
	if u == nil {
		a.renderCodexEmpty()
		return
	}
	primary := u.primary()
	secondary := u.secondary()
	plan := codexPlanLabel(u.PlanType)
	account := a.codex.AccountLabel()
	if account == "" {
		account = "—"
	}
	a.setCodexTitle("Codex · " + plan)
	a.codexAccount.SetTitle("Account: " + account)
	a.codexPlan.SetTitle("Plan: " + plan)
	a.renderCodexWindow(a.codexPrimary, a.codexPrimaryReset, "Primary", primary)
	a.renderCodexWindow(a.codexSecondary, a.codexSecondaryReset, "Secondary", secondary)

	primaryFc := a.codexForecast(primary, a.codexPrimaryRate)
	secondaryFc := a.codexForecast(secondary, a.codexSecondRate)
	worst := primaryFc
	if secondaryFc.Pos() > worst.Pos() {
		worst = secondaryFc
	}
	a.codexBurn.SetTitle("Forecast @ rate · " + worst.Category)
	a.codexBurnPrimary.SetTitle("Primary:  " + primaryFc.Text())
	a.codexBurnSecondary.SetTitle("Secondary:  " + secondaryFc.Text())

	a.codexSpeed.SetTitle(fmt.Sprintf("Last %s:  primary %s · secondary %s",
		spanLabel(a.codexRateSpan), codexRateText(primary, a.codexPrimaryRate, true), codexRateText(secondary, a.codexSecondRate, true)))
	a.codexSpeedLeft.SetTitle(fmt.Sprintf("      ↳ spare at reset:  primary %s · secondary %s",
		a.codexSpare(primary, a.codexPrimaryRate), a.codexSpare(secondary, a.codexSecondRate)))
	a.codexSpeedAvg.SetTitle(fmt.Sprintf("Since reset:  primary %s · secondary %s",
		a.codexAvgRate(primary), a.codexAvgRate(secondary)))
	a.codexSpeedAvgLeft.SetTitle(fmt.Sprintf("      ↳ spare at reset:  primary %s · secondary %s",
		a.codexSpare(primary, codexSinceRate(primary)), a.codexSpare(secondary, codexSinceRate(secondary))))
	a.renderCodexCredits()
	a.codexResets.SetTitle(codexResetsText(u))
	a.renderCodexScoped(u.Limits)
	a.renderHistory()
}

func (a *App) renderCodexEmpty() {
	a.setCodexTitle("Codex · —")
	account := a.codex.AccountLabel()
	if account == "" {
		account = "—"
	}
	a.codexAccount.SetTitle("Account: " + account)
	a.codexPlan.SetTitle("Plan: —")
	a.codexPrimary.SetTitle("Primary:  —")
	a.codexPrimaryReset.SetTitle("      resets in —")
	a.codexSecondary.SetTitle("Secondary:  —")
	a.codexSecondaryReset.SetTitle("      resets in —")
	a.codexBurn.SetTitle("Forecast @ rate · —")
	a.codexBurnPrimary.SetTitle("Primary:  —")
	a.codexBurnSecondary.SetTitle("Secondary:  —")
	a.codexSpeed.SetTitle("Last 15m:  —")
	a.codexSpeedLeft.SetTitle("      ↳ spare at reset:  —")
	a.codexSpeedAvg.SetTitle("Since reset:  —")
	a.codexSpeedAvgLeft.SetTitle("      ↳ spare at reset:  —")
	a.renderCodexCredits()
	a.codexResets.SetTitle("Resets available: —")
	a.renderCodexScoped(nil)
}

func (a *App) codexDial() (dialRenderInput, string) {
	u := a.lastCodex
	if u == nil {
		return dialRenderInput{limits: blankLimits}, "codex-empty"
	}
	primary := u.primary()
	secondary := u.secondary()
	sess := a.codexLimitForDial(primary)
	week := a.codexLimitForDial(secondary)
	if primary != nil {
		sess.TimeTicks = codexTimeTicks(primary.WindowSeconds)
	}
	if secondary != nil {
		week.TimeTicks = codexTimeTicks(secondary.WindowSeconds)
	}
	limits := map[string]Limit{"session": sess, "weekly": week}
	pace := aggregateDialPace(limits)
	mark := quotaPaceColor(pace)

	// Only one window left (e.g. Codex now hands out only a weekly limit) — draw
	// it as one big full circle instead of a half-empty pair of semicircles.
	full := (primary == nil) != (secondary == nil)
	source := "session"
	if primary == nil {
		source = "weekly"
	}
	ring := a.codexCreditRing()
	key := fmt.Sprintf("codex-%d-%d-%d-%d-%d-%d-%d-%d-%v-%s-%s-%s",
		iround(limitPct(limits["session"])), iround(limitPct(limits["weekly"])),
		iround(limits["session"].TimeLeft*48), iround(limits["weekly"].TimeLeft*48),
		limits["session"].TimeTicks, limits["weekly"].TimeTicks,
		int(pace), int(timeFill), full, source, dialPaceKey(limits), ring.key())
	return dialRenderInput{limits: limits, markColor: mark, hasMark: true, full: full, source: source, credits: ring}, key
}

func (a *App) codexLimitForDial(w *CodexWindow) Limit {
	if w == nil {
		return Limit{Pct: 100, TimeLeft: 1}
	}
	window := float64(w.WindowSeconds)
	reset, ok := codexResetAt(w)
	paceLeft, paceWindow := a.paceClock(reset, ok, window)
	return Limit{
		Pct:          w.UsedPercent,
		TimeLeft:     codexTimeFraction(w),
		WindowSecs:   window,
		WaitOKSecs:   paceWaitOK(window),
		PaceLeftSecs: paceLeft, PaceWindowSecs: paceWindow,
	}
}

// paceClock is the working-hours clock a window's pace is projected on (see
// limitPace): the working seconds left to its reset and in the whole window.
// Both are 0 when the reset is unknown, which leaves the dial on the wall
// clock.
func (a *App) paceClock(reset time.Time, ok bool, windowSecs float64) (left, window float64) {
	if !ok || windowSecs <= 0 {
		return 0, 0
	}
	start := reset.Add(-time.Duration(windowSecs * float64(time.Second)))
	return workingSecondsBetween(time.Now(), reset, a.workStart, a.workEnd),
		workingSecondsBetween(start, reset, a.workStart, a.workEnd)
}

// paceWaitOK applies the statusline's two tolerances to API windows. Claude
// and Codex currently expose short (~5h) and long (~7d) windows; using one day
// as the boundary also keeps an unexpected short window from getting a 12h
// tolerance larger than the window itself.
func paceWaitOK(windowSecs float64) float64 {
	if windowSecs >= 24*60*60 {
		return 12 * 60 * 60
	}
	return 30 * 60
}

// codexTimeTicks derives the inner-ring unit from the actual API window rather
// than from whether Codex called it primary or secondary. Codex can expose its
// only weekly window as primary; that full circle must still show seven days,
// not 168 hours clamped to 24 marks.
func codexTimeTicks(windowSeconds int64) int {
	if windowSeconds >= 24*60*60 {
		return dayTicks(float64(windowSeconds))
	}
	return hourTicks(float64(windowSeconds))
}

func codexTimeFraction(w *CodexWindow) float64 {
	if w == nil || w.ResetsAt <= 0 || w.WindowSeconds <= 0 {
		return 0
	}
	return clamp(time.Until(time.Unix(w.ResetsAt, 0)).Seconds()/float64(w.WindowSeconds), 0, 1)
}

func limitPct(l Limit) float64 { return l.Pct }

func (a *App) renderCodexWindow(row, reset *systray.MenuItem, name string, w *CodexWindow) {
	if w == nil {
		row.SetTitle(name + ":  —")
		reset.SetTitle("      resets in —")
		return
	}
	left, known := codexSecondsUntil(w)
	t, tok := codexResetAt(w)
	row.SetTitle(fmt.Sprintf("%s:  %.0f%% left", name, 100-w.UsedPercent))
	reset.SetTitle("      resets in " + withClock(fmtRemaining(left, known), t, tok))
}

// codexResetAt is a window's absolute reset moment (from its epoch field).
func codexResetAt(w *CodexWindow) (time.Time, bool) {
	if w == nil || w.ResetsAt <= 0 {
		return time.Time{}, false
	}
	return time.Unix(w.ResetsAt, 0), true
}

func (a *App) codexForecast(w *CodexWindow, rate float64) Forecast {
	if w == nil {
		return projectForecast(0, 0, 0, false)
	}
	reset := time.Unix(w.ResetsAt, 0).Format(time.RFC3339)
	work, known := workingSecondsUntil(reset, a.workStart, a.workEnd, time.Now())
	left, lknown := codexSecondsUntil(w)
	rate = forecastRate(rate, w.UsedPercent, left, float64(w.WindowSeconds), lknown)
	return projectForecast(w.UsedPercent, rate, work, known)
}

// codexRateUnit is the display unit for a window's rates, by its length: a
// daily-or-longer window reads in %/d like the Claude weekly rows, a shorter
// one in %/h.
func codexRateUnit(w *CodexWindow) (secs float64, label string) {
	if w != nil && w.WindowSeconds >= 24*3600 {
		return 86400, "d"
	}
	return 3600, "h"
}

// codexRateText formats a per-second rate in the window's unit, or "—".
func codexRateText(w *CodexWindow, ratePerSec float64, ok bool) string {
	secs, label := codexRateUnit(w)
	if !ok {
		return "—"
	}
	return fmt.Sprintf("%.1f%%/%s", ratePerSec*secs, label)
}

// codexAvgRate is the average pace since the window opened, in the window's
// unit, or "—" while too little has elapsed.
func (a *App) codexAvgRate(w *CodexWindow) string {
	if w == nil {
		return "—"
	}
	left, known := codexSecondsUntil(w)
	r, ok := avgRate(w.UsedPercent, left, float64(w.WindowSeconds), 1, known)
	return codexRateText(w, r, ok)
}

// codexSpare is the projected % left at the window's reset at the given rate.
func (a *App) codexSpare(w *CodexWindow, rate float64) string {
	if w == nil {
		return "—"
	}
	reset := time.Unix(w.ResetsAt, 0).Format(time.RFC3339)
	work, known := workingSecondsUntil(reset, a.workStart, a.workEnd, time.Now())
	return spareText(w.UsedPercent, rate, work, known)
}

// codexSinceRate is the average consumption since the window opened, in % per
// second (0 when too little has elapsed to measure).
func codexSinceRate(w *CodexWindow) float64 {
	if w == nil {
		return 0
	}
	left, known := codexSecondsUntil(w)
	r, _ := avgRate(w.UsedPercent, left, float64(w.WindowSeconds), 1, known)
	return r
}

func (a *App) renderCodexScoped(limits []CodexLimit) {
	shown := 0
	for _, l := range limits {
		if l.ID == "codex" {
			continue
		}
		if l.Primary == nil && l.Secondary == nil {
			continue
		}
		if shown >= len(a.codexScoped) {
			break
		}
		name := l.Name
		if name == "" {
			name = l.ID
		}
		w := l.Primary
		if w == nil {
			w = l.Secondary
		}
		left, known := codexSecondsUntil(w)
		t, tok := codexResetAt(w)
		it := a.codexScoped[shown]
		it.SetTitle(fmt.Sprintf("%s:  %.0f%% left · %s", name, 100-w.UsedPercent,
			withClock(fmtRemaining(left, known), t, tok)))
		it.Show()
		shown++
	}
	for i := shown; i < len(a.codexScoped); i++ {
		a.codexScoped[i].Hide()
	}
	if shown == 0 {
		a.codexScopedParent.SetTitle("Additional limits: —")
	} else {
		a.codexScopedParent.SetTitle("Additional limits")
	}
}

func (a *App) renderProfile() {
	if a.profile == nil {
		return
	}
	name := a.profile.Account.FullName
	if name == "" {
		name = a.profile.Account.DisplayName
	}
	if name == "" {
		name = "—"
	}
	account := name
	if a.profile.Account.Email != "" {
		account += "  ·  " + a.profile.Account.Email
	}
	a.account.SetTitle("Account: " + account)
	tier := a.profile.Organization.RateLimitTier
	otype := a.profile.Organization.OrganizationType
	plan := planNames[tier]
	if plan == "" {
		plan = planNames[otype]
	}
	if plan == "" {
		plan = tier
	}
	if plan == "" {
		plan = "—"
	}
	a.plan.SetTitle("🎗 " + plan)
	a.setClaudeTitle("Claude · " + plan)
}

func (a *App) renderClaudeEmpty(account string) {
	a.setClaudeSessionRows(true)
	a.setClaudeTitle("Claude · —")
	a.account.SetTitle("Account: " + account)
	a.plan.SetTitle("Plan: —")
	a.session.SetTitle("Session (5h): —")
	a.sessionReset.SetTitle("      resets in —")
	a.weekly.SetTitle("Weekly: —")
	a.weeklyReset.SetTitle("      resets in —")
	a.weeklyBlocks.SetTitle("      5h blocks: —")
	a.weeklyBlocksBasis.SetTitle("")
	a.burn.SetTitle("Forecast @ rate · —")
	a.burnSession.SetTitle("Session (5h): —")
	a.burnWeekly.SetTitle("Weekly: —")
	a.speed.SetTitle("Last 15m: —")
	a.speedLeft.SetTitle("      ↳ spare at reset: —")
	a.speedAvg.SetTitle("Since reset: —")
	a.speedAvgLeft.SetTitle("      ↳ spare at reset: —")
	a.renderClaudeCredits()
	a.claudeResets.SetTitle("Resets available: —")
	a.renderScoped(nil)
}

// skipCalibModel reports models we deliberately don't compute a 5h-block
// coefficient for. Sonnet has its own large scoped weekly that doesn't track the
// 5-hour session meaningfully, so there's nothing useful to calibrate there.
func skipCalibModel(name string) bool {
	return strings.Contains(strings.ToLower(name), "sonnet")
}

// scopedModelName is the display name for a weekly_scoped limit (its calibrator
// key), falling back to "model" when the scope carries no name.
func scopedModelName(l ScopedLimit) string {
	if l.Scope != nil && l.Scope.Model != nil && l.Scope.Model.DisplayName != "" {
		return l.Scope.Model.DisplayName
	}
	return "model"
}

// modelCalib returns the calibrator for a model, creating it on first sight.
func (a *App) modelCalib(name string) *WeeklyCalibrator {
	c := a.calibByModel[name]
	if c == nil {
		c = &WeeklyCalibrator{}
		a.calibByModel[name] = c
	}
	return c
}

// modelBlockText is the per-model calibration suffix: how much weekly the
// model's full 5-hour session costs, and the Δsession % basis it was measured
// from. Empty until a real spend lands — no synthetic default.
func (a *App) modelBlockText(name string) string {
	if skipCalibModel(name) {
		return ""
	}
	c := a.calibByModel[name]
	if c == nil {
		return ""
	}
	bp, measured := c.BlockPct()
	if !measured {
		return ""
	}
	ds, _ := c.Snapshot()
	return fmt.Sprintf("  ·  5h-block ≈ %.1f%%/wk (basis %.0f%% sess)", bp, ds)
}

func (a *App) renderScoped(limits []ScopedLimit) {
	shown := 0
	for _, l := range limits {
		if l.Kind != "weekly_scoped" || l.Scope == nil {
			continue
		}
		if l.Percent <= 0 && l.ResetsAt == "" {
			continue
		}
		if shown >= len(a.scoped) {
			break
		}
		model := scopedModelName(l)
		left, known := secondsUntil(l.ResetsAt)
		reset, resetOk := parseISO(l.ResetsAt)
		it := a.scoped[shown]
		it.SetTitle(fmt.Sprintf("%s:  %.0f%% left · %s%s", model, 100-l.Percent,
			withClock(fmtRemaining(left, known), reset, resetOk), a.modelBlockText(model)))
		it.Show()
		shown++
	}
	for i := shown; i < len(a.scoped); i++ {
		a.scoped[i].Hide()
	}
	if shown == 0 {
		a.scopedParent.SetTitle("Weekly by model: —")
	} else {
		a.scopedParent.SetTitle("Weekly by model")
	}
}

// recordHistory logs one utilisation reading (cumulative %, 0..100) for a
// series, timestamped now, feeding the burn-history charts.
func (a *App) recordHistory(series string, v float64) {
	if a.history == nil {
		a.history = map[string][]histSample{}
	}
	a.history[series] = addHistSample(a.history[series], time.Now().Unix(), v)
	a.notePctStep(series, v)
}

// notePctStep watches a quota series for a step up — quota just got spent — and
// fires the owning dial's pulse, scaled by how much went. The first reading of a
// series in this process only seeds the comparison; resets (which drop the
// counter) and the credit series fire nothing.
func (a *App) notePctStep(series string, v float64) {
	side := pulseSideFor(series)
	if side == "" {
		return
	}
	if a.lastPct == nil {
		a.lastPct = map[string]float64{}
	}
	prev, seen := a.lastPct[series]
	a.lastPct[series] = v
	if !seen || v <= prev+pulseMinDelta {
		return
	}
	now := time.Now()
	if side == "codex" {
		a.codexPulse = a.codexPulse.arm(now, v-prev)
	} else {
		a.claudePulse = a.claudePulse.arm(now, v-prev)
	}
}

// pulseSideFor maps a history series onto the dial side that should pulse, or
// "" for series that shouldn't (the credit counters).
func pulseSideFor(series string) string {
	switch series {
	case seriesClaudeSession, seriesClaudeWeekly:
		return "claude"
	case seriesCodexPrimary, seriesCodexSecondary:
		return "codex"
	}
	return ""
}

type historyWindow struct {
	samples []histSample
	secs    int64
	present bool
	used    float64
	resetAt int64
}

func makeBurnPanel(provider, label string, series historyWindow, horizon, grid, now int64, accent [3]int) burnChartPanel {
	if horizon <= 0 {
		horizon = 1
	}
	if grid <= 0 {
		grid = horizon
	}
	fifteenMin := windowUseRate(series.samples, now, int64(rateWindow))
	fiveMin := windowUseRate(series.samples, now, 5*60)
	trend, trendKnown := rateTrend(fiveMin, fifteenMin)
	periodLeft := float64(series.resetAt - now)
	periodKnown := series.resetAt > now
	averageRate, averageKnown := avgRate(series.used, periodLeft, float64(series.secs), 1, periodKnown)
	return burnChartPanel{
		Provider: provider, Label: label,
		Intervals:  burnIntervals(series.samples, now, horizon),
		WindowSecs: series.secs, GridSecs: grid,
		Start: now - horizon, Now: now,
		Known:  series.present && len(series.samples) >= 2,
		Accent: accent, UsedPct: series.used,
		RecentRate: fifteenMin.Rate, RateKnown: fifteenMin.OK,
		AverageRate: averageRate, AverageKnown: averageKnown,
		PeriodLeft: periodLeft, PeriodKnown: periodKnown, RateSpan: fifteenMin.Span,
		LastUseAt: fifteenMin.At, Trend: trend, TrendKnown: trendKnown, ResetAt: series.resetAt,
		Samples: series.samples,
	}
}

// smoothed sets the panel's blurred-curve width (0 = plain bars).
func smoothed(p burnChartPanel, sigmaSecs float64) burnChartPanel {
	p.SmoothSecs = sigmaSecs
	return p
}

// weekSmoothOptions are the 7d history row's selectable blur widths, in
// menu order. The key is what gets persisted.
var weekSmoothOptions = []struct {
	key   string
	label string
	secs  float64
}{
	{"off", "Off (plain bars)", 0},
	{"30m", "30 min", 30 * 60},
	{"1h", "1 hour", 3600},
	{"2h", "2 hours", 2 * 3600},
	{"4h", "4 hours", 4 * 3600},
}

const weekSmoothDefault = "1h"

// normWeekSmooth maps a persisted key onto a known option, falling back to
// the default for an absent or unknown value.
func normWeekSmooth(key string) string {
	for _, o := range weekSmoothOptions {
		if o.key == key {
			return key
		}
	}
	return weekSmoothDefault
}

func weekSmoothSecs(key string) float64 {
	for _, o := range weekSmoothOptions {
		if o.key == key {
			return o.secs
		}
	}
	return weekSmoothSecs(weekSmoothDefault)
}

func weekSmoothLabel(key string) string {
	for _, o := range weekSmoothOptions {
		if o.key == key {
			return o.label
		}
	}
	return weekSmoothLabel(weekSmoothDefault)
}

// codexHistoryWindows maps the API's primary/secondary names onto visual
// short/long columns. A sole weekly primary belongs next to Claude's weekly
// chart, not in the short-window row just because the API called it primary.
func (a *App) codexHistoryWindows() (short, long historyWindow) {
	p, s := (*CodexWindow)(nil), (*CodexWindow)(nil)
	if a.lastCodex != nil {
		p, s = a.lastCodex.primary(), a.lastCodex.secondary()
	}
	pw := historyWindow{samples: a.history[seriesCodexPrimary]}
	sw := historyWindow{samples: a.history[seriesCodexSecondary]}
	if p != nil {
		pw.secs, pw.present = codexWindowSecs(p, 0), true
		pw.used, pw.resetAt = p.UsedPercent, p.ResetsAt
	}
	if s != nil {
		sw.secs, sw.present = codexWindowSecs(s, 0), true
		sw.used, sw.resetAt = s.UsedPercent, s.ResetsAt
	}
	switch {
	case pw.present && sw.present && pw.secs <= sw.secs:
		return pw, sw
	case pw.present && sw.present:
		return sw, pw
	case pw.present && pw.secs < 24*60*60:
		return pw, historyWindow{}
	case pw.present:
		return historyWindow{}, pw
	case sw.present && sw.secs < 24*60*60:
		return sw, historyWindow{}
	case sw.present:
		return historyWindow{}, sw
	default:
		return historyWindow{}, historyWindow{}
	}
}

func windowChartLabel(window historyWindow, fallback string) string {
	if !window.present || window.secs <= 0 {
		return fallback
	}
	if window.secs >= 24*60*60 {
		return fmt.Sprintf("%dd", int(math.Round(float64(window.secs)/(24*60*60))))
	}
	return fmt.Sprintf("%dh", int(math.Round(float64(window.secs)/(60*60))))
}

// claudeHistoryWindows is the Claude counterpart of codexHistoryWindows: the
// short (5h) window is absent on a weekly-only account, so its card falls back
// to a slice of the weekly history like the Codex one does.
func (a *App) claudeHistoryWindows() (short, long historyWindow) {
	short = historyWindow{samples: a.history[seriesClaudeSession], secs: int64(fiveHourSecs)}
	long = historyWindow{samples: a.history[seriesClaudeWeekly], secs: int64(weekSecs)}
	u := a.lastUsage
	if u == nil {
		return short, long
	}
	if u.HasSession() {
		short.present = true
		short.used = u.FiveHour.Utilization
		if reset, ok := parseISO(u.FiveHour.ResetsAt); ok {
			short.resetAt = reset.Unix()
		}
	}
	long.present = true
	long.used = u.SevenDay.Utilization
	if reset, ok := parseISO(u.SevenDay.ResetsAt); ok {
		long.resetAt = reset.Unix()
	}
	return short, long
}

// middleHistory keeps the middle peer chart useful when a provider exposes
// only a weekly quota window. In that case it shows a five-hour slice of the
// real weekly utilisation history rather than inventing a five-hour quota or
// leaving the card empty. When a genuine short quota window exists, that window
// and its native horizon still win.
func middleHistory(short, long historyWindow) (historyWindow, int64, string) {
	if short.present {
		horizon := short.secs
		if horizon <= 0 {
			horizon = int64(fiveHourSecs)
		}
		return short, horizon, windowChartLabel(short, "5h")
	}
	return long, int64(fiveHourSecs), "5h"
}

func chartGrid(horizon int64, targetLines int) int64 {
	if horizon <= 0 || targetLines <= 0 {
		return 1
	}
	return max(int64(60), horizon/int64(targetLines))
}

func (a *App) setPairedHistoryRow(it *systray.MenuItem, left, right burnChartPanel) {
	if it == nil {
		return
	}
	it.SetTitle("")
	it.SetIconSize(renderPairedBurnChart(left, right, a.workStart, a.workEnd), pairedChartWidth, pairedChartHeight)
}

// renderHistory keeps the two providers visible as peer cards at the top of
// the menu: Claude on the left, Codex on the right. Each card retains its own
// update intervals, window length and reset, so its affordable pace is its own.
func (a *App) renderHistory() {
	if a.histHour == nil {
		return
	}
	now := time.Now().Unix()
	claudeShort, claudeLong := a.claudeHistoryWindows()
	claudeFocus := claudeShort
	if !claudeFocus.present {
		claudeFocus = claudeLong
	}
	codexShort, codexLong := a.codexHistoryWindows()
	codexFocus := codexShort
	if !codexFocus.present {
		codexFocus = codexLong
	}

	a.setPairedHistoryRow(a.histHour,
		smoothed(makeBurnPanel("Claude", "1h", claudeFocus, 3600, 5*60, now, claudeChartAccent), 3600/smoothSigmaDiv),
		smoothed(makeBurnPanel("Codex", "1h", codexFocus, 3600, 5*60, now, codexChartAccent), 3600/smoothSigmaDiv))
	claudeMiddle, claudeMiddleHorizon, claudeMiddleLabel := middleHistory(claudeShort, claudeLong)
	codexMiddle, codexMiddleHorizon, codexMiddleLabel := middleHistory(codexShort, codexLong)
	a.setPairedHistoryRow(a.histSession,
		smoothed(makeBurnPanel("Claude", claudeMiddleLabel, claudeMiddle,
			claudeMiddleHorizon, chartGrid(claudeMiddleHorizon, 20), now, claudeChartAccent), float64(claudeMiddleHorizon)/smoothSigmaDiv),
		smoothed(makeBurnPanel("Codex", codexMiddleLabel, codexMiddle,
			codexMiddleHorizon, chartGrid(codexMiddleHorizon, 20), now, codexChartAccent), float64(codexMiddleHorizon)/smoothSigmaDiv))
	longHorizon := int64(weekSecs)
	if codexLong.secs > 0 {
		longHorizon = codexLong.secs
	}
	weekSigma := weekSmoothSecs(a.weekSmooth)
	a.setPairedHistoryRow(a.histWeek,
		smoothed(makeBurnPanel("Claude", "7d", claudeLong, int64(weekSecs), chartGrid(int64(weekSecs), 28), now, claudeChartAccent), weekSigma),
		smoothed(makeBurnPanel("Codex", windowChartLabel(codexLong, "long"), codexLong,
			longHorizon, chartGrid(longHorizon, 28), now, codexChartAccent), weekSigma))
	claudeCreditReset, codexCreditReset := int64(0), int64(0)
	if secs, ok := a.claudeCreditHorizon(); ok {
		claudeCreditReset = now + int64(secs)
	}
	if secs, ok := a.codexCreditHorizon(); ok {
		codexCreditReset = now + int64(secs)
	}
	a.setPairedHistoryRow(a.histCredits,
		makeCreditPanel("Claude", a.history[seriesClaudeCredits], claudeCredits(a.lastUsage), claudeCreditReset, now, claudeChartAccent),
		makeCreditPanel("Codex", a.history[seriesCodexCredits], codexCredits(a.lastCodex), codexCreditReset, now, codexChartAccent))
}

// --- credits ----------------------------------------------------------------

// The credits horizon is the provider's longest quota window reset: credits
// are only burned while a plan window is exhausted, and that reset is the
// moment plan quota returns.
func (a *App) claudeCreditHorizon() (float64, bool) {
	if a.lastUsage == nil {
		return 0, false
	}
	return secondsUntil(a.lastUsage.SevenDay.ResetsAt)
}

func (a *App) codexCreditHorizon() (float64, bool) {
	return codexSecondsUntil(codexLongestWindow(a.lastCodex))
}

// codexLongestWindow is the window with the longest span (normally weekly).
func codexLongestWindow(u *CodexUsage) *CodexWindow {
	p, s := u.primary(), u.secondary()
	switch {
	case p == nil:
		return s
	case s == nil:
		return p
	case s.WindowSeconds > p.WindowSeconds:
		return s
	default:
		return p
	}
}

// creditRate is the rolling recent credit burn (display units per second) for
// a credit series — the same 15-minute window the quota "Last 15m" uses.
func (a *App) creditRate(series string) useRate {
	return windowUseRate(a.history[series], time.Now().Unix(), int64(rateWindow))
}

func (a *App) claudeCreditState() (creditBudget, creditProjection, useRate) {
	b := claudeCredits(a.lastUsage)
	rate := a.creditRate(seriesClaudeCredits)
	resetSecs, known := a.claudeCreditHorizon()
	return b, projectCredits(b, rate.Rate, rate.OK, resetSecs, known), rate
}

func (a *App) codexCreditState() (creditBudget, creditProjection, useRate) {
	b := codexCredits(a.lastCodex)
	rate := a.creditRate(seriesCodexCredits)
	resetSecs, known := a.codexCreditHorizon()
	return b, projectCredits(b, rate.Rate, rate.OK, resetSecs, known), rate
}

func (a *App) claudeCreditRing() creditRing {
	b, p, _ := a.claudeCreditState()
	return p.ring(b.Known)
}

func (a *App) codexCreditRing() creditRing {
	b, p, _ := a.codexCreditState()
	return p.ring(b.Known)
}

// recordCodexCreditSpend logs Codex credit spend as a running counter of
// balance drops, since the API reports only the balance. A top-up (balance
// rising) adds nothing, so the series stays monotone like the quota series.
func (a *App) recordCodexCreditSpend(prev, next creditBudget) {
	if !next.Known || !next.LeftKnown {
		return
	}
	counter := 0.0
	if h := a.history[seriesCodexCredits]; len(h) > 0 {
		counter = h[len(h)-1].V
	}
	if prev.LeftKnown {
		counter += math.Max(0, prev.Left-next.Left)
	}
	a.recordHistory(seriesCodexCredits, counter)
}

func (a *App) renderClaudeCredits() {
	if a.claudeCredits == nil {
		return
	}
	b, p, rate := a.claudeCreditState()
	line := "💳 " + creditsLine(b)
	if !b.Known {
		line = "💳 Credits: off (extra usage disabled)"
	}
	a.claudeCredits.SetTitle(line)
	setDetailRow(a.claudeCreditsRunway, creditsRunwayLine(b, p, rate.OK, rate.Span))
}

func (a *App) renderCodexCredits() {
	if a.codexCredits == nil {
		return
	}
	b, p, rate := a.codexCreditState()
	line := creditsLine(b)
	if !b.Known {
		line = "Credits: none"
	}
	a.codexCredits.SetTitle(line)
	setDetailRow(a.codexCreditsRunway, creditsRunwayLine(b, p, rate.OK, rate.Span))
}

// setDetailRow shows an indented detail row with text, or hides it when there
// is nothing to say.
func setDetailRow(it *systray.MenuItem, text string) {
	if it == nil {
		return
	}
	if text == "" {
		it.Hide()
		return
	}
	it.SetTitle(text)
	it.Show()
}

// makeCreditPanel builds the 5-hour credits burn card. Bars are credit spend
// per run between balance changes; the dashed line is the pace that would
// have spent the balance left at each moment exactly by the horizon reset;
// AVG is the rate over the last 24h.
func makeCreditPanel(provider string, samples []histSample, b creditBudget, resetAt, now int64, accent [3]int) burnChartPanel {
	horizon := int64(fiveHourSecs)
	fifteen := windowUseRate(samples, now, int64(rateWindow))
	five := windowUseRate(samples, now, 5*60)
	day := windowUseRate(samples, now, 24*60*60)
	trend, trendKnown := rateTrend(five, fifteen)
	periodLeft := float64(resetAt - now)
	periodKnown := resetAt > now
	remaining := 0.0
	if b.LeftKnown && !b.Unlimited {
		remaining = b.Left
	} else {
		periodKnown = false // nothing finite to pace against
	}
	unit := b.Unit
	if unit == "" {
		unit = "cr"
	}
	return burnChartPanel{
		Provider: provider, Label: "credits 5h",
		Intervals:  burnIntervals(samples, now, horizon),
		WindowSecs: 24 * 60 * 60, GridSecs: chartGrid(horizon, 20),
		Start: now - horizon, Now: now,
		Known:  b.Known && len(samples) >= 2,
		Accent: accent, Unit: unit, Remaining: remaining,
		RecentRate: fifteen.Rate, RateKnown: fifteen.OK,
		AverageRate: day.Rate, AverageKnown: day.OK,
		PeriodLeft: periodLeft, PeriodKnown: periodKnown, RateSpan: fifteen.Span,
		LastUseAt: fifteen.At, Trend: trend, TrendKnown: trendKnown, ResetAt: resetAt,
		Samples: samples,
	}
}

// failText is the "last failure" status: when it happened and what it was.
func failText(at time.Time, msg string) string {
	if at.IsZero() {
		return "—"
	}
	if msg == "" {
		return ago(at)
	}
	return ago(at) + " · " + msg
}

// codexWindowSecs is w's window length in seconds, or def when unknown.
func codexWindowSecs(w *CodexWindow, def int64) int64 {
	if w != nil && w.WindowSeconds > 0 {
		return int64(w.WindowSeconds)
	}
	return def
}

func (a *App) setIcon(png []byte, key string) {
	if a.iconKey == key {
		return
	}
	a.iconKey = key
	systray.SetIcon(png)
}

// renderIcon paints the paired dial. Each side independently shows either its
// live dial or, when that side is in error, a red spark that the blink loop
// toggles on and off — so Claude and Codex can blink red on their own.
func (a *App) renderIcon() {
	now := time.Now()
	ci, ck := a.errorOr(a.errorOn, "claude", a.claudeDial)
	xi, xk := a.errorOr(a.codexErrorOn, "codex", a.codexDial)
	if !a.errorOn {
		ci.pulse, ck = withPulse(a.claudePulse, now, ck)
	}
	if !a.codexErrorOn {
		xi.pulse, xk = withPulse(a.codexPulse, now, xk)
	}
	a.sideMark[0], a.sideMark[1] = ci.markColor, xi.markColor
	a.setIcon(renderDialSVG([]dialRenderInput{ci, xi}), ck+"|"+xk)
}

// withPulse renders p's current frame and extends the icon cache key with it, so
// every animation frame counts as a different icon.
func withPulse(p spendPulse, now time.Time, key string) (pulseFrame, string) {
	f := pulseIconFrame(p, now, pulseFX)
	if !f.Active {
		return f, key
	}
	t, _ := p.progress(now)
	return f, fmt.Sprintf("%s-pulse%d-%d", key, p.Seed, iround(t*pulseDur/pulseFrameSecs))
}

// errorOr returns the red-blink error frame for a side that's erroring, or the
// side's normal dial otherwise.
func (a *App) errorOr(on bool, name string, dial func() (dialRenderInput, string)) (dialRenderInput, string) {
	if !on {
		return dial()
	}
	if a.blink {
		return dialRenderInput{limits: blankLimits, markColor: burnRed, hasMark: true}, name + "-err-on"
	}
	return dialRenderInput{limits: blankLimits}, name + "-err-off"
}

func (a *App) claudeDial() (dialRenderInput, string) {
	u := a.lastUsage
	if u == nil {
		return dialRenderInput{limits: blankLimits}, "claude-empty"
	}
	sessPct, weekPct := u.FiveHour.Utilization, u.SevenDay.Utilization
	sessTf := timeFraction(u.FiveHour.ResetsAt, fiveHourSecs)
	weekTf := timeFraction(u.SevenDay.ResetsAt, weekSecs)
	weekBlock := a.weeklyBlockPct()
	sessReset, sessOk := parseISO(u.FiveHour.ResetsAt)
	weekReset, weekOk := parseISO(u.SevenDay.ResetsAt)
	sessPaceLeft, sessPaceWindow := a.paceClock(sessReset, sessOk, fiveHourSecs)
	weekPaceLeft, weekPaceWindow := a.paceClock(weekReset, weekOk, weekSecs)
	if !u.HasSession() {
		// Weekly-only account: one big full circle for the weekly window, as
		// for a weekly-only Codex plan. No 5h blocks to notch.
		week := Limit{Pct: weekPct, TimeLeft: weekTf, WindowSecs: weekSecs, WaitOKSecs: 12 * 60 * 60, TimeTicks: dayTicks(weekSecs),
			PaceLeftSecs: weekPaceLeft, PaceWindowSecs: weekPaceWindow}
		limits := map[string]Limit{"session": {Pct: 100, TimeLeft: 1}, "weekly": week}
		pace := limitPace(week, clamp(1-weekPct/100, 0, 1))
		ring := a.claudeCreditRing()
		key := fmt.Sprintf("claude-weekly-%d-%d-%d-%d-%s", iround(weekPct), iround(weekTf*48),
			int(pace), int(timeFill), ring.key())
		return dialRenderInput{limits: limits, markColor: quotaPaceColor(pace), hasMark: true,
			full: true, source: "weekly", credits: ring}, key
	}
	limits := map[string]Limit{
		"session": {Pct: sessPct, TimeLeft: sessTf, WindowSecs: fiveHourSecs, WaitOKSecs: 30 * 60, TimeTicks: hourTicks(fiveHourSecs),
			PaceLeftSecs: sessPaceLeft, PaceWindowSecs: sessPaceWindow},
		"weekly": {Pct: weekPct, TimeLeft: weekTf, WindowSecs: weekSecs, WaitOKSecs: 12 * 60 * 60, BlockPct: weekBlock, TimeTicks: dayTicks(weekSecs),
			PaceLeftSecs: weekPaceLeft, PaceWindowSecs: weekPaceWindow},
	}
	pace := aggregateDialPace(limits)
	mark := quotaPaceColor(pace)
	ring := a.claudeCreditRing()
	key := fmt.Sprintf("claude-%d-%d-%d-%d-%d-%d-%d-%s-%s", iround(sessPct), iround(weekPct),
		iround(sessTf*48), iround(weekTf*48), int(pace), int(timeFill),
		iround(weekBlock*10), dialPaceKey(limits), ring.key())
	return dialRenderInput{limits: limits, markColor: mark, hasMark: true, credits: ring}, key
}

// sessionLeftText renders the session's "% left", noting when the weekly ("main")
// limit caps how much of it is actually spendable. One full session costs bp% of
// the weekly, so the weekly's remaining budget covers only (weekLeft/bp) of a
// session; when that is below the session budget left, the rest is unreachable.
func (a *App) sessionLeftText(sessPct, weekPct float64) string {
	sessLeft := math.Max(0, 100-sessPct)
	bp, ok := a.calib.BlockPct()
	if ok && bp > 0 {
		weekLeft := math.Max(0, 100-weekPct)
		usable := math.Min(sessLeft, weekLeft/bp*100) // % of the session actually spendable
		if usable < sessLeft-0.5 {
			return fmt.Sprintf("%.0f%% left · %.0f%% usable (weekly cap)", sessLeft, usable)
		}
	}
	return fmt.Sprintf("%.0f%% left", sessLeft)
}

// weeklyBlockPct is the weekly % one full 5-hour session costs, floored so the
// dial's separators stay countable: this is where maxWeekTicks is enforced,
// writeBlockTicks draws whatever BlockPct it is given. 0 until a real spend is
// measured — no separators are drawn before then.
func (a *App) weeklyBlockPct() float64 {
	bp, ok := a.calib.BlockPct()
	if !ok {
		return 0
	}
	return math.Max(bp, 100.0/maxWeekTicks)
}

// weekBlocksText summarises the weekly limit in 5-hour-session blocks for the
// menu, using the un-clamped estimate (the text isn't size-limited like the
// dial). weekPct is the weekly % already used.
func (a *App) weekBlocksText(weekPct float64) string {
	bp, ok := a.calib.BlockPct()
	if !ok {
		return "5h blocks: calibrating…"
	}
	total := 100.0 / bp
	left := math.Max(0, 100-weekPct) / bp
	ds, _ := a.calib.Snapshot()
	return fmt.Sprintf("≈ %.1f / %.0f × 5h blocks left (1 ≈ %.1f%%/wk, basis %.0f%% sess)",
		left, total, bp, ds)
}

// weekBlocksLines splits the 5h-block summary into a headline (blocks left) and
// a basis detail, so the weekly entry spreads across rows instead of one
// overlong line. While calibrating the headline carries the status and the
// basis is blank.
func (a *App) weekBlocksLines(weekPct float64) (headline, basis string) {
	bp, ok := a.calib.BlockPct()
	if !ok {
		return "5h blocks: calibrating…", ""
	}
	total := 100.0 / bp
	left := math.Max(0, 100-weekPct) / bp
	ds, _ := a.calib.Snapshot()
	return fmt.Sprintf("≈ %.1f / %.0f × 5h blocks left", left, total),
		fmt.Sprintf("1 block ≈ %.1f%%/wk · basis %.0f%% session", bp, ds)
}

func (a *App) showError(msg string) {
	a.errorOn = true
	a.renderIcon()
	systray.SetTitle("")
	a.status.SetTitle("⚠ " + msg)
	for _, it := range []*systray.MenuItem{a.account, a.plan, a.session,
		a.sessionReset, a.weekly, a.weeklyReset} {
		it.SetTitle("—")
	}
}

// resetCandidate is one quota window's upcoming reset, labelled for display.
type resetCandidate struct {
	provider string // "Claude" / "Codex"
	window   string // "5h" / "7d" / "weekly" …
	at       time.Time
}

// nearestReset picks the soonest reset still in the future; ok is false when
// none is known.
func nearestReset(cands []resetCandidate, now time.Time) (resetCandidate, bool) {
	var best resetCandidate
	found := false
	for _, c := range cands {
		if c.at.IsZero() || !c.at.After(now) {
			continue
		}
		if !found || c.at.Before(best.at) {
			best, found = c, true
		}
	}
	return best, found
}

// resetText renders "in 2d 3h · Fri 09:00" for a reset moment.
func resetText(at time.Time, now time.Time) string {
	return withClock("in "+fmtRemaining(at.Sub(now).Seconds(), true), at, true)
}

func (a *App) claudeResetCandidates() []resetCandidate {
	u := a.lastUsage
	if u == nil {
		return nil
	}
	var out []resetCandidate
	if u.HasSession() {
		if t, ok := parseISO(u.FiveHour.ResetsAt); ok {
			out = append(out, resetCandidate{"Claude", "5h", t})
		}
	}
	if t, ok := parseISO(u.SevenDay.ResetsAt); ok {
		out = append(out, resetCandidate{"Claude", "7d", t})
	}
	return out
}

func (a *App) codexResetCandidates() []resetCandidate {
	u := a.lastCodex
	if u == nil {
		return nil
	}
	var out []resetCandidate
	for _, w := range []*CodexWindow{u.primary(), u.secondary()} {
		if t, ok := codexResetAt(w); ok {
			label := "5h"
			if w.WindowSeconds >= 24*60*60 {
				label = fmt.Sprintf("%dd", int(math.Round(float64(w.WindowSeconds)/86400)))
			} else if w.WindowSeconds > 0 {
				label = fmt.Sprintf("%dh", int(math.Round(float64(w.WindowSeconds)/3600)))
			}
			out = append(out, resetCandidate{"Codex", label, t})
		}
	}
	return out
}

func (a *App) setClaudeTitle(s string) {
	a.claudeTitle = s
	a.refreshResetRows()
}

func (a *App) setCodexTitle(s string) {
	a.codexTitle = s
	a.refreshResetRows()
}

// refreshResetRows keeps the "next reset" row and the block-title suffixes
// ("Claude · Max 20× · ↻ 2d 3h") current. Called every tick; it only pushes
// a title to systray when the text actually changed.
func (a *App) refreshResetRows() {
	now := time.Now()
	claude := a.claudeResetCandidates()
	codex := a.codexResetCandidates()
	suffix := func(cands []resetCandidate) string {
		if c, ok := nearestReset(cands, now); ok {
			return " · ↻ " + fmtRemaining(c.at.Sub(now).Seconds(), true)
		}
		return ""
	}
	if a.claudeParent != nil {
		if t := orElse(a.claudeTitle, "Claude · —") + suffix(claude); t != a.claudeTitleShown {
			a.claudeTitleShown = t
			a.claudeParent.SetTitle(t)
		}
	}
	if a.codexParent != nil {
		if t := orElse(a.codexTitle, "Codex · —") + suffix(codex); t != a.codexTitleShown {
			a.codexTitleShown = t
			a.codexParent.SetTitle(t)
		}
	}
	if a.nextReset != nil {
		text := "⏱ Next reset: —"
		if c, ok := nearestReset(append(claude, codex...), now); ok {
			text = fmt.Sprintf("⏱ Next reset: %s %s · %s", c.provider, c.window, resetText(c.at, now))
		}
		if text != a.nextResetShown {
			a.nextResetShown = text
			a.nextReset.SetTitle(text)
		}
	}
}

func (a *App) updateStatus() {
	a.applyIconHeight() // track the burn-driven size; shrinks back when idle
	a.refreshResetRows()
	a.claudeOk.SetTitle("Claude ✓ last success: " + agoOf(a.lastOk))
	a.claudeFail.SetTitle("Claude ✗ last failure: " + failText(a.lastFail, a.lastFailMsg))
	a.codexOk.SetTitle("Codex ✓ last success: " + agoOf(a.lastCodexOk))
	a.codexFail.SetTitle("Codex ✗ last failure: " + failText(a.lastCodexFail, a.lastCodexFailMsg))
	if a.inflight {
		a.status.SetTitle(fmt.Sprintf("⟳ requesting %s… %.0fs", a.reqSource,
			time.Since(a.reqStart).Seconds()))
		return
	}
	if a.cached {
		age := math.Inf(1)
		if !a.lastOk.IsZero() {
			age = time.Since(a.lastOk).Seconds()
		}
		if age >= cacheTTL {
			a.cached = false
			a.showError("No fresh data — " + orElse(a.lastFailMsg, "error"))
		} else {
			a.status.SetTitle(fmt.Sprintf("⚠ %s — showing cached (%ds)",
				a.lastFailMsg, int(cacheTTL-age)))
		}
	} else if a.errorOn {
		left := time.Until(a.nextPoll).Seconds()
		if left > 0 {
			a.status.SetTitle(fmt.Sprintf("⚠ backing off — retry in %ds", int(left)+1))
		}
	}
}

// --- persistence ------------------------------------------------------------

type persistState struct {
	LastOk       *float64 `json:"last_ok"`
	NextAllowed  float64  `json:"next_allowed"`
	PollInterval float64  `json:"poll_interval"`
	WorkStart    string   `json:"work_start"`
	WorkEnd      string   `json:"work_end"`
	TimeFill     string   `json:"time_fill"`
	SpendPulse   string   `json:"spend_pulse"` // pulseModeNames key; absent => default
	IconHeight   float64  `json:"icon_height"`
	DynIconOff   bool     `json:"dyn_icon_off"` // absent => false => auto-size on
	WeekSmooth   string   `json:"week_smooth"`  // weekSmoothOptions key; absent/unknown => default
	SessRate     float64  `json:"sess_rate"`
	WeekRate     float64  `json:"week_rate"`
	CalibDs      float64  `json:"calib_ds"` // retained session-spend sample (%)
	CalibDw      float64  `json:"calib_dw"` // matching weekly spend (%)
	CalibAt      float64  `json:"calib_at,omitempty"`
	// per-model retained samples, keyed by model display name
	CalibModels      map[string]calibSample  `json:"calib_models,omitempty"`
	CodexPrimaryRate float64                 `json:"codex_primary_rate"`
	CodexSecondRate  float64                 `json:"codex_secondary_rate"`
	CodexRefresh     string                  `json:"codex_refresh_token"`
	CodexAccount     string                  `json:"codex_account"`
	ClaudeRefresh    string                  `json:"claude_refresh_token"`
	Usage            json.RawMessage         `json:"usage"`
	Profile          json.RawMessage         `json:"profile"`
	CodexUsage       json.RawMessage         `json:"codex_usage"`
	History          map[string][]histSample `json:"history,omitempty"`
}

// calibSample is one persisted calibration sample and its observation time.
type calibSample struct {
	Ds float64 `json:"ds"`
	Dw float64 `json:"dw"`
	At float64 `json:"at,omitempty"`
}

func (a *App) loadState() {
	raw, err := os.ReadFile(a.statePath)
	if err != nil {
		return
	}
	var s persistState
	if json.Unmarshal(raw, &s) != nil {
		return
	}
	if s.LastOk != nil {
		a.lastOk = time.Unix(int64(*s.LastOk), 0)
	}
	if s.NextAllowed > 0 {
		if delay := s.NextAllowed - float64(time.Now().Unix()); delay > 0 {
			// Don't let a stale value defer the first poll beyond the ceiling.
			delay = math.Min(delay, maxPollSecs)
			a.nextPoll = time.Now().Add(time.Duration(delay * float64(time.Second)))
			a.nextAllowedEpoch = float64(time.Now().Unix()) + delay
		}
	}
	if m, ok := parseHHMM(s.WorkStart); ok {
		a.workStart = m
	}
	if m, ok := parseHHMM(s.WorkEnd); ok {
		a.workEnd = m
	}
	setTimeFillFromString(s.TimeFill)
	setPulseModeFromString(s.SpendPulse)
	if s.IconHeight >= minIconHeight && s.IconHeight <= maxIconHeight {
		a.iconHeight = s.IconHeight
	}
	a.dynIconOff = s.DynIconOff
	a.weekSmooth = normWeekSmooth(s.WeekSmooth)
	if s.PollInterval >= basePollSecs && s.PollInterval <= maxPollSecs {
		a.pollInterval = s.PollInterval
	}
	a.sessRate, a.weekRate = s.SessRate, s.WeekRate
	calibFallbackAt := time.Now()
	if !a.lastOk.IsZero() {
		calibFallbackAt = a.lastOk
	}
	calibAt := calibFallbackAt
	if s.CalibAt > 0 {
		calibAt = time.Unix(int64(s.CalibAt), 0)
	}
	a.calib.RestoreAt(s.CalibDs, s.CalibDw, calibAt)
	for name, cs := range s.CalibModels {
		c := &WeeklyCalibrator{}
		at := calibFallbackAt
		if cs.At > 0 {
			at = time.Unix(int64(cs.At), 0)
		}
		c.RestoreAt(cs.Ds, cs.Dw, at)
		a.calibByModel[name] = c
	}
	a.codexPrimaryRate, a.codexSecondRate = s.CodexPrimaryRate, s.CodexSecondRate
	a.codex.LoadRefresh(s.CodexRefresh)
	a.codex.LoadAccount(s.CodexAccount)
	a.client.LoadRefresh(s.ClaudeRefresh)
	if len(s.Usage) > 0 && string(s.Usage) != "null" {
		a.lastUsageRaw = s.Usage
		a.lastUsage = parseUsage(s.Usage)
	}
	if len(s.CodexUsage) > 0 && string(s.CodexUsage) != "null" {
		a.lastCodexRaw = s.CodexUsage
		a.lastCodex = parseCodexUsageCached(s.CodexUsage)
	}
	if len(s.Profile) > 0 && string(s.Profile) != "null" {
		var p Profile
		if json.Unmarshal(s.Profile, &p) == nil {
			a.profile = &p
		}
	}
	if s.History != nil {
		now := time.Now().Unix()
		a.history = map[string][]histSample{}
		for name, samples := range s.History {
			if pruned := decimateHistory(samples, now); len(pruned) > 0 {
				a.history[name] = pruned
			}
		}
	}
	a.seedRates()
}

// seedRates primes the rate trackers with the last persisted sample, timestamped
// at the last successful poll, so the FIRST poll after a restart measures a rate
// straight away instead of needing two fresh polls before "Last 15m" leaves zero.
// (Claude and Codex are fetched in one poll cycle, so lastOk stands in for the
// Codex sample time too.) A window that reset during downtime self-corrects:
// RateTracker.Update drops the seed when the new utilisation comes in lower.
func (a *App) seedRates() {
	if a.lastOk.IsZero() {
		return
	}
	t := float64(a.lastOk.Unix())
	if a.lastUsage != nil {
		a.rates.Update(t, a.lastUsage.FiveHour.Utilization, a.lastUsage.SevenDay.Utilization)
	}
	if a.lastCodex != nil {
		if p, s, ok := codexPercents(a.lastCodex); ok {
			a.cRates.Update(t, p, s)
		}
	}
}

func (a *App) saveState(force bool) {
	if !force && time.Since(a.lastSave).Seconds() < saveEverySecs {
		return
	}
	a.lastSave = time.Now()
	calibDs, calibDw := a.calib.Snapshot()
	var calibModels map[string]calibSample
	for name, c := range a.calibByModel {
		if ds, dw := c.Snapshot(); ds > 0 && dw > 0 {
			if calibModels == nil {
				calibModels = map[string]calibSample{}
			}
			calibModels[name] = calibSample{Ds: ds, Dw: dw, At: c.SnapshotAt()}
		}
	}
	s := persistState{
		NextAllowed:      a.nextAllowedEpoch,
		PollInterval:     a.pollInterval,
		WorkStart:        fmtHHMM(a.workStart),
		WorkEnd:          fmtHHMM(a.workEnd),
		TimeFill:         timeFillString(),
		SpendPulse:       pulseModeString(),
		IconHeight:       a.iconHeight,
		DynIconOff:       a.dynIconOff,
		WeekSmooth:       a.weekSmooth,
		SessRate:         a.sessRate,
		WeekRate:         a.weekRate,
		CalibDs:          calibDs,
		CalibDw:          calibDw,
		CalibAt:          a.calib.SnapshotAt(),
		CalibModels:      calibModels,
		CodexPrimaryRate: a.codexPrimaryRate,
		CodexSecondRate:  a.codexSecondRate,
		CodexRefresh:     a.codex.RefreshToken(),
		CodexAccount:     a.codex.AccountLabel(),
		ClaudeRefresh:    a.client.RefreshToken(),
		Usage:            json.RawMessage(a.lastUsageRaw),
		CodexUsage:       json.RawMessage(a.lastCodexRaw),
		History:          a.history,
	}
	if !a.lastOk.IsZero() {
		v := float64(a.lastOk.Unix())
		s.LastOk = &v
	}
	if a.profile != nil {
		if pb, err := json.Marshal(a.profile); err == nil {
			s.Profile = pb
		}
	}
	if data, err := json.MarshalIndent(s, "", "  "); err == nil {
		// 0600: the file holds the Codex refresh token (a long-lived secret).
		// Chmod too, since WriteFile leaves an existing file's mode unchanged.
		_ = os.WriteFile(a.statePath, data, 0o600)
		_ = os.Chmod(a.statePath, 0o600)
	}
}

// --- helpers ----------------------------------------------------------------

// scheduleNext sets the next poll (and the persisted next-allowed epoch) secs
// from now.
func (a *App) scheduleNext(secs float64) {
	now := time.Now()
	a.nextPoll = now.Add(time.Duration(secs * float64(time.Second)))
	a.nextAllowedEpoch = float64(now.Unix()) + secs
}

func (a *App) scheduleCodexNext(secs float64) {
	a.nextCodexPoll = time.Now().Add(time.Duration(secs * float64(time.Second)))
}

func nowSecs() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func iround(x float64) int { return int(math.Round(x)) }

func ago(t time.Time) string { return fmtAgo(time.Since(t).Seconds()) }

func agoOf(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return ago(t)
}

func orElse(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (u *CodexUsage) primary() *CodexWindow {
	if u == nil || len(u.Limits) == 0 {
		return nil
	}
	return u.Limits[0].Primary
}

func (u *CodexUsage) secondary() *CodexWindow {
	if u == nil || len(u.Limits) == 0 {
		return nil
	}
	return u.Limits[0].Secondary
}

func codexPercents(u *CodexUsage) (float64, float64, bool) {
	primary, secondary := u.primary(), u.secondary()
	if primary == nil && secondary == nil {
		return 0, 0, false
	}
	pctPrimary, pctSecondary := 0.0, 0.0
	if primary != nil {
		pctPrimary = primary.UsedPercent
	}
	if secondary != nil {
		pctSecondary = secondary.UsedPercent
	}
	return pctPrimary, pctSecondary, true
}

func codexSecondsUntil(w *CodexWindow) (float64, bool) {
	if w == nil || w.ResetsAt <= 0 {
		return 0, false
	}
	return time.Until(time.Unix(w.ResetsAt, 0)).Seconds(), true
}

func codexPlanLabel(plan string) string {
	switch plan {
	case "free":
		return "Free"
	case "go":
		return "Go"
	case "plus":
		return "Plus"
	case "pro":
		return "Pro"
	case "prolite":
		return "Pro Lite"
	case "team":
		return "Team"
	case "business":
		return "Business"
	case "enterprise":
		return "Enterprise"
	case "edu", "education":
		return "Edu"
	case "", "unknown":
		return "—"
	default:
		return plan
	}
}

func codexResetsText(u *CodexUsage) string {
	if u == nil || !u.ResetCreditsKnown {
		return "Resets available: —"
	}
	return fmt.Sprintf("Resets available: %d", u.ResetCredits)
}

func fmtHHMM(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }

func parseHHMM(s string) (int, bool) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0, false
	}
	return h*60 + m, true
}
