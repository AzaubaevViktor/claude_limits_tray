package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPollSchedulerSerialisesProviders(t *testing.T) {
	now := time.Now()
	a := &App{inflight: true}
	if got := a.nextDueRequest(now); got != pollNone {
		t.Fatalf("in-flight request must block another provider, got %v", got)
	}
}

func TestPollSchedulerDoesNotShareProviderBackoff(t *testing.T) {
	now := time.Now()
	a := &App{
		nextPoll:      now.Add(10 * time.Minute), // Claude is backing off.
		nextCodexPoll: now.Add(-time.Second),
	}
	if got := a.nextDueRequest(now); got != pollCodex {
		t.Fatalf("Codex should remain due while Claude backs off, got %v", got)
	}

	a.nextPoll = now.Add(-time.Second)
	a.nextCodexPoll = now.Add(10 * time.Minute) // Codex is backing off.
	if got := a.nextDueRequest(now); got != pollClaude {
		t.Fatalf("Claude should remain due while Codex backs off, got %v", got)
	}
}

func TestPollSchedulerAlternatesWhenBothDue(t *testing.T) {
	now := time.Now()
	a := &App{lastReqSource: pollClaude}
	if got := a.nextDueRequest(now); got != pollCodex {
		t.Fatalf("after Claude, Codex should run next, got %v", got)
	}
	a.lastReqSource = pollCodex
	if got := a.nextDueRequest(now); got != pollClaude {
		t.Fatalf("after Codex, Claude should run next, got %v", got)
	}
}

func TestCodexTimeTicksUseDaysForLongPrimaryWindow(t *testing.T) {
	if got := codexTimeTicks(7 * 24 * 60 * 60); got != 7 {
		t.Fatalf("weekly Codex window should have 7 daily segments, got %d", got)
	}
	if got := codexTimeTicks(5 * 60 * 60); got != 5 {
		t.Fatalf("short Codex window should keep 5 hourly segments, got %d", got)
	}
}

func TestCodexResetsText(t *testing.T) {
	if got := codexResetsText(&CodexUsage{ResetCredits: 0, ResetCreditsKnown: true}); got != "Resets available: 0" {
		t.Fatalf("known zero resets = %q", got)
	}
	if got := codexResetsText(&CodexUsage{}); got != "Resets available: —" {
		t.Fatalf("missing resets = %q", got)
	}
}

func TestCodexHistoryPlacesSoleWeeklyPrimaryInLongRow(t *testing.T) {
	primaryHistory := []histSample{{T: 1, V: 10}, {T: 2, V: 20}}
	a := &App{
		history: map[string][]histSample{seriesCodexPrimary: primaryHistory},
		lastCodex: &CodexUsage{Limits: []CodexLimit{{
			Primary: &CodexWindow{UsedPercent: 20, WindowSeconds: int64(weekSecs), ResetsAt: 12345},
		}}},
	}
	short, long := a.codexHistoryWindows()
	if short.present {
		t.Fatal("sole weekly primary must not occupy the short-window card")
	}
	if !long.present || long.secs != int64(weekSecs) || len(long.samples) != len(primaryHistory) {
		t.Fatalf("long window mapped incorrectly: %+v", long)
	}
	if long.used != 20 || long.resetAt != 12345 {
		t.Fatalf("projection inputs did not follow weekly primary: %+v", long)
	}
}

func TestChartGridKeepsTimelineResolution(t *testing.T) {
	if got := int64(fiveHourSecs) / chartGrid(int64(fiveHourSecs), 20); got != 20 {
		t.Fatalf("5h chart has %d grid columns, want 20", got)
	}
	if got := int64(weekSecs) / chartGrid(int64(weekSecs), 28); got != 28 {
		t.Fatalf("7d chart has %d grid columns, want 28", got)
	}
}

func TestCodexHistoryOrdersWindowsByDuration(t *testing.T) {
	a := &App{
		history: map[string][]histSample{
			seriesCodexPrimary:   {{T: 1, V: 7}},
			seriesCodexSecondary: {{T: 1, V: 3}},
		},
		lastCodex: &CodexUsage{Limits: []CodexLimit{{
			Primary:   &CodexWindow{WindowSeconds: int64(weekSecs)},
			Secondary: &CodexWindow{WindowSeconds: int64(fiveHourSecs)},
		}}},
	}
	short, long := a.codexHistoryWindows()
	if short.secs != int64(fiveHourSecs) || long.secs != int64(weekSecs) {
		t.Fatalf("windows not ordered short/long: %v / %v", short.secs, long.secs)
	}
	if short.samples[0].V != 3 || long.samples[0].V != 7 {
		t.Fatal("history series did not follow their windows during reordering")
	}
}

func TestClaudeDialCentreMatchesWorstOuterPace(t *testing.T) {
	now := time.Now()
	a := &App{
		calib: &WeeklyCalibrator{},
		lastUsage: &Usage{
			FiveHour: Window{Utilization: 80, ResetsAt: now.Add(150 * time.Minute).Format(time.RFC3339)},
			SevenDay: Window{Utilization: 1, ResetsAt: now.Add(4 * 24 * time.Hour).Format(time.RFC3339)},
		},
	}
	in, _ := a.claudeDial()
	if in.markColor != dialRed {
		t.Fatalf("centre = %v, want worst outer colour %v", in.markColor, dialRed)
	}
}

// After a restart the rate history is empty, so a naive tracker needs two fresh
// polls before "Last 15m" can leave zero. Seeding from the last persisted sample
// lets the very first poll measure a rate.
func TestSeedRatesEnablesFirstPollRate(t *testing.T) {
	a := newApp()
	a.lastOk = time.Unix(1000, 0)
	a.lastUsage = &Usage{FiveHour: Window{Utilization: 40}, SevenDay: Window{Utilization: 20}}
	a.seedRates()

	s, w, ok := a.rates.Update(1300, 46, 21) // one poll, 300s after the seed
	if !ok {
		t.Fatal("seeded tracker should compute a rate on the first poll")
	}
	approx(t, s, 6.0/300, 1e-6, "sess rate from seed")
	approx(t, w, 1.0/300, 1e-6, "week rate from seed")
}

// Task 9: no synthetic default — nothing is shown until a real spend is measured.
func TestNoBlockBeforeCalibration(t *testing.T) {
	a := newApp()
	a.statePath = filepath.Join(t.TempDir(), "state.json")
	a.calib = &WeeklyCalibrator{}
	a.calibByModel = map[string]*WeeklyCalibrator{}

	if bp := a.weeklyBlockPct(); bp != 0 {
		t.Fatalf("uncalibrated block should be 0, got %v", bp)
	}
	if txt := a.weekBlocksText(0); !strings.Contains(txt, "calibrating") {
		t.Fatalf("want calibrating placeholder, got %q", txt)
	}
	// The dial draws no separators until measured (BlockPct == 0).
	a.lastUsage = &Usage{FiveHour: Window{Utilization: 20}, SevenDay: Window{Utilization: 30}}
	in, _ := a.claudeDial()
	if in.limits["weekly"].BlockPct != 0 {
		t.Fatalf("uncalibrated should draw no separators, BlockPct=%v", in.limits["weekly"].BlockPct)
	}
}

// Task 14: once measured, the global Claude weekly line shows the Δsession basis.
func TestGlobalWeeklyShowsBasis(t *testing.T) {
	a := newApp()
	a.calib = &WeeklyCalibrator{}
	a.calib.Observe(0, 0)
	a.calib.Observe(30, 3) // Δs=30, Δw=3 -> 10%/wk, basis 30% session
	txt := a.weekBlocksText(0)
	if !strings.Contains(txt, "10.0%/wk") || !strings.Contains(txt, "basis 30% sess") {
		t.Fatalf("global weekly line should show ratio + basis, got %q", txt)
	}
}

// Task 1+9: per-model rows are empty until measured, then show ratio + basis.
func TestModelCalibrationText(t *testing.T) {
	a := newApp()
	a.calibByModel = map[string]*WeeklyCalibrator{}

	if got := a.modelBlockText("Opus"); got != "" {
		t.Fatalf("unmeasured model should show nothing, got %q", got)
	}
	c := a.modelCalib("Opus")
	c.Observe(0, 0)
	c.Observe(15, 3) // Δs=15, Δw=3 -> 20%/wk, basis 15% session
	got := a.modelBlockText("Opus")
	if !strings.Contains(got, "20.0%/wk") || !strings.Contains(got, "basis 15% sess") {
		t.Fatalf("measured model text wrong: %q", got)
	}
}

// Task 7: the auto-size opt-out persists; absent => on (dynIconOff false).
func TestDynIconPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	a := newApp()
	a.statePath = path
	a.dynIconOff = true
	a.saveState(true)

	b := newApp()
	b.statePath = path
	b.dynIconOff = false
	b.loadState()
	if !b.dynIconOff {
		t.Fatal("dynIconOff should persist as true")
	}

	c := newApp()
	c.statePath = filepath.Join(t.TempDir(), "none.json")
	c.loadState()
	if c.dynIconOff {
		t.Fatal("fresh install should default to auto-size ON (dynIconOff false)")
	}
}

// Task 13: Sonnet is never given a 5h-block coefficient.
func TestSonnetSkippedFromCalibration(t *testing.T) {
	if !skipCalibModel("Sonnet") || !skipCalibModel("Claude Sonnet 4.6") {
		t.Fatal("Sonnet should be skipped")
	}
	if skipCalibModel("Claude Opus 4.8") {
		t.Fatal("Opus should be calibrated")
	}
	a := newApp()
	a.calibByModel = map[string]*WeeklyCalibrator{}
	c := a.modelCalib("Sonnet")
	c.Observe(0, 0)
	c.Observe(20, 2)
	if got := a.modelBlockText("Sonnet"); got != "" {
		t.Fatalf("Sonnet should show no coefficient, got %q", got)
	}
}

// Task 5: the steady-state throttle is at least hourly (exit/quit/SIGINT force a
// flush via saveState(true), verified in signalLoop/onExit/the Quit handler).
func TestSaveThrottleAtLeastHourly(t *testing.T) {
	if saveEverySecs < 3600 {
		t.Fatalf("steady-state save should be >= hourly, got %v", saveEverySecs)
	}
}

// Tasks 1+5: global and per-model calibration survive a save/load round-trip.
func TestPersistCalibRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	observed := time.Now().Add(-23 * time.Hour).Truncate(time.Second)

	a := newApp()
	a.statePath = path
	a.calib = &WeeklyCalibrator{}
	a.calibByModel = map[string]*WeeklyCalibrator{}
	a.calib.RestoreAt(16, 2, observed)  // global k=0.125 -> 12.5%/session
	a.modelCalib("Opus").Restore(10, 2) // 20%/session
	a.saveState(true)

	b := newApp()
	b.statePath = path
	b.calib = &WeeklyCalibrator{}
	b.calibByModel = map[string]*WeeklyCalibrator{}
	b.loadState()

	if bp, measured := b.calib.BlockPct(); !measured || !near(bp, 12.5, 0.01) {
		t.Fatalf("global calib not restored: %v measured=%v", bp, measured)
	}
	if got := b.calib.SnapshotAt(); got != float64(observed.Unix()) {
		t.Fatalf("global calibration timestamp not restored: got %.0f want %d", got, observed.Unix())
	}
	if _, measured := b.calib.BlockPctAt(observed.Add(25 * time.Hour)); measured {
		t.Fatal("reloaded calibration should expire relative to its original timestamp")
	}
	mc := b.calibByModel["Opus"]
	if mc == nil {
		t.Fatal("per-model calib not restored")
	}
	if bp, measured := mc.BlockPct(); !measured || !near(bp, 20, 0.01) {
		t.Fatalf("model calib not restored: %v measured=%v", bp, measured)
	}
}

func TestLegacySaturatedCalibUsesLastOkAsAge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	lastOk := float64(time.Now().Add(-time.Hour).Unix())
	raw, err := json.Marshal(persistState{
		LastOk:  &lastOk,
		CalibDs: 100,
		CalibDw: 15,
		// CalibAt is absent in state files written before calibration ageing.
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}

	a := newApp()
	a.statePath = path
	a.loadState()
	if bp, ok := a.calib.BlockPct(); !ok || !near(bp, 15, 1e-9) {
		t.Fatalf("recent legacy 100%% sample should remain temporarily, got %v ok=%v", bp, ok)
	}
	if got := a.calib.SnapshotAt(); got != lastOk {
		t.Fatalf("legacy sample age = %.0f, want last_ok %.0f", got, lastOk)
	}
}

func TestSessionLeftTextWeeklyCap(t *testing.T) {
	a := newApp()
	// One session costs 15% of the weekly (ds=60 -> dw=9; a sub-cap sample).
	a.calib.Restore(60, 9)

	// 3% weekly left -> only 3/15 = 20% of the session is usable, though its own
	// budget shows 80% left.
	got := a.sessionLeftText(20, 97) // sessPct=20 (80% left), weekPct=97 (3% left)
	if !strings.Contains(got, "80% left") || !strings.Contains(got, "20% usable (weekly cap)") {
		t.Fatalf("capped session text = %q", got)
	}

	// Plenty of weekly left -> no cap note.
	if got := a.sessionLeftText(80, 20); got != "20% left" {
		t.Fatalf("uncapped session text = %q", got)
	}

	// Not calibrated -> no cap note even when the weekly is low.
	b := newApp()
	if got := b.sessionLeftText(20, 97); got != "80% left" {
		t.Fatalf("uncalibrated session text = %q", got)
	}
}

// A weekly-only account (five_hour null, no "session" limit) must be
// recognised so the session half of the UI can be hidden.
func TestDecodeUsageDetectsMissingSessionWindow(t *testing.T) {
	weeklyOnly := parseUsage([]byte(`{"five_hour":null,"seven_day":{"utilization":10,"resets_at":"2026-11-06T09:00:00Z"},
		"limits":[{"kind":"weekly_all","group":"weekly","percent":10,"is_active":true}]}`))
	if weeklyOnly == nil || weeklyOnly.HasSession() {
		t.Fatalf("five_hour null should mean no session window: %+v", weeklyOnly)
	}
	absent := parseUsage([]byte(`{"seven_day":{"utilization":10}}`))
	if absent == nil || absent.HasSession() {
		t.Fatal("a missing five_hour key should mean no session window")
	}
	present := parseUsage([]byte(`{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":10}}`))
	if present == nil || !present.HasSession() {
		t.Fatal("a five_hour object (even idle) is a session window")
	}
	byLimit := parseUsage([]byte(`{"five_hour":null,"seven_day":{},"limits":[{"kind":"session","group":"session","percent":0}]}`))
	if byLimit == nil || !byLimit.HasSession() {
		t.Fatal("a session-kind limit counts as a session window")
	}
	var nilUsage *Usage
	if nilUsage.HasSession() {
		t.Fatal("nil usage has no session")
	}
}

func TestClaudeWeeklyOnlyDialIsOneFullCircle(t *testing.T) {
	now := time.Now()
	u := parseUsage([]byte(`{"five_hour":null,"seven_day":{"utilization":40,"resets_at":"` +
		now.Add(3*24*time.Hour).Format(time.RFC3339) + `"}}`))
	a := &App{calib: &WeeklyCalibrator{}, lastUsage: u, history: map[string][]histSample{}}
	in, key := a.claudeDial()
	if !in.full || in.source != "weekly" {
		t.Fatalf("weekly-only Claude should collapse to a full weekly circle: %+v", in)
	}
	if in.limits["weekly"].Pct != 40 || in.limits["weekly"].BlockPct != 0 {
		t.Fatalf("weekly limit should carry the usage and no 5h-block notches: %+v", in.limits["weekly"])
	}
	if !strings.HasPrefix(key, "claude-weekly-") {
		t.Fatalf("icon key should mark the mode: %s", key)
	}
	// With a session window the paired semicircles are back.
	a.lastUsage = parseUsage([]byte(`{"five_hour":{"utilization":5},"seven_day":{"utilization":40}}`))
	if in, _ := a.claudeDial(); in.full {
		t.Fatal("a session window should restore the two-semicircle dial")
	}
}

func TestClaudeHistoryWindowsFallBackToWeeklyWithoutSession(t *testing.T) {
	a := &App{
		history: map[string][]histSample{
			seriesClaudeSession: {{T: 1, V: 3}},
			seriesClaudeWeekly:  {{T: 1, V: 7}, {T: 2, V: 8}},
		},
		lastUsage: parseUsage([]byte(`{"five_hour":null,"seven_day":{"utilization":8,"resets_at":"2026-11-06T09:00:00Z"}}`)),
	}
	short, long := a.claudeHistoryWindows()
	if short.present {
		t.Fatal("no session window -> the short card must not claim one")
	}
	if !long.present || long.used != 8 || long.resetAt == 0 || len(long.samples) != 2 {
		t.Fatalf("weekly window mapped incorrectly: %+v", long)
	}
	middle, horizon, label := middleHistory(short, long)
	if !middle.present || middle.secs != int64(weekSecs) || horizon != int64(fiveHourSecs) || label != "5h" {
		t.Fatalf("middle card should be a 5h slice of the weekly history: %+v %d %s", middle, horizon, label)
	}
	a.lastUsage = parseUsage([]byte(`{"five_hour":{"utilization":3,"resets_at":"2026-11-06T09:00:00Z"},"seven_day":{"utilization":8}}`))
	if short, _ := a.claudeHistoryWindows(); !short.present || short.used != 3 {
		t.Fatalf("session window should be present again: %+v", short)
	}
}

func TestClaudeResetsText(t *testing.T) {
	if got := claudeResetsText(parseUsage([]byte(`{"five_hour":{},"seven_day":{}}`))); got != "Resets available: —" {
		t.Fatalf("no field = %q", got)
	}
	obj := parseUsage([]byte(`{"five_hour":{},"seven_day":{},"rate_limit_reset_credits":{"available_count":2}}`))
	if got := claudeResetsText(obj); got != "Resets available: 2" {
		t.Fatalf("object form = %q", got)
	}
	bare := parseUsage([]byte(`{"five_hour":{},"seven_day":{},"rate_limit_reset_credits":1}`))
	if got := claudeResetsText(bare); got != "Resets available: 1" {
		t.Fatalf("bare number = %q", got)
	}
	if got := claudeResetsText(nil); got != "Resets available: —" {
		t.Fatalf("nil usage = %q", got)
	}
}
