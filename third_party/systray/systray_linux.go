package systray

// SetTemplateIcon sets the systray icon as a template icon (on macOS), falling back
// to a regular icon on other platforms.
// templateIconBytes and iconBytes should be the content of .ico for windows and
// .ico/.jpg/.png for other platforms.
func SetTemplateIcon(templateIconBytes []byte, regularIconBytes []byte) {
	SetIcon(regularIconBytes)
}

// SetIcon sets the icon of a menu item. Only works on macOS and Windows.
// iconBytes should be the content of .ico/.jpg/.png
func (item *MenuItem) SetIcon(iconBytes []byte) {
}

// SetIconSize is a no-op on Linux, matching SetIcon.
func (item *MenuItem) SetIconSize(iconBytes []byte, width, height float64) {
}

// SetImageView is a no-op on Linux, matching SetIcon.
func (item *MenuItem) SetImageView(iconBytes []byte, width, height float64) {
}

// MenuImageWidth has no measured row width to report on Linux.
func MenuImageWidth() float64 { return 0 }

// SetTemplateIcon sets the icon of a menu item as a template icon (on macOS). On Windows, it
// falls back to the regular icon bytes and on Linux it does nothing.
// templateIconBytes and regularIconBytes should be the content of .ico for windows and
// .ico/.jpg/.png for other platforms.
func (item *MenuItem) SetTemplateIcon(templateIconBytes []byte, regularIconBytes []byte) {
}
