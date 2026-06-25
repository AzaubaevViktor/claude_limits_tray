package main

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/getlantern/systray"
)

const (
	profileEvery   = 60
	cacheTTL       = 600.0
	saveEverySecs  = 300.0
	scopedPoolSize = 4

	// Adaptive polling. The usage endpoint rate-limits well below once-per-30s
	// (and shares that budget with Claude Code itself), so we start slow and
	// self-tune: every 429 grows the steady-state interval, a run of clean
	// successes shrinks it back toward the floor.
	basePollSecs      = 120.0 // floor / starting interval
	maxPollSecs       = 600.0 // ceiling (also the backoff cap)
	pollGrowth        = 1.5   // multiply on 429, divide after a success streak
	okStreakToSpeedUp = 3     // clean successes before we poll faster again
)

var planNames = map[string]string{
	"default_claude_max_5x":  "Claude Max 5×",
	"default_claude_max_20x": "Claude Max 20×",
	"claude_max":             "Claude Max",
	"claude_pro":             "Claude Pro",
}

type pollResult struct {
	raw        []byte
	usage      *Usage
	prof       *Profile
	codexRaw   []byte
	codexUsage *CodexUsage
	codexErr   error
	err        error
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
	nextAllowedEpoch float64
	lastSave         time.Time
	inflight         bool
	reqStart         time.Time
	results          chan pollResult
	iconKey          string

	// working hours (local minutes from midnight) and last data
	workStart    int
	workEnd      int
	lastUsageRaw []byte
	lastUsage    *Usage
	sessRate     float64
	weekRate     float64

	lastCodexRaw     []byte
	lastCodex        *CodexUsage
	lastCodexOk      time.Time
	lastCodexFail    time.Time
	lastCodexFailMsg string
	codexPrimaryRate float64
	codexSecondRate  float64

	statePath string

	// menu items
	claudeParent                               *systray.MenuItem
	account, plan                              *systray.MenuItem
	session, sessionReset, weekly, weeklyReset *systray.MenuItem
	burn, burnSession, burnWeekly              *systray.MenuItem
	speed, speedAvg                            *systray.MenuItem
	scopedParent                               *systray.MenuItem
	scoped                                     []*systray.MenuItem
	codexParent                                *systray.MenuItem
	codexPlan                                  *systray.MenuItem
	codexPrimary, codexPrimaryReset            *systray.MenuItem
	codexSecondary, codexSecondaryReset        *systray.MenuItem
	codexBurn, codexBurnPrimary                *systray.MenuItem
	codexBurnSecondary                         *systray.MenuItem
	codexSpeed, codexSpeedAvg                  *systray.MenuItem
	codexCredits, codexStatus                  *systray.MenuItem
	codexScopedParent                          *systray.MenuItem
	codexScoped                                []*systray.MenuItem
	legendParent                               *systray.MenuItem
	work                                       *systray.MenuItem
	startItems, endItems                       []hourItem
	status, ok, fail                           *systray.MenuItem
}

func newApp() *App {
	a := &App{
		client:       &Client{},
		codex:        &CodexClient{},
		rates:        &RateTracker{},
		cRates:       &RateTracker{},
		results:      make(chan pollResult, 1),
		workStart:    9 * 60,
		workEnd:      18 * 60,
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
	a.buildMenu()
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
		if !a.inflight && !time.Now().Before(a.nextPoll) {
			a.startRequest()
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

// --- polling ----------------------------------------------------------------

func (a *App) startRequest() {
	a.inflight = true
	a.reqStart = time.Now()
	a.pollCount++
	wantProfile := a.profile == nil || a.pollCount%profileEvery == 0
	client := a.client
	codex := a.codex
	results := a.results
	go func() {
		codexRaw, codexUsage, codexErr := codex.Usage()
		raw, usage, err := client.Usage()
		if err != nil {
			results <- pollResult{
				codexRaw:   codexRaw,
				codexUsage: codexUsage,
				codexErr:   codexErr,
				err:        err,
			}
			return
		}
		var prof *Profile
		if wantProfile {
			prof, _ = client.Profile()
		}
		results <- pollResult{
			raw:        raw,
			usage:      usage,
			prof:       prof,
			codexRaw:   codexRaw,
			codexUsage: codexUsage,
			codexErr:   codexErr,
		}
	}()
}

func (a *App) drain() {
	select {
	case r := <-a.results:
		a.inflight = false
		a.noteCodex(r)
		if r.err != nil {
			a.noteFail(r.err)
		} else {
			a.onSuccess(r)
		}
		// A rotated refresh token must be saved promptly — losing it would mean
		// a keychain prompt on the next launch.
		if a.codex.TakeRefreshDirty() {
			a.saveState(true)
		}
	default:
	}
}

func (a *App) noteCodex(r pollResult) {
	if r.codexErr != nil {
		a.lastCodexFail = time.Now()
		a.lastCodexFailMsg = describe(r.codexErr)
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
	a.lastCodexFailMsg = ""
	a.codexErrorOn = false
	if primary, secondary, ok := codexPercents(r.codexUsage); ok {
		if p, s, ok := a.cRates.Update(nowSecs(), primary, secondary); ok {
			a.codexPrimaryRate, a.codexSecondRate = p, s
		}
	}
	a.lastCodexRaw = r.codexRaw
	a.lastCodex = r.codexUsage
	a.renderCodex()
	a.renderIcon()
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
	if status == 429 {
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

func describe(err error) string {
	ae, ok := err.(*APIError)
	if !ok {
		return err.Error()
	}
	switch {
	case ae.Status == 429:
		return "Rate-limited (429)"
	case ae.Status == 401 || ae.Status == 403:
		return "Auth expired — run any Claude command"
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
	if weekFc.Pos() > worst.Pos() {
		worst = weekFc
	}

	a.renderIcon()
	systray.SetTitle("")

	a.session.SetTitle(fmt.Sprintf("🕐 Session (5h):  %.0f%% left", 100-sessPct))
	a.sessionReset.SetTitle("      resets in " + fmtRemaining(sessLeft, sessKnown))
	a.weekly.SetTitle(fmt.Sprintf("📅 Weekly:  %.0f%% left", 100-weekPct))
	a.weeklyReset.SetTitle("      resets in " + fmtRemaining(weekLeft, weekKnown))

	avgSessH, asOk := avgRate(sessPct, sessLeft, fiveHourSecs, 3600, sessKnown)
	avgWeekD, awOk := avgRate(weekPct, weekLeft, weekSecs, 86400, weekKnown)
	a.speed.SetTitle(fmt.Sprintf("⚡ Last 5m:  sess %.1f%%/h · week %.1f%%/d",
		a.sessRate*3600, a.weekRate*86400))
	a.speedAvg.SetTitle(fmt.Sprintf("⚡ Since reset:  sess %s%%/h · week %s%%/d",
		fmtRate(avgSessH, asOk), fmtRate(avgWeekD, awOk)))
	a.burn.SetTitle("Forecast @ rate · " + worst.Category)
	a.burnSession.SetTitle("Session (5h):  " + sessFc.Text())
	a.burnWeekly.SetTitle("Weekly:  " + weekFc.Text())

	a.renderScoped(u.Limits)
	a.status.SetTitle(fmt.Sprintf("✓ sess %.0f%% left · week %.0f%% left",
		100-sessPct, 100-weekPct))
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
	a.codexParent.SetTitle("Codex · " + plan)
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

	a.codexSpeed.SetTitle(fmt.Sprintf("Last 5m:  primary %.1f%%/h · secondary %.1f%%/h",
		a.codexPrimaryRate*3600, a.codexSecondRate*3600))
	a.codexSpeedAvg.SetTitle(fmt.Sprintf("Since reset:  primary %s%%/h · secondary %s%%/h",
		a.codexAvgRate(primary), a.codexAvgRate(secondary)))
	a.codexCredits.SetTitle(codexCreditsText(u))
	status := "Last success: " + agoOf(a.lastCodexOk)
	if a.lastCodexFailMsg != "" {
		status += " · last failure: " + a.lastCodexFailMsg
	}
	a.codexStatus.SetTitle(status)
	a.renderCodexScoped(u.Limits)
}

func (a *App) renderCodexEmpty() {
	a.codexParent.SetTitle("Codex · —")
	a.codexPlan.SetTitle("Plan: —")
	a.codexPrimary.SetTitle("Primary:  —")
	a.codexPrimaryReset.SetTitle("      resets in —")
	a.codexSecondary.SetTitle("Secondary:  —")
	a.codexSecondaryReset.SetTitle("      resets in —")
	a.codexBurn.SetTitle("Forecast @ rate · —")
	a.codexBurnPrimary.SetTitle("Primary:  —")
	a.codexBurnSecondary.SetTitle("Secondary:  —")
	a.codexSpeed.SetTitle("Last 5m:  —")
	a.codexSpeedAvg.SetTitle("Since reset:  —")
	a.codexCredits.SetTitle("Credits: —")
	msg := "Last success: " + agoOf(a.lastCodexOk)
	if a.lastCodexFailMsg != "" {
		msg += " · last failure: " + a.lastCodexFailMsg
	}
	a.codexStatus.SetTitle(msg)
	a.renderCodexScoped(nil)
}

func (a *App) codexDial() (map[string]Limit, [3]int, bool, string) {
	u := a.lastCodex
	if u == nil {
		return blankLimits, [3]int{}, false, "codex-empty"
	}
	primary := u.primary()
	secondary := u.secondary()
	limits := map[string]Limit{
		"session": codexLimitForDial(primary),
		"weekly":  codexLimitForDial(secondary),
	}
	primaryFc := a.codexForecast(primary, a.codexPrimaryRate)
	secondaryFc := a.codexForecast(secondary, a.codexSecondRate)
	worst := primaryFc
	if secondaryFc.Pos() > worst.Pos() {
		worst = secondaryFc
	}
	key := fmt.Sprintf("codex-%d-%d-%d-%d-%d",
		iround(limitPct(limits["session"])), iround(limitPct(limits["weekly"])),
		iround(limits["session"].TimeLeft*48), iround(limits["weekly"].TimeLeft*48),
		iround(worst.Pos()*60))
	return limits, worst.Color(), true, key
}

func codexLimitForDial(w *CodexWindow) Limit {
	if w == nil {
		return Limit{Pct: 100, TimeLeft: 1}
	}
	return Limit{Pct: w.UsedPercent, TimeLeft: codexTimeFraction(w)}
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
	row.SetTitle(fmt.Sprintf("%s:  %.0f%% left", name, 100-w.UsedPercent))
	reset.SetTitle("      resets in " + fmtRemaining(left, known))
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

func (a *App) codexAvgRate(w *CodexWindow) string {
	if w == nil {
		return "—"
	}
	left, known := codexSecondsUntil(w)
	r, ok := avgRate(w.UsedPercent, left, float64(w.WindowSeconds), 3600, known)
	return fmtRate(r, ok)
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
		it := a.codexScoped[shown]
		it.SetTitle(fmt.Sprintf("%s:  %.0f%% left · %s", name, 100-w.UsedPercent,
			fmtRemaining(left, known)))
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
	a.account.SetTitle(name + "  ·  " + a.profile.Account.Email)
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
	a.claudeParent.SetTitle("Claude · " + plan)
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
		model := "model"
		if l.Scope.Model != nil && l.Scope.Model.DisplayName != "" {
			model = l.Scope.Model.DisplayName
		}
		left, known := secondsUntil(l.ResetsAt)
		it := a.scoped[shown]
		it.SetTitle(fmt.Sprintf("%s:  %.0f%% left · %s", model, 100-l.Percent,
			fmtRemaining(left, known)))
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
	cl, cc, ch, ck := a.errorOr(a.errorOn, "claude", a.claudeDial)
	xl, xc, xh, xk := a.errorOr(a.codexErrorOn, "codex", a.codexDial)
	a.setIcon(renderDualDial(cl, cc, ch, xl, xc, xh), ck+"|"+xk)
}

// errorOr returns the red-blink error frame for a side that's erroring, or the
// side's normal dial otherwise.
func (a *App) errorOr(on bool, name string, dial func() (map[string]Limit, [3]int, bool, string)) (map[string]Limit, [3]int, bool, string) {
	if !on {
		return dial()
	}
	if a.blink {
		return blankLimits, burnRed, true, name + "-err-on"
	}
	return blankLimits, [3]int{}, false, name + "-err-off"
}

func (a *App) claudeDial() (map[string]Limit, [3]int, bool, string) {
	u := a.lastUsage
	if u == nil {
		return blankLimits, [3]int{}, false, "claude-empty"
	}
	sessPct, weekPct := u.FiveHour.Utilization, u.SevenDay.Utilization
	sessLeft, sessKnown := secondsUntil(u.FiveHour.ResetsAt)
	weekLeft, weekKnown := secondsUntil(u.SevenDay.ResetsAt)
	sessTf := timeFraction(u.FiveHour.ResetsAt, fiveHourSecs)
	weekTf := timeFraction(u.SevenDay.ResetsAt, weekSecs)
	now := time.Now()
	sessWork, sw := workingSecondsUntil(u.FiveHour.ResetsAt, a.workStart, a.workEnd, now)
	weekWork, ww := workingSecondsUntil(u.SevenDay.ResetsAt, a.workStart, a.workEnd, now)
	sessFc := projectForecast(sessPct, forecastRate(a.sessRate, sessPct, sessLeft, fiveHourSecs, sessKnown), sessWork, sw)
	weekFc := projectForecast(weekPct, forecastRate(a.weekRate, weekPct, weekLeft, weekSecs, weekKnown), weekWork, ww)
	worst := sessFc
	if weekFc.Pos() > worst.Pos() {
		worst = weekFc
	}
	limits := map[string]Limit{
		"session": {sessPct, sessTf},
		"weekly":  {weekPct, weekTf},
	}
	key := fmt.Sprintf("claude-%d-%d-%d-%d-%d", iround(sessPct), iround(weekPct),
		iround(sessTf*48), iround(weekTf*48), iround(worst.Pos()*60))
	return limits, worst.Color(), true, key
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

func (a *App) updateStatus() {
	a.ok.SetTitle("✓ Last success: " + agoOf(a.lastOk))
	a.fail.SetTitle("✗ Last failure: " + agoOf(a.lastFail))
	if a.inflight {
		a.status.SetTitle(fmt.Sprintf("⟳ requesting… %.0fs", time.Since(a.reqStart).Seconds()))
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
	LastOk           *float64        `json:"last_ok"`
	NextAllowed      float64         `json:"next_allowed"`
	PollInterval     float64         `json:"poll_interval"`
	WorkStart        string          `json:"work_start"`
	WorkEnd          string          `json:"work_end"`
	SessRate         float64         `json:"sess_rate"`
	WeekRate         float64         `json:"week_rate"`
	CodexPrimaryRate float64         `json:"codex_primary_rate"`
	CodexSecondRate  float64         `json:"codex_secondary_rate"`
	CodexRefresh     string          `json:"codex_refresh_token"`
	Usage            json.RawMessage `json:"usage"`
	Profile          json.RawMessage `json:"profile"`
	CodexUsage       json.RawMessage `json:"codex_usage"`
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
	if s.PollInterval >= basePollSecs && s.PollInterval <= maxPollSecs {
		a.pollInterval = s.PollInterval
	}
	a.sessRate, a.weekRate = s.SessRate, s.WeekRate
	a.codexPrimaryRate, a.codexSecondRate = s.CodexPrimaryRate, s.CodexSecondRate
	a.codex.LoadRefresh(s.CodexRefresh)
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
}

func (a *App) saveState(force bool) {
	if !force && time.Since(a.lastSave).Seconds() < saveEverySecs {
		return
	}
	a.lastSave = time.Now()
	s := persistState{
		NextAllowed:      a.nextAllowedEpoch,
		PollInterval:     a.pollInterval,
		WorkStart:        fmtHHMM(a.workStart),
		WorkEnd:          fmtHHMM(a.workEnd),
		SessRate:         a.sessRate,
		WeekRate:         a.weekRate,
		CodexPrimaryRate: a.codexPrimaryRate,
		CodexSecondRate:  a.codexSecondRate,
		CodexRefresh:     a.codex.RefreshToken(),
		Usage:            json.RawMessage(a.lastUsageRaw),
		CodexUsage:       json.RawMessage(a.lastCodexRaw),
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

func codexCreditsText(u *CodexUsage) string {
	if u == nil || len(u.Limits) == 0 || u.Limits[0].Credits == nil {
		if u != nil && u.ResetCredits > 0 {
			return fmt.Sprintf("Credits: %d reset", u.ResetCredits)
		}
		return "Credits: —"
	}
	c := u.Limits[0].Credits
	switch {
	case c.Unlimited:
		return "Credits: unlimited"
	case c.Balance != nil && *c.Balance != "":
		return "Credits: " + *c.Balance
	case c.HasCredits:
		return "Credits: available"
	default:
		return "Credits: none"
	}
}

func fmtHHMM(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }

func parseHHMM(s string) (int, bool) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0, false
	}
	return h*60 + m, true
}
