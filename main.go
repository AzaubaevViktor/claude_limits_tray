// Command claude-limits is a macOS menu-bar monitor for Claude and Codex usage limits.
//
// A paired SVG icon shows Claude on the left and Codex on the right. Each dial
// has two concentric rings (short window left, long window right; outer = %
// left, inner = time since reset) plus a centre dot that reports the worst
// since-reset pace category across the two outer quota rings.
//
// This is a Go port of the Python app; the dial is emitted as SVG and the menu
// bar is driven by getlantern/systray.
package main

import "github.com/getlantern/systray"

func main() {
	app := newApp()
	systray.Run(app.onReady, app.onExit)
}
