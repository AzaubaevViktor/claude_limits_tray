package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/getlantern/systray"
)

func (a *App) buildMenu() {
	// Burn history sits at the very top of the menu so the charts are visible
	// without drilling into a submenu.
	a.histHeader = systray.AddMenuItem("Burn history · dashed = affordable pace (left ÷ working time to reset)", "")
	a.histHeader.Disable()
	a.histHour = systray.AddMenuItem("", "")
	a.histSession = systray.AddMenuItem("", "")
	a.histWeek = systray.AddMenuItem("", "")
	a.histCredits = systray.AddMenuItem("", "")
	systray.AddSeparator()

	a.claudeParent = systray.AddMenuItem("Claude · —", "")
	a.account = a.claudeParent.AddSubMenuItem("Account: —", "")
	a.claudeSwitchAuth = a.claudeParent.AddSubMenuItem("Use current Claude login", "")
	a.onClick(a.claudeSwitchAuth, func() { a.useCurrentClaudeLogin() })
	a.claudeRefreshNow = a.claudeParent.AddSubMenuItem("Refresh Claude token now", "")
	a.onClick(a.claudeRefreshNow, func() { a.refreshClaudeTokenNow() })
	a.plan = a.claudeParent.AddSubMenuItem("Plan", "")
	a.session = a.claudeParent.AddSubMenuItem("Session", "")
	a.sessionReset = a.claudeParent.AddSubMenuItem("Session resets", "")
	a.weekly = a.claudeParent.AddSubMenuItem("Weekly", "")
	a.weeklyReset = a.claudeParent.AddSubMenuItem("Weekly resets", "")
	a.weeklyBlocks = a.claudeParent.AddSubMenuItem("5h blocks", "")
	a.weeklyBlocksBasis = a.claudeParent.AddSubMenuItem("basis", "")
	a.claudeCredits = a.claudeParent.AddSubMenuItem("💳 Credits: —", "")
	a.claudeCreditsRunway = a.claudeParent.AddSubMenuItem("", "")
	a.claudeCreditsRunway.Hide()
	a.claudeResets = a.claudeParent.AddSubMenuItem("Resets available: —", "")
	a.burn = a.claudeParent.AddSubMenuItem("Forecast @ current rate", "")
	a.burnSession = a.burn.AddSubMenuItem("Forecast session", "")
	a.burnWeekly = a.burn.AddSubMenuItem("Forecast weekly", "")
	a.speed = a.burn.AddSubMenuItem("Speed (15m)", "")
	a.speedLeft = a.burn.AddSubMenuItem("Spare at reset (15m)", "")
	a.speedAvg = a.burn.AddSubMenuItem("Speed (since reset)", "")
	a.speedAvgLeft = a.burn.AddSubMenuItem("Spare at reset (since reset)", "")
	a.scopedParent = a.claudeParent.AddSubMenuItem("Weekly by model: —", "")
	for i := 0; i < scopedPoolSize; i++ {
		it := a.scopedParent.AddSubMenuItem("", "")
		it.Disable()
		it.Hide()
		a.scoped = append(a.scoped, it)
	}

	a.codexParent = systray.AddMenuItem("Codex · —", "")
	a.codexAccount = a.codexParent.AddSubMenuItem("Account: —", "")
	a.codexSwitchAuth = a.codexParent.AddSubMenuItem("Use current Codex login", "")
	a.onClick(a.codexSwitchAuth, func() { a.useCurrentCodexLogin() })
	a.codexPlan = a.codexParent.AddSubMenuItem("Plan: —", "")
	a.codexPrimary = a.codexParent.AddSubMenuItem("Primary", "")
	a.codexPrimaryReset = a.codexParent.AddSubMenuItem("Primary resets", "")
	a.codexSecondary = a.codexParent.AddSubMenuItem("Secondary", "")
	a.codexSecondaryReset = a.codexParent.AddSubMenuItem("Secondary resets", "")
	a.codexBurn = a.codexParent.AddSubMenuItem("Forecast @ current rate", "")
	a.codexBurnPrimary = a.codexBurn.AddSubMenuItem("Forecast primary", "")
	a.codexBurnSecondary = a.codexBurn.AddSubMenuItem("Forecast secondary", "")
	a.codexSpeed = a.codexBurn.AddSubMenuItem("Speed (15m)", "")
	a.codexSpeedLeft = a.codexBurn.AddSubMenuItem("Spare at reset (15m)", "")
	a.codexSpeedAvg = a.codexBurn.AddSubMenuItem("Speed (since reset)", "")
	a.codexSpeedAvgLeft = a.codexBurn.AddSubMenuItem("Spare at reset (since reset)", "")
	a.codexCredits = a.codexParent.AddSubMenuItem("Credits: —", "")
	a.codexCreditsRunway = a.codexParent.AddSubMenuItem("", "")
	a.codexCreditsRunway.Hide()
	a.codexResets = a.codexParent.AddSubMenuItem("Resets available: —", "")
	a.codexScopedParent = a.codexParent.AddSubMenuItem("Additional limits: —", "")
	for i := 0; i < scopedPoolSize; i++ {
		it := a.codexScopedParent.AddSubMenuItem("", "")
		it.Disable()
		it.Hide()
		a.codexScoped = append(a.codexScoped, it)
	}

	a.legendParent = systray.AddMenuItem("Legend (the dial)", "")
	for _, row := range legendRows() {
		it := a.legendParent.AddSubMenuItem(row[0]+":  "+row[1], "")
		it.Disable()
		a.legendItems = append(a.legendItems, it)
	}

	a.timeFillParent = systray.AddMenuItem("Time ring: "+timeFillString(), "")
	a.timeFillRemaining = a.timeFillParent.AddSubMenuItemCheckbox(
		"Fill = time left", "", timeFill == fillRemaining)
	a.timeFillElapsed = a.timeFillParent.AddSubMenuItemCheckbox(
		"Fill = time elapsed", "", timeFill == fillElapsed)
	a.onClick(a.timeFillRemaining, func() { a.setTimeFill(fillRemaining) })
	a.onClick(a.timeFillElapsed, func() { a.setTimeFill(fillElapsed) })

	a.pulseParent = systray.AddMenuItem("Spend pulse: "+pulseModeString(), "")
	for _, o := range pulseModeOrder {
		mode := o.mode
		it := a.pulseParent.AddSubMenuItemCheckbox(o.label, "", mode == pulseFX)
		a.pulseItems = append(a.pulseItems, it)
		a.onClick(it, func() { a.setPulseMode(mode) })
	}

	a.pulseTest = a.pulseParent.AddSubMenuItem("Test it now", "")
	setEnabled(a.pulseTest, pulseFX != pulseOff)
	a.onClick(a.pulseTest, func() { a.firePulseTest() })

	a.weekSmoothParent = systray.AddMenuItem("7d smoothing: "+weekSmoothLabel(a.weekSmooth), "")
	for _, o := range weekSmoothOptions {
		key := o.key
		it := a.weekSmoothParent.AddSubMenuItemCheckbox(o.label, "", key == a.weekSmooth)
		a.weekSmoothItems = append(a.weekSmoothItems, it)
		a.onClick(it, func() { a.setWeekSmooth(key) })
	}

	a.buildIconSizeMenu()
	systray.AddSeparator()

	a.buildWorkMenu()
	systray.AddSeparator()

	a.nextReset = systray.AddMenuItem("⏱ Next reset: —", "")
	a.status = systray.AddMenuItem("Status", "")
	a.claudeOk = systray.AddMenuItem("Claude ✓ last success: —", "")
	a.claudeFail = systray.AddMenuItem("Claude ✗ last failure: —", "")
	a.codexOk = systray.AddMenuItem("Codex ✓ last success: —", "")
	a.codexFail = systray.AddMenuItem("Codex ✗ last failure: —", "")
	refresh := systray.AddMenuItem("Refresh now", "")
	quit := systray.AddMenuItem("Quit", "")

	// the info-only rows are display, not buttons — grey them out so they
	// don't highlight on hover (submenu parents stay active so they open)
	for _, it := range []*systray.MenuItem{
		a.account, a.plan, a.session, a.sessionReset, a.weekly, a.weeklyReset,
		a.weeklyBlocks, a.weeklyBlocksBasis, a.claudeCredits, a.claudeCreditsRunway, a.claudeResets,
		a.burnSession, a.burnWeekly, a.speed, a.speedLeft, a.speedAvg, a.speedAvgLeft,
		a.histHour, a.histSession, a.histWeek, a.histCredits,
		a.nextReset, a.status, a.claudeOk, a.claudeFail, a.codexOk, a.codexFail,
		a.codexAccount, a.codexPlan, a.codexPrimary, a.codexPrimaryReset, a.codexSecondary,
		a.codexSecondaryReset, a.codexBurnPrimary, a.codexBurnSecondary,
		a.codexSpeed, a.codexSpeedLeft, a.codexSpeedAvg, a.codexSpeedAvgLeft,
		a.codexCredits, a.codexCreditsRunway, a.codexResets,
	} {
		it.Disable()
	}

	a.onClick(refresh, func() {
		a.nextPoll = time.Time{} // poll both providers, one after the other
		a.nextCodexPoll = time.Time{}
		if !a.inflight {
			a.startDueRequest()
		}
	})
	go func() {
		<-quit.ClickedCh
		a.mu.Lock()
		a.saveState(true)
		a.mu.Unlock()
		systray.Quit()
	}()
}

func (a *App) buildIconSizeMenu() {
	a.iconSizeParent = systray.AddMenuItem("Icon size", "")
	a.dynIconItem = a.iconSizeParent.AddSubMenuItemCheckbox(
		fmt.Sprintf("Grow while burning (%d→%d pt)", int(dynIconMin), int(dynIconMax)), "", !a.dynIconOff)
	a.onClick(a.dynIconItem, func() { a.setDynIcon(a.dynIconOff) })
	for pt := int(minIconHeight); pt <= int(maxIconHeight); pt++ {
		pt := pt
		it := a.iconSizeParent.AddSubMenuItemCheckbox(iconSizeLabel(pt), "", pt == iround(a.iconHeight))
		a.iconSizeItems = append(a.iconSizeItems, hourItem{it, pt})
		a.onClick(it, func() { a.setIconHeight(float64(pt)) })
	}
	a.refreshIconSizeMenu()
}

// iconSizeLabel marks the default height.
func iconSizeLabel(pt int) string {
	if pt == int(defaultIconHeight) {
		return fmt.Sprintf("%d pt · default", pt)
	}
	return fmt.Sprintf("%d pt", pt)
}

func (a *App) refreshIconSizeMenu() {
	if a.dynIconOff {
		a.iconSizeParent.SetTitle(fmt.Sprintf("Icon size: %d pt", iround(a.iconHeight)))
	} else {
		a.iconSizeParent.SetTitle(fmt.Sprintf("Icon size: auto %d–%d pt", int(dynIconMin), int(dynIconMax)))
	}
	if a.dynIconItem != nil {
		setCheck(a.dynIconItem, !a.dynIconOff)
	}
	for _, hi := range a.iconSizeItems {
		setCheck(hi.item, hi.min == iround(a.iconHeight))
	}
}

// setIconHeight changes the manual menu-bar icon height. When auto-size is on
// the change is stored but only takes visible effect once auto-size is turned
// off; applyIconHeight decides what actually reaches systray.
func (a *App) setIconHeight(h float64) {
	a.iconHeight = h
	a.refreshIconSizeMenu()
	a.iconKey = "" // force a repaint — the SVG is unchanged, only its scale
	a.applyIconHeight()
	a.renderIcon()
	a.saveState(true)
}

// setDynIcon toggles the burn-driven auto-size (opt-out; on by default).
func (a *App) setDynIcon(on bool) {
	a.dynIconOff = !on
	a.refreshIconSizeMenu()
	a.iconKey = "" // force a repaint at the new scale
	a.applyIconHeight()
	a.renderIcon()
	a.saveState(true)
}

func (a *App) buildWorkMenu() {
	a.work = systray.AddMenuItem("Working hours", "")
	startMenu := a.work.AddSubMenuItem("Start", "")
	endMenu := a.work.AddSubMenuItem("End", "")
	for m := 0; m < 24*60; m += 30 {
		mm := m
		si := startMenu.AddSubMenuItemCheckbox(fmtHHMM(m), "", m == a.workStart)
		ei := endMenu.AddSubMenuItemCheckbox(fmtHHMM(m), "", m == a.workEnd)
		a.startItems = append(a.startItems, hourItem{si, mm})
		a.endItems = append(a.endItems, hourItem{ei, mm})
		a.onClick(si, func() { a.workStart = mm; a.afterWorkChange() })
		a.onClick(ei, func() { a.workEnd = mm; a.afterWorkChange() })
	}
	a.refreshWorkMenu()
}

func (a *App) refreshWorkMenu() {
	a.work.SetTitle("Working hours: " + fmtHHMM(a.workStart) + "–" + fmtHHMM(a.workEnd))
	for _, hi := range a.startItems {
		setCheck(hi.item, hi.min == a.workStart)
	}
	for _, hi := range a.endItems {
		setCheck(hi.item, hi.min == a.workEnd)
	}
}

func (a *App) afterWorkChange() {
	a.refreshWorkMenu()
	a.saveState(true)
	if a.lastUsage != nil && !a.errorOn {
		a.apply()
	}
	// Off-hours shading on the Codex burn charts also follows the work window.
	if a.lastCodex != nil {
		a.renderCodex()
	}
}

// setTimeFill switches the inner (time) ring between filling with elapsed and
// remaining time. The colour is unchanged either way; only the arc differs.
func (a *App) setTimeFill(m fillMode) {
	timeFill = m
	a.timeFillParent.SetTitle("Time ring: " + timeFillString())
	setCheck(a.timeFillRemaining, timeFill == fillRemaining)
	setCheck(a.timeFillElapsed, timeFill == fillElapsed)
	a.renderLegend()
	a.saveState(true)
	a.renderIcon()
	if a.lastUsage != nil && !a.errorOn {
		a.apply()
	}
}

// setPulseMode picks how the dial reacts when quota % is spent: nothing, a
// flash of the centre dot and halo, or that plus sparks flying off the rim.
// Sparks need room around the dial, so switching them on/off changes the icon's
// width — force a repaint rather than trusting the cached key.
func (a *App) setPulseMode(m pulseMode) {
	pulseFX = m
	a.pulseParent.SetTitle("Spend pulse: " + pulseModeString())
	for i, o := range pulseModeOrder {
		setCheck(a.pulseItems[i], o.mode == pulseFX)
	}
	setEnabled(a.pulseTest, pulseFX != pulseOff) // nothing to show with it off
	a.clearPulseOverlay()                        // the sparks may have just been switched off
	a.renderLegend()
	a.iconKey = ""
	a.renderIcon()
	a.saveState(true)
}

func (a *App) renderLegend() {
	rows := legendRows()
	for i, it := range a.legendItems {
		if i < len(rows) {
			it.SetTitle(rows[i][0] + ":  " + rows[i][1])
		}
	}
}

// setWeekSmooth picks the 7d history row's blur width. The 1h and 5h rows
// are always smoothed relative to their horizon; the weekly row is where
// outliers matter most, so its width is a menu choice down to plain bars.
func (a *App) setWeekSmooth(key string) {
	a.weekSmooth = normWeekSmooth(key)
	a.weekSmoothParent.SetTitle("7d smoothing: " + weekSmoothLabel(a.weekSmooth))
	for i, o := range weekSmoothOptions {
		setCheck(a.weekSmoothItems[i], o.key == a.weekSmooth)
	}
	a.renderHistory()
	a.saveState(true)
}

func setEnabled(it *systray.MenuItem, on bool) {
	if on {
		it.Enable()
	} else {
		it.Disable()
	}
}

func setCheck(it *systray.MenuItem, on bool) {
	if on {
		it.Check()
	} else {
		it.Uncheck()
	}
}

// onClick wires a menu item's clicks to a handler, holding the app lock so the
// handler runs serialized with the tick loop.
func (a *App) onClick(it *systray.MenuItem, fn func()) {
	go func() {
		for range it.ClickedCh {
			a.mu.Lock()
			fn()
			a.mu.Unlock()
		}
	}()
}

func (a *App) signalLoop() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	<-c
	a.mu.Lock()
	a.saveState(true)
	a.mu.Unlock()
	systray.Quit()
}
