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
	raw   []byte
	usage *Usage
	prof  *Profile
	err   error
}

type hourItem struct {
	item *systray.MenuItem
	min  int
}

type App struct {
	mu     sync.Mutex
	client *Client
	rates  *RateTracker

	// runtime state
	profile          *Profile
	pollCount        int
	errorOn          bool
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

	statePath string

	// menu items
	account, plan                             *systray.MenuItem
	session, sessionReset, weekly, weeklyReset *systray.MenuItem
	burn, burnSession, burnWeekly, speed       *systray.MenuItem
	scopedParent                              *systray.MenuItem
	scoped                                    []*systray.MenuItem
	legendParent                              *systray.MenuItem
	work                                      *systray.MenuItem
	startItems, endItems                      []hourItem
	status, ok, fail                          *systray.MenuItem
}

func newApp() *App {
	a := &App{
		client:       &Client{},
		rates:        &RateTracker{},
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
	systray.SetTooltip("Claude limits")
	a.buildMenu()
	if a.lastUsage != nil {
		a.apply()
	} else {
		a.setIcon(renderDial(blankLimits, [3]int{}, false), "init")
	}
	if a.profile != nil {
		a.renderProfile() // restore account/plan immediately
	}
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
		if a.errorOn {
			a.blink = !a.blink
			a.renderErrorIcon()
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
	results := a.results
	go func() {
		raw, usage, err := client.Usage()
		if err != nil {
			results <- pollResult{err: err}
			return
		}
		var prof *Profile
		if wantProfile {
			prof, _ = client.Profile()
		}
		results <- pollResult{raw: raw, usage: usage, prof: prof}
	}()
}

func (a *App) drain() {
	select {
	case r := <-a.results:
		a.inflight = false
		if r.err != nil {
			a.noteFail(r.err)
		} else {
			a.onSuccess(r)
		}
	default:
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
	sessTf := timeFraction(u.FiveHour.ResetsAt, fiveHourSecs)
	weekTf := timeFraction(u.SevenDay.ResetsAt, weekSecs)

	now := time.Now()
	sessWork, sw := workingSecondsUntil(u.FiveHour.ResetsAt, a.workStart, a.workEnd, now)
	weekWork, ww := workingSecondsUntil(u.SevenDay.ResetsAt, a.workStart, a.workEnd, now)
	sessFc := projectForecast(sessPct, a.sessRate, sessWork, sw)
	weekFc := projectForecast(weekPct, a.weekRate, weekWork, ww)
	worst := sessFc
	if weekFc.Pos() > worst.Pos() {
		worst = weekFc
	}

	limits := map[string]Limit{
		"session": {sessPct, sessTf},
		"weekly":  {weekPct, weekTf},
	}
	key := fmt.Sprintf("%d-%d-%d-%d-%d", iround(sessPct), iround(weekPct),
		iround(sessTf*48), iround(weekTf*48), iround(worst.Pos()*60))
	a.setIcon(renderDial(limits, worst.Color(), true), key)
	systray.SetTitle("")

	a.session.SetTitle(fmt.Sprintf("🕐 Session (5h):  %.0f%% left", 100-sessPct))
	a.sessionReset.SetTitle("      resets in " + fmtRemaining(sessLeft, sessKnown))
	a.weekly.SetTitle(fmt.Sprintf("📅 Weekly:  %.0f%% left", 100-weekPct))
	a.weeklyReset.SetTitle("      resets in " + fmtRemaining(weekLeft, weekKnown))

	a.speed.SetTitle(fmt.Sprintf("⚡ Speed (5m):  sess %.1f%%/h · week %.1f%%/h",
		a.sessRate*3600, a.weekRate*3600))
	a.burn.SetTitle("Forecast @ rate · " + worst.Category)
	a.burnSession.SetTitle("Session (5h):  " + sessFc.Text())
	a.burnWeekly.SetTitle("Weekly:  " + weekFc.Text())

	a.renderScoped(u.Limits)
	a.status.SetTitle(fmt.Sprintf("✓ sess %.0f%% left · week %.0f%% left",
		100-sessPct, 100-weekPct))
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

func (a *App) renderErrorIcon() {
	c := [3]int{}
	has := a.blink
	if has {
		c = burnRed
	}
	a.setIcon(renderDial(blankLimits, c, has), fmt.Sprintf("err-%v", a.blink))
}

func (a *App) showError(msg string) {
	a.errorOn = true
	a.renderErrorIcon()
	systray.SetTitle("?")
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
	LastOk      *float64        `json:"last_ok"`
	NextAllowed  float64         `json:"next_allowed"`
	PollInterval float64         `json:"poll_interval"`
	WorkStart    string          `json:"work_start"`
	WorkEnd      string          `json:"work_end"`
	SessRate     float64         `json:"sess_rate"`
	WeekRate     float64         `json:"week_rate"`
	Usage        json.RawMessage `json:"usage"`
	Profile      json.RawMessage `json:"profile"`
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
	if len(s.Usage) > 0 && string(s.Usage) != "null" {
		a.lastUsageRaw = s.Usage
		a.lastUsage = parseUsage(s.Usage)
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
		NextAllowed:  a.nextAllowedEpoch,
		PollInterval: a.pollInterval,
		WorkStart:    fmtHHMM(a.workStart),
		WorkEnd:      fmtHHMM(a.workEnd),
		SessRate:     a.sessRate,
		WeekRate:     a.weekRate,
		Usage:        json.RawMessage(a.lastUsageRaw),
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
		_ = os.WriteFile(a.statePath, data, 0o644)
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

func fmtHHMM(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }

func parseHHMM(s string) (int, bool) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0, false
	}
	return h*60 + m, true
}
