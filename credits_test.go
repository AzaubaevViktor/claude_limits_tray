package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// The user's example: at the current rate the credits last two days, the
// reset is four days away -> the ring is half lit, and the next day's spend is
// a quarter of the ring (one of the four days).
func TestProjectCreditsHalfCoverQuarterDay(t *testing.T) {
	b := creditBudget{Known: true, Left: 200, LeftKnown: true, Unit: "cr"}
	rate := 100.0 / 86400 // 100 cr/day -> 200 cr last two days
	p := projectCredits(b, rate, true, 4*86400, true)
	if !p.Projected || p.Empty {
		t.Fatalf("should be projectable: %+v", p)
	}
	if !near(p.Cover, 0.5, 1e-9) || !near(p.DaySlice, 0.25, 1e-9) {
		t.Fatalf("cover/day = %v/%v, want 0.5/0.25", p.Cover, p.DaySlice)
	}
	if !near(p.Runway, 2*86400, 1e-6) || !near(p.DailySpend, 100, 1e-9) {
		t.Fatalf("runway/daily = %v/%v", p.Runway, p.DailySpend)
	}
}

func TestProjectCreditsCoverCapsAtFullAndDaySliceWithinCover(t *testing.T) {
	b := creditBudget{Known: true, Left: 1000, LeftKnown: true}
	// 10 cr/day over 4 days needs 40 of 1000 -> lasts; day slice = 1/4.
	p := projectCredits(b, 10.0/86400, true, 4*86400, true)
	if p.Cover != 1 || !near(p.DaySlice, 0.25, 1e-9) {
		t.Fatalf("lasting budget: cover %v day %v", p.Cover, p.DaySlice)
	}
	// Runway shorter than a day: the day slice is clipped to the cover.
	p = projectCredits(creditBudget{Known: true, Left: 10, LeftKnown: true}, 100.0/86400, true, 4*86400, true)
	if !near(p.Cover, 0.1/4, 1e-9) || !near(p.DaySlice, p.Cover, 1e-12) {
		t.Fatalf("short runway: cover %v day %v", p.Cover, p.DaySlice)
	}
}

func TestProjectCreditsIdleEmptyAndUnknownReset(t *testing.T) {
	b := creditBudget{Known: true, Left: 50, LeftKnown: true}
	idle := projectCredits(b, 0, true, 3*86400, true)
	if !idle.Projected || idle.Cover != 1 || idle.DaySlice != 0 || !math.IsInf(idle.Runway, 1) {
		t.Fatalf("idle budget should cover everything with no day slice: %+v", idle)
	}
	empty := projectCredits(creditBudget{Known: true, Left: 0, LeftKnown: true}, 1, true, 3*86400, true)
	if !empty.Projected || !empty.Empty || empty.Cover != 0 {
		t.Fatalf("exhausted budget: %+v", empty)
	}
	noReset := projectCredits(b, 1, true, 0, false)
	if noReset.Projected || !near(noReset.Runway, 50, 1e-9) {
		t.Fatalf("unknown reset must not project a cover but keeps the runway: %+v", noReset)
	}
	if p := projectCredits(creditBudget{}, 1, true, 3*86400, true); p.Projected {
		t.Fatalf("no budget -> nothing to project: %+v", p)
	}
	if p := projectCredits(b, 0, false, 3*86400, true); p.Projected {
		t.Fatalf("unmeasured rate must not read as idle: %+v", p)
	}
	unlimited := projectCredits(creditBudget{Known: true, Unlimited: true}, 1, true, 0, false)
	if !unlimited.Projected || unlimited.Cover != 1 {
		t.Fatalf("unlimited always covers: %+v", unlimited)
	}
}

func TestClaudeCreditsFromExtraUsage(t *testing.T) {
	u := parseUsage([]byte(`{"five_hour":{"utilization":1},"seven_day":{"utilization":2},
		"extra_usage":{"is_enabled":true,"monthly_limit":5000,"used_credits":1250,"utilization":25,
		"currency":"USD","decimal_places":2}}`))
	if u == nil {
		t.Fatal("usage should parse")
	}
	b := claudeCredits(u)
	if !b.Known || b.Unit != "$" || !b.LeftKnown || !near(b.Left, 37.5, 1e-9) ||
		!near(b.Used, 12.5, 1e-9) || !near(b.Limit, 50, 1e-9) {
		t.Fatalf("extra usage budget = %+v", b)
	}
	if pct, ok := b.usedPct(); !ok || !near(pct, 25, 1e-9) {
		t.Fatalf("used pct = %v ok=%v", pct, ok)
	}
	if got := creditsLine(b); got != "Credits: $37.50 left of $50.00 (25% used)" {
		t.Fatalf("credits line = %q", got)
	}
}

func TestClaudeCreditsDisabledOrOddShapesAreIgnored(t *testing.T) {
	disabled := parseUsage([]byte(`{"five_hour":{},"seven_day":{},
		"extra_usage":{"is_enabled":false,"monthly_limit":null,"used_credits":null},
		"spend":{"enabled":false,"used":{"amount_minor":0,"currency":"USD","exponent":2}}}`))
	if b := claudeCredits(disabled); b.Known {
		t.Fatalf("disabled credits should not be a budget: %+v", b)
	}
	odd := parseUsage([]byte(`{"five_hour":{},"seven_day":{},"extra_usage":"weird","spend":[1,2]}`))
	if odd == nil {
		t.Fatal("odd credit shapes must not break the usage response")
	}
	if b := claudeCredits(odd); b.Known {
		t.Fatalf("odd shapes should be ignored: %+v", b)
	}
}

func TestClaudeCreditsPrefersEnabledSpend(t *testing.T) {
	u := parseUsage([]byte(`{"five_hour":{},"seven_day":{},
		"extra_usage":{"is_enabled":true,"monthly_limit":9999,"used_credits":1,"decimal_places":2},
		"spend":{"enabled":true,"used":{"amount_minor":1230,"currency":"USD","exponent":2},
		"limit":{"amount_minor":10000,"currency":"USD","exponent":2},"balance":"87.70"}}`))
	b := claudeCredits(u)
	if !b.Known || !near(b.Used, 12.3, 1e-9) || !near(b.Limit, 100, 1e-9) || !near(b.Left, 87.7, 1e-9) {
		t.Fatalf("spend budget = %+v", b)
	}
}

func TestCodexCreditsBalance(t *testing.T) {
	raw := []byte(`{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":1793827387}},
		"credits":{"has_credits":true,"unlimited":false,"balance":"60873.9708015000"}}`)
	u, err := parseCodexUsage(raw)
	if err != nil {
		t.Fatal(err)
	}
	b := codexCredits(u)
	if !b.Known || !b.LeftKnown || !near(b.Left, 60873.9708015, 1e-6) || b.Unit != "cr" {
		t.Fatalf("codex budget = %+v", b)
	}
	if got := creditsLine(b); got != "Credits: 60,874 cr left" {
		t.Fatalf("codex credits line = %q", got)
	}
	none, _ := parseCodexUsage([]byte(`{"credits":{"has_credits":false,"unlimited":false,"balance":null}}`))
	if b := codexCredits(none); b.Known {
		t.Fatalf("no credits bought -> no budget: %+v", b)
	}
	unlimited, _ := parseCodexUsage([]byte(`{"credits":{"has_credits":true,"unlimited":true}}`))
	if b := codexCredits(unlimited); !b.Known || !b.Unlimited {
		t.Fatalf("unlimited budget = %+v", b)
	}
}

func TestLenientAmountShapes(t *testing.T) {
	cases := map[string]float64{
		`12.5`:   12.5,
		`"12.5"`: 12.5,
		`{"amount_minor":1250,"currency":"USD","exponent":2}`: 12.5,
		`{"amount_minor":"1250"}`:                             12.5,
		`{"amount":12.5}`:                                     12.5,
	}
	for raw, want := range cases {
		got, _, ok := lenientAmount(json.RawMessage(raw))
		if !ok || !near(got, want, 1e-9) {
			t.Errorf("lenientAmount(%s) = %v ok=%v, want %v", raw, got, ok, want)
		}
	}
	for _, raw := range []string{``, `null`, `"abc"`, `{"x":1}`, `[1]`} {
		if _, _, ok := lenientAmount(json.RawMessage(raw)); ok {
			t.Errorf("lenientAmount(%s) should fail", raw)
		}
	}
}

func TestFmtAmount(t *testing.T) {
	cases := []struct {
		v    float64
		unit string
		want string
	}{
		{12.3, "$", "$12.30"},
		{0.05, "$", "$0.05"},
		{1234.5, "$", "$1,235"},
		{60873.97, "cr", "60,874 cr"},
		{312.46, "cr", "312.5 cr"},
		{3.25, "cr", "3.25 cr"},
		{1234567, "", "1,234,567"},
	}
	for _, c := range cases {
		if got := fmtAmount(c.v, c.unit); got != c.want {
			t.Errorf("fmtAmount(%v,%q) = %q, want %q", c.v, c.unit, got, c.want)
		}
	}
}

func TestCreditsRunwayLine(t *testing.T) {
	b := creditBudget{Known: true, Left: 200, LeftKnown: true, Unit: "cr"}
	p := projectCredits(b, 100.0/86400, true, 4*86400, true)
	got := creditsRunwayLine(b, p, true, 900)
	if !strings.Contains(got, "lasts 2d 0h of 4d 0h to reset (50%)") || !strings.Contains(got, "≈ 100.0 cr/day (last 15m)") {
		t.Fatalf("runway line = %q", got)
	}
	idle := creditsRunwayLine(b, projectCredits(b, 0, true, 4*86400, true), true, 900)
	if !strings.Contains(idle, "idle · lasts to reset (4d 0h)") {
		t.Fatalf("idle line = %q", idle)
	}
	empty := creditsRunwayLine(creditBudget{Known: true, LeftKnown: true}, projectCredits(creditBudget{Known: true, LeftKnown: true}, 1, true, 3600, true), true, 900)
	if !strings.Contains(empty, "exhausted") {
		t.Fatalf("empty line = %q", empty)
	}
	if got := creditsRunwayLine(creditBudget{}, creditProjection{}, false, 0); got != "" {
		t.Fatalf("no budget -> no detail row, got %q", got)
	}
}

func TestCreditRingRendering(t *testing.T) {
	in := dialRenderInput{limits: blankLimits,
		credits: creditRing{Show: true, Projected: true, Cover: 0.5, DaySlice: 0.25}}
	svg := string(renderDialSVG([]dialRenderInput{in}))
	for _, part := range []string{`data-credit="track"`, `data-credit="cover"`, `data-credit="day"`} {
		if !strings.Contains(svg, part) {
			t.Fatalf("credit ring should draw %s:\n%s", part, svg)
		}
	}
	if !strings.Contains(svg, `r="8.45"`) {
		t.Fatal("credit ring should sit on the outer edge radius")
	}
	// Without a budget the icon is unchanged: no credit parts at all.
	plain := string(renderDialSVG([]dialRenderInput{{limits: blankLimits}}))
	if strings.Contains(plain, "data-credit") {
		t.Fatal("no budget -> no credit ring")
	}
	// Not projectable: only the grey track.
	track := string(renderDialSVG([]dialRenderInput{{limits: blankLimits, credits: creditRing{Show: true}}}))
	if !strings.Contains(track, `data-credit="track"`) || strings.Contains(track, `data-credit="cover"`) {
		t.Fatal("unprojectable budget -> track only")
	}
	// Exhausted: red track, no cover.
	empty := string(renderDialSVG([]dialRenderInput{{limits: blankLimits, credits: creditRing{Show: true, Projected: true, Empty: true}}}))
	if !strings.Contains(empty, svgRGB(dialRed)) || strings.Contains(empty, `data-credit="cover"`) {
		t.Fatal("exhausted budget -> red track only")
	}
	// Full-circle mode draws the ring too.
	full := string(renderDialSVG([]dialRenderInput{{limits: map[string]Limit{"weekly": {Pct: 0, TimeLeft: 0.5}},
		source: "weekly", full: true, credits: creditRing{Show: true, Projected: true, Cover: 1}}}))
	if !strings.Contains(full, `data-credit="cover"`) {
		t.Fatal("full-circle mode should draw the credit ring")
	}
}

func TestRecordCodexCreditSpendCountsBalanceDrops(t *testing.T) {
	a := &App{history: map[string][]histSample{}}
	bal := func(v float64) creditBudget { return creditBudget{Known: true, Left: v, LeftKnown: true, Unit: "cr"} }
	a.recordCodexCreditSpend(creditBudget{}, bal(1000)) // first sighting: counter starts at 0
	time.Sleep(1100 * time.Millisecond)                 // samples need distinct unix seconds
	a.recordCodexCreditSpend(bal(1000), bal(940))       // spent 60
	time.Sleep(1100 * time.Millisecond)
	a.recordCodexCreditSpend(bal(940), bal(1940)) // top-up: nothing spent
	h := a.history[seriesCodexCredits]
	if len(h) != 3 || h[0].V != 0 || !near(h[1].V, 60, 1e-9) || !near(h[2].V, 60, 1e-9) {
		t.Fatalf("spend counter = %+v", h)
	}
	a.recordCodexCreditSpend(bal(1940), creditBudget{}) // no budget -> nothing logged
	if len(a.history[seriesCodexCredits]) != 3 {
		t.Fatal("a missing budget must not add samples")
	}
}

func TestCodexLongestWindow(t *testing.T) {
	short := &CodexWindow{WindowSeconds: int64(fiveHourSecs), ResetsAt: 10}
	long := &CodexWindow{WindowSeconds: int64(weekSecs), ResetsAt: 20}
	u := &CodexUsage{Limits: []CodexLimit{{Primary: short, Secondary: long}}}
	if got := codexLongestWindow(u); got != long {
		t.Fatalf("longest = %+v, want weekly", got)
	}
	u.Limits[0].Primary, u.Limits[0].Secondary = long, nil
	if got := codexLongestWindow(u); got != long {
		t.Fatalf("sole window = %+v", got)
	}
	if codexLongestWindow(nil) != nil {
		t.Fatal("nil usage -> nil window")
	}
}

func TestCodexDialCarriesCreditRing(t *testing.T) {
	now := time.Now().Unix()
	a := &App{
		history: map[string][]histSample{seriesCodexCredits: {{T: now - 600, V: 0}, {T: now, V: 100}}},
		lastCodex: &CodexUsage{Limits: []CodexLimit{{
			Primary: &CodexWindow{UsedPercent: 100, WindowSeconds: int64(weekSecs), ResetsAt: now + 4*86400},
			Credits: &CodexCredits{HasCredits: true, Balance: strPtr("28800")}, // 100 cr/10 min -> 14400/day -> 2 days
		}}},
	}
	in, key := a.codexDial()
	if !in.credits.Show || !in.credits.Projected || !near(in.credits.Cover, 0.5, 0.01) || !near(in.credits.DaySlice, 0.25, 0.01) {
		t.Fatalf("codex ring = %+v", in.credits)
	}
	if !strings.Contains(key, in.credits.key()) {
		t.Fatalf("icon key should include the ring: %s", key)
	}
	// No credits bought: no ring, icon unchanged.
	a.lastCodex.Limits[0].Credits = &CodexCredits{}
	if in, _ := a.codexDial(); in.credits.Show {
		t.Fatal("no budget -> no ring")
	}
}

func strPtr(s string) *string { return &s }

func TestClaudeDialRingFollowsExtraUsage(t *testing.T) {
	now := time.Now()
	u := parseUsage([]byte(`{"five_hour":{"utilization":10},"seven_day":{"utilization":20,"resets_at":"` +
		now.Add(2*24*time.Hour).Format(time.RFC3339) + `"},
		"extra_usage":{"is_enabled":true,"monthly_limit":10000,"used_credits":2000,"decimal_places":2}}`))
	a := &App{calib: &WeeklyCalibrator{}, lastUsage: u,
		history: map[string][]histSample{seriesClaudeCredits: {{T: now.Unix() - 900, V: 19}, {T: now.Unix(), V: 20}}}}
	in, _ := a.claudeDial()
	// $80 left at $1/15min = $96/day -> 20h runway of 48h -> cover 0.417, day 0.417 (clipped).
	if !in.credits.Show || !in.credits.Projected || !near(in.credits.Cover, 20.0/48, 0.01) {
		t.Fatalf("claude ring = %+v", in.credits)
	}
	if !near(in.credits.DaySlice, in.credits.Cover, 1e-9) {
		t.Fatalf("day slice should clip to the cover: %+v", in.credits)
	}
	a.lastUsage = parseUsage([]byte(`{"five_hour":{},"seven_day":{},"extra_usage":{"is_enabled":false}}`))
	if in, _ := a.claudeDial(); in.credits.Show {
		t.Fatal("disabled extra usage -> no ring")
	}
}

func TestMakeCreditPanelUsesCreditUnits(t *testing.T) {
	now := int64(1_000_000)
	samples := []histSample{{T: now - 1800, V: 0}, {T: now - 900, V: 50}, {T: now, V: 100}}
	b := creditBudget{Known: true, Left: 4800, LeftKnown: true, Unit: "cr"}
	panel := makeCreditPanel("Codex", samples, b, now+86400, now, codexChartAccent)
	if panel.Unit != "cr" || panel.Remaining != 4800 || !panel.Known || !panel.RateKnown {
		t.Fatalf("panel = %+v", panel)
	}
	// Affordable pace: 4800 cr over a day = 200 cr/h; the 200 cr/h reading is exactly 1x.
	if !near(panel.sustainableRate(), 4800.0/86400, 1e-12) || !near(panelBurnRatio(panel), 1, 1e-9) {
		t.Fatalf("sustainable %v ratio %v", panel.sustainableRate(), panelBurnRatio(panel))
	}
	svg := string(renderPairedBurnChart(panel, makeCreditPanel("Claude", nil, creditBudget{}, 0, now, claudeChartAccent), 9*60, 18*60))
	for _, want := range []string{"CODEX CREDITS 5H", `data-spent="100.0 cr"`, "CLAUDE CREDITS 5H", `data-spent="—"`} {
		if !strings.Contains(svg, want) {
			t.Fatalf("credits chart should contain %q:\n%s", want, svg)
		}
	}
	strip, _ := renderQuotaStrip(stripSide{Provider: "Codex", Rows: []stripRow{{"CR", panel}}}, stripSide{}, time.Unix(now, 0))
	if !strings.Contains(string(strip), `data-speed-ratio="1.000"`) {
		t.Fatalf("the strip should read credits at 1x their affordable pace: %s", strip)
	}
	if strings.Contains(string(strip), `data-pace-mark="true"`) {
		t.Fatal("credit rows have no nominal whole-window pace mark")
	}
	if got := countSubstr(svg, `data-burn-bar="true"`); got != 2 {
		t.Fatalf("want one bar per interval, got %d", got)
	}
	if !strings.Contains(svg, "/h</text>") || strings.Contains(svg, "%/h") {
		t.Fatal("axis labels should be in credits per hour, not percent")
	}
}

func TestFailText(t *testing.T) {
	if got := failText(time.Time{}, "boom"); got != "—" {
		t.Fatalf("never failed = %q", got)
	}
	got := failText(time.Now().Add(-90*time.Second), "Rate-limited (429)")
	if !strings.HasPrefix(got, "1m 30s ago · Rate-limited (429)") {
		t.Fatalf("failure text = %q", got)
	}
	if got := failText(time.Now(), ""); !strings.HasSuffix(got, "ago") {
		t.Fatalf("message-less failure = %q", got)
	}
}
