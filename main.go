// Command claude-limits is a macOS menu-bar monitor for Claude and Codex usage limits.
//
// A circular icon shows two concentric rings (session left, weekly right;
// outer = % left, inner = time since reset) plus a centre Claude spark whose
// colour forecasts whether you'll run out of quota at your current rate. The
// dropdown spells everything out and includes a separate Codex usage block.
//
// This is a Go port of the Python app; the dial is drawn with fogleman/gg and
// the menu bar is driven by getlantern/systray.
package main

import "github.com/getlantern/systray"

func main() {
	app := newApp()
	systray.Run(app.onReady, app.onExit)
}
