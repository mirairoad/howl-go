// Package desktop opens a howl application in an OS-native window instead of a
// browser: WKWebView on macOS, WebKitGTK on Linux. There is no bundled runtime
// and no second language — the window is a few hundred kilobytes of C++ shim
// over a webview the OS already ships, and the "server" is the same *app.App
// this binary would otherwise Listen with, on a loopback socket nobody else can
// reach.
//
// It lives in its own module because it needs cgo and a webview binding, and
// the framework's go.mod has exactly one dependency. Nothing in core imports
// this; an application opts in by importing it from its own main.
package desktop

import (
	"fmt"
	"net"
	"net/http"
	"runtime"
	"time"

	webview "github.com/webview/webview_go"

	"github.com/mirairoad/howl-go/core/app"
)

// Cocoa and GTK both require every window call to happen on the thread that
// initialised them, and Go will otherwise migrate this goroutine to whichever
// OS thread is free. Locking in init covers main, which is the only goroutine
// allowed to call Run.
func init() { runtime.LockOSThread() }

// Options is the window. Everything is optional.
type Options struct {
	Title  string
	Width  int
	Height int
	// MinWidth and MinHeight are the floor a user can drag the window to. Worth
	// setting: without one the window resizes to nothing, and a layout that
	// reflows correctly at every width still has a width below which it stops
	// reflowing and starts clipping.
	MinWidth  int
	MinHeight int
	// MaxWidth and MaxHeight are the ceiling. Usually leave them zero — a
	// desktop window that refuses to fill the screen it was dragged onto is a
	// worse answer than a layout that centres its content.
	MaxWidth  int
	MaxHeight int
	// Fixed forbids resizing altogether, ignoring the four bounds above. For a
	// panel or a launcher, not for an application window.
	Fixed bool
	// Attach points the window at a server that is already running instead of
	// starting one. This is the `howl dev` path: the dev server's front door
	// stays up across rebuilds, so the window connects once, keeps its
	// EventSource, and reloads its content on every rebuild rather than being
	// killed and respawned. Both the application and the handler are ignored.
	Attach string
	// Debug enables the webview's inspector — right-click, Inspect Element on
	// both platforms. Off by default: webview only sets developerExtrasEnabled
	// when it is asked to, so a shipped binary has no inspector at all.
	Debug bool
	// ContextMenu keeps the webview's own right-click menu. It is suppressed by
	// default because what it offers is Reload, Back, Forward and Services —
	// browser affordances in something that is not presenting itself as a
	// browser. Debug implies this: an inspector you cannot right-click into is
	// no inspector.
	ContextMenu bool
}

// Run serves h and opens a window on it. It blocks until the window is closed
// and is the last line of main, like a.Listen — but unlike a.Listen it returns
// nil when that happens, because closing a window is how the user quits. Do not
// wrap it in log.Fatal.
//
// The listener is 127.0.0.1 on a port the kernel picks. A fixed port would be
// a second instance's failure and, worse, something any other process on the
// machine could talk to; the window is the only thing that ever learns this
// number.
func Run(a *app.App, h http.Handler, opt Options) error {
	url := opt.Attach
	if url == "" {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		// http.Serve in the background: the socket is already listening and the
		// kernel queues the window's first connection, so there is no race
		// between Navigate and the first Accept.
		go a.Serve(ln, h) //nolint:errcheck // the window closing is the exit path
		url = "http://" + ln.Addr().String()
	} else {
		full, addr, err := normalize(opt.Attach)
		if err != nil {
			return err
		}
		if err := wait(addr, 10*time.Second); err != nil {
			return err
		}
		url = full
	}

	w := webview.New(opt.Debug)
	defer w.Destroy()
	if !opt.ContextMenu && !opt.Debug {
		w.Init(appChrome)
	}
	if opt.Title != "" {
		w.SetTitle(opt.Title)
	}
	// Bounds before the size, so a Width below MinWidth lands clamped into a
	// legal window rather than one the user cannot drag back out of.
	if opt.MinWidth > 0 && opt.MinHeight > 0 {
		w.SetSize(opt.MinWidth, opt.MinHeight, webview.HintMin)
	}
	if opt.MaxWidth > 0 && opt.MaxHeight > 0 {
		w.SetSize(opt.MaxWidth, opt.MaxHeight, webview.HintMax)
	}
	if opt.Width > 0 && opt.Height > 0 {
		// The constants are untyped ints, so the type has to be named here.
		hint := webview.Hint(webview.HintNone)
		if opt.Fixed {
			hint = webview.HintFixed
		}
		w.SetSize(opt.Width, opt.Height, hint)
	}
	w.Navigate(url)
	w.Run()
	return nil
}

// wait blocks until the address answers. Without it, a window started next to
// `howl dev` races the first build and lands on a connection-refused page that
// no rebuild will ever reload, because the dev client that would have reloaded
// it was never served.
func wait(addr string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("desktop: %s did not answer within %s: %w", addr, limit, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// appChrome is injected before every page load, including after a dev reload.
//
// The dropped-file guard is not cosmetic: a file dragged onto any webview
// navigates it to file:// by default, which in a window with no address bar is
// an app that has silently become a file viewer with no way back.
const appChrome = `
addEventListener("contextmenu", (e) => e.preventDefault());
addEventListener("dragover", (e) => e.preventDefault());
addEventListener("drop", (e) => e.preventDefault());
`
