package main

import (
	"math"

	"github.com/getlantern/systray"
)

// Dynamic menu-bar icon sizing: while tokens are actively being consumed at any
// provider/window, the icon grows. Opt-out (on by default); when off, the manual
// "Icon size" setting applies instead.
const (
	dynIconMin = 18.0 // idle height (no active consumption)
	dynIconMax = 24.0 // height at/above the reference burn

	// dynRefProjectedPct: a sustained rate that would consume this share of a
	// window's whole quota maxes the icon. 25% is 5%/h over the 5-hour session
	// (the specified reference); every window is scored on its OWN length, so a
	// fast weekly burn and a fast 5h burn are equally "active" (see task #11).
	dynRefProjectedPct = 25.0
)

// windowBurnLevel normalizes a consumption rate (quota %/sec) against its window
// length (seconds) to 0..1: the share of a full window the current pace projects
// to, over the reference, capped at 1. 0 when idle.
func windowBurnLevel(ratePerSec, windowSecs float64) float64 {
	if ratePerSec <= 0 || windowSecs <= 0 {
		return 0
	}
	return clamp(ratePerSec*windowSecs/dynRefProjectedPct, 0, 1)
}

// dynamicIconHeight maps a 0..1 burn level to a height between the idle and the
// max dynamic sizes.
func dynamicIconHeight(level float64) float64 {
	return dynIconMin + (dynIconMax-dynIconMin)*clamp(level, 0, 1)
}

// burnLevel is the worst normalized burn across every provider window — Claude
// session and weekly, Codex primary and secondary — each scored on its own
// window length (task #11). 0 when nothing is moving. (Per-model weekly limits
// share the weekly window length, so the aggregate weekly rate represents them.)
func (a *App) burnLevel() float64 {
	level := 0.0
	if a.lastUsage != nil {
		level = math.Max(level, windowBurnLevel(a.seriesRate(seriesClaudeSession).Rate, fiveHourSecs))
		level = math.Max(level, windowBurnLevel(a.seriesRate(seriesClaudeWeekly).Rate, weekSecs))
	}
	if a.lastCodex != nil {
		if p := a.lastCodex.primary(); p != nil {
			level = math.Max(level, windowBurnLevel(a.seriesRate(seriesCodexPrimary).Rate, float64(p.WindowSeconds)))
		}
		if s := a.lastCodex.secondary(); s != nil {
			level = math.Max(level, windowBurnLevel(a.seriesRate(seriesCodexSecondary).Rate, float64(s.WindowSeconds)))
		}
	}
	return level
}

// targetIconHeight is the manual size when auto-size is off, otherwise the
// burn-driven dynamic height.
func (a *App) targetIconHeight() float64 {
	if a.dynIconOff {
		return a.iconHeight
	}
	return dynamicIconHeight(a.burnLevel())
}

// applyIconHeight pushes the target height to systray, skipping no-op updates so
// it can be called every tick.
func (a *App) applyIconHeight() {
	h := a.targetIconHeight()
	if h == a.appliedIconHeight {
		return
	}
	a.appliedIconHeight = h
	systray.SetIconHeight(h)
}
