//go:build !darwin

package desktop

// installMenu is macOS-only. GTK has no application-wide menu bar to install
// into, and WebKitGTK already binds Ctrl+C/V/X and Ctrl+A inside the page
// itself — which is the gap the macOS side exists to close.
func installMenu(string) {}
