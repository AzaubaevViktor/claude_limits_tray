package systray

/*
#cgo darwin CFLAGS: -DDARWIN -x objective-c -fobjc-arc
#cgo darwin LDFLAGS: -framework Cocoa -framework WebKit

#include "systray.h"
*/
import "C"

import (
	"unsafe"
)

// SetTemplateIcon sets the systray icon as a template icon (on Mac), falling back
// to a regular icon on other platforms.
// templateIconBytes and regularIconBytes should be the content of .ico for windows and
// .ico/.jpg/.png for other platforms.
func SetTemplateIcon(templateIconBytes []byte, regularIconBytes []byte) {
	cstr := (*C.char)(unsafe.Pointer(&templateIconBytes[0]))
	C.setIcon(cstr, (C.int)(len(templateIconBytes)), true)
}

// SetIconHeight sets the menu-bar icon height in points (macOS only). Takes
// effect on the next SetIcon call.
func SetIconHeight(h float64) {
	C.setIconHeight(C.double(h))
}

// ShowSparkOverlay paints svg (or any image the system can decode) in a
// borderless, click-through window of width x height points centred on the
// menu-bar icon and layered above the menu bar — for animation that has to
// leave the icon's own rectangle. Repeated calls just swap the image; call
// HideSparkOverlay to take the window down. A no-op while the status item
// itself is hidden.
func ShowSparkOverlay(iconBytes []byte, width, height float64) {
	if len(iconBytes) == 0 {
		return
	}
	cstr := (*C.char)(unsafe.Pointer(&iconBytes[0]))
	C.showSparkOverlay(cstr, (C.int)(len(iconBytes)), C.double(width), C.double(height))
}

// HideSparkOverlay takes the overlay window down.
func HideSparkOverlay() {
	C.hideSparkOverlay()
}

// MainScreenHeight is the height in points of the screen that holds the menu
// bar, or 0 if it can't be determined.
func MainScreenHeight() float64 {
	return float64(C.mainScreenHeight())
}

// SetIcon sets the icon of a menu item. Only works on macOS and Windows.
// iconBytes should be the content of .ico/.jpg/.png
func (item *MenuItem) SetIcon(iconBytes []byte) {
	cstr := (*C.char)(unsafe.Pointer(&iconBytes[0]))
	C.setMenuItemIcon(cstr, (C.int)(len(iconBytes)), C.int(item.id), false)
}

// SetIconSize sets a menu item's icon at an explicit point size. This is useful
// for wide inline artwork such as charts; SetIcon retains the upstream 16x16
// behaviour for ordinary menu glyphs.
func (item *MenuItem) SetIconSize(iconBytes []byte, width, height float64) {
	cstr := (*C.char)(unsafe.Pointer(&iconBytes[0]))
	C.setMenuItemIconSize(cstr, (C.int)(len(iconBytes)), C.int(item.id), C.double(width), C.double(height), false)
}

// SetTemplateIcon sets the icon of a menu item as a template icon (on macOS). On Windows, it
// falls back to the regular icon bytes and on Linux it does nothing.
// templateIconBytes and regularIconBytes should be the content of .ico for windows and
// .ico/.jpg/.png for other platforms.
func (item *MenuItem) SetTemplateIcon(templateIconBytes []byte, regularIconBytes []byte) {
	cstr := (*C.char)(unsafe.Pointer(&templateIconBytes[0]))
	C.setMenuItemIcon(cstr, (C.int)(len(templateIconBytes)), C.int(item.id), true)
}
