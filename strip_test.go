package main

import (
	"encoding/xml"
	"fmt"
	"strings"
	"testing"
	"time"
)

// stripTestSide builds a side with a measured 5h window, a quiet 7d one and a
// credit balance, so one render covers every row shape the strip can draw.
func stripTestSide(now int64) stripSide {
	quota := func(label string, used, rate float64, resetIn int64, known bool) stripRow {
		return stripRow{Label: label, Panel: burnChartPanel{
			Provider: "Claude", Label: label, WindowSecs: int64(fiveHourSecs),
			Start: now - 3600, Now: now, Known: true, Accent: claudeChartAccent,
			UsedPct: used, RecentRate: rate, RateKnown: known,
			PeriodLeft: float64(resetIn), PeriodKnown: true, ResetAt: now + resetIn,
			Trend: 0.5, TrendKnown: true, WorkStart: 9 * 60, WorkEnd: 18 * 60,
		}}
	}
	credits := stripRow{Label: "CR", Panel: burnChartPanel{
		Provider: "Claude", Unit: "cr", Remaining: 20, Now: now,
		RecentRate: 0.001, RateKnown: true, PeriodLeft: 3600, PeriodKnown: true,
		ResetAt: now + 3600, Accent: claudeChartAccent,
	}}
	return stripSide{Provider: "Claude", Accent: claudeChartAccent, Rows: []stripRow{
		quota("5H", 40, 10.0/3600, 2*3600, true),
		quota("7D", 20, 0, 5*86400+13*3600, false),
		credits,
	}}
}

func TestQuotaStripShowsEveryWindowsCountdownAndSpeed(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.Local)
	side := stripTestSide(now.Unix())
	svg, h := renderQuotaStrip(side, stripSide{Provider: "Codex", Accent: codexChartAccent}, now)
	out := string(svg)
	var root struct {
		XMLName xml.Name `xml:"svg"`
	}
	if err := xml.Unmarshal(svg, &root); err != nil {
		t.Fatalf("strip is not valid SVG: %v", err)
	}
	// The row is the card plus the margin drawn above and below it.
	if want := stripHeight(3) + 2*chartMargin; h != want {
		t.Fatalf("height %v, want the three-row height %v", h, want)
	}
	if !strings.Contains(out, fmt.Sprintf(`width="%.0f" height="%.0f"`, pairedChartWidth, h)) {
		t.Fatalf("the image should be as wide as the charts: %.120s", out)
	}
	for _, row := range []string{"5H", "7D", "CR"} {
		if got := countSubstr(out, `data-strip-row="`+row+`"`); got != 1 {
			t.Fatalf("want one %s row, got %d", row, got)
		}
	}
	// The countdown is plain clock time to the reset, not the working-hours
	// pace clock the speed is measured on.
	if !strings.Contains(out, `data-strip-reset="2h00m"`) || !strings.Contains(out, `data-strip-reset="5d 13h"`) {
		t.Fatalf("resets should be spelled out per window: %s", out)
	}
	// Every countdown is paired with the day and time it lands on, stamped
	// from the moment the strip was rendered for.
	for _, want := range []string{
		`data-strip-clock="Thu 14:00"`, // 2h from Thursday noon
		`data-strip-clock="Wed 01:00"`, // 5d 13h out, still inside the week
		`data-strip-clock="Thu 13:00"`, // the credit balance's own reset
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing reset stamp %s: %s", want, out)
		}
	}
	// 60% left over the two working hours to a 14:00 reset affords 30%/h, so a
	// 10%/h reading is a third of it; the idle 7d window has nothing measured.
	if !strings.Contains(out, `data-speed-ratio="0.333"`) {
		t.Fatalf("the measured window should carry its affordable-pace ratio: %s", out)
	}
	if got := countSubstr(out, `data-speed-needle="true"`); got != 2 {
		t.Fatalf("want a marker only for the two measured rows, got %d", got)
	}
	if got := countSubstr(out, `data-pace-mark="true"`); got != 2 {
		t.Fatalf("want a nominal-pace pip per quota row and none for credits, got %d", got)
	}
	if got := countSubstr(out, `data-trend-value="true"`); got != 2 {
		t.Fatalf("want a trend value per row that has one, got %d", got)
	}
	if !strings.Contains(out, `data-strip-runway="6H"`) {
		t.Fatalf("the runway at the measured rate should be stated: %s", out)
	}
}

func TestQuotaStripStaysOneRowTallWhenNothingIsKnown(t *testing.T) {
	now := time.Now()
	svg, h := renderQuotaStrip(stripSide{Provider: "Claude"}, stripSide{Provider: "Codex"}, now)
	if want := stripHeight(1) + 2*chartMargin; h != want {
		t.Fatalf("empty strip height %v, want %v", h, want)
	}
	if strings.Contains(string(svg), "data-strip-row") {
		t.Fatal("an unknown provider should contribute no rows")
	}
}
