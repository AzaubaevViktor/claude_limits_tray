package main

import "github.com/getlantern/systray"

// theme.go keeps the menu images legible in both macOS appearances. The cards
// sit on the dropdown's own background — near-black in dark mode, near-white in
// light — so the fixed dark palette they used to carry floated over a light
// menu as a black slab. Only the neutrals change: the speed ramp (purple →
// green → yellow → red) is the reading itself and means the same either way.
type chartTheme struct {
	Panel   [3]int // card fill
	Plot    [3]int // the plot area inside a card
	Text    [3]int // the headline figures
	Label   [3]int // row labels
	Dim     [3]int // secondary labels and units
	Caption [3]int // column captions
	Faint   [3]int // separators
	Track   [3]int // an empty bar's track
	Grid    [3]int // vertical gridlines
	Axis    [3]int // axis lines, their labels and the reset marker
	AxisTxt [3]int
	// Night is how strongly the off-hours bands darken the plot. A light card
	// needs a much lighter veil than a dark one to read as "same plot, dimmer".
	Night float64
	Dark  bool
}

var darkChartTheme = chartTheme{
	Panel:   [3]int{28, 28, 30},
	Plot:    [3]int{48, 48, 52},
	Text:    [3]int{236, 236, 241},
	Label:   [3]int{150, 150, 158},
	Dim:     [3]int{140, 140, 148},
	Caption: [3]int{132, 132, 140},
	Faint:   [3]int{120, 120, 128},
	Track:   [3]int{72, 72, 78},
	Grid:    [3]int{150, 150, 155},
	Axis:    [3]int{185, 185, 190},
	AxisTxt: [3]int{165, 165, 172},
	Night:   0.24,
	Dark:    true,
}

// lightChartTheme mirrors the dark one about the background: the card is a
// shade off white, the ink tiers run dark-to-light instead of light-to-dark,
// and the axis furniture is darkened so the same stroke opacities still read.
var lightChartTheme = chartTheme{
	Panel:   [3]int{247, 247, 249},
	Plot:    [3]int{226, 226, 232},
	Text:    [3]int{28, 28, 30},
	Label:   [3]int{104, 104, 112},
	Dim:     [3]int{112, 112, 120},
	Caption: [3]int{118, 118, 126},
	Faint:   [3]int{152, 152, 160},
	Track:   [3]int{206, 206, 214},
	Grid:    [3]int{96, 96, 104},
	Axis:    [3]int{72, 72, 80},
	AxisTxt: [3]int{104, 104, 112},
	Night:   0.10,
}

// chartPalette is what every menu image draws its neutrals from. It follows the
// system appearance (see refreshChartTheme) and starts dark, which is what a
// menu-bar app most often sits in.
var chartPalette = darkChartTheme

// refreshChartTheme re-reads the system appearance and reports whether it
// changed, so the caller can repaint what is already on screen.
func refreshChartTheme() bool {
	want := darkChartTheme
	if !systray.IsDarkMode() {
		want = lightChartTheme
	}
	if want.Dark == chartPalette.Dark {
		return false
	}
	chartPalette = want
	return true
}
