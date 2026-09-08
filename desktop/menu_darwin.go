//go:build darwin

package desktop

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa
#include <stdlib.h>
int howl_install_menu(const char *name);
*/
import "C"

import (
	"log/slog"
	"unsafe"
)

// installMenu gives the window the standard macOS menu bar. See menu_darwin.m
// for why it is not optional: with no menu there is no ⌘C either.
//
// Called after webview.New, which is what creates the NSApplication, and
// before Run, which is what starts its loop. On the main thread, because init
// locks this goroutine to it.
func installMenu(title string) {
	if title == "" {
		title = "App"
	}
	c := C.CString(title)
	defer C.free(unsafe.Pointer(c))
	n := C.howl_install_menu(c)
	slog.Debug("desktop: menu bar", "items", int(n))
}
