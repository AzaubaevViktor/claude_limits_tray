package main

import (
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/getlantern/systray"
)

func (a *App) buildMenu() {
	a.account = systray.AddMenuItem("Account", "")
	a.plan = systray.AddMenuItem("Plan", "")
	systray.AddSeparator()
	a.session = systray.AddMenuItem("Session", "")
	a.sessionReset = systray.AddMenuItem("Session resets", "")
	a.weekly = systray.AddMenuItem("Weekly", "")
	a.weeklyReset = systray.AddMenuItem("Weekly resets", "")
	systray.AddSeparator()

	a.burn = systray.AddMenuItem("Forecast @ current rate", "")
	a.burnSession = a.burn.AddSubMenuItem("Forecast session", "")
	a.burnWeekly = a.burn.AddSubMenuItem("Forecast weekly", "")
	a.speed = a.burn.AddSubMenuItem("Speed", "")

	a.scopedParent = systray.AddMenuItem("Weekly by model: —", "")
	for i := 0; i < scopedPoolSize; i++ {
		it := a.scopedParent.AddSubMenuItem("", "")
		it.Hide()
		a.scoped = append(a.scoped, it)
	}

	a.legendParent = systray.AddMenuItem("Legend (the dial)", "")
	for _, row := range legendRows() {
		a.legendParent.AddSubMenuItem(row[0]+":  "+row[1], "")
	}
	systray.AddSeparator()

	a.buildWorkMenu()
	systray.AddSeparator()

	a.status = systray.AddMenuItem("Status", "")
	a.ok = systray.AddMenuItem("Last success: —", "")
	a.fail = systray.AddMenuItem("Last failure: —", "")
	refresh := systray.AddMenuItem("Refresh now", "")
	quit := systray.AddMenuItem("Quit", "")

	a.onClick(refresh, func() {
		a.nextPoll = time.Time{} // poll on the next tick
		if !a.inflight {
			a.startRequest()
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
