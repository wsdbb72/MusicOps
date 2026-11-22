//go:build darwin

package desktop

// macOS owns the Dock and menu-bar icon through the application bundle.
// Wails uses the bundle icon automatically, so there is no native window icon
// correction to perform here.
func applyNativeWindowIcon() error { return nil }
