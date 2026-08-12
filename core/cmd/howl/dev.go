package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mirairoad/howl-go/core/app"
	"github.com/mirairoad/howl-go/core/console"
)

func dev(args []string) error {
	fset := flag.NewFlagSet("dev", flag.ExitOnError)
	root := fset.String("dir", ".", "module root")
	addr := fset.String("addr", ":9000", "address to serve on — stays up across restarts")
	pagesDir := fset.String("pages", "client/pages", "page tree, relative to -dir")
	staticDir := fset.String("static", "client/public", "static files, served from disk in dev")
	module := fset.String("module", "", "import path of -pages (default: derived from go.mod)")
	pkg := fset.String("pkg", ".", "package to build")
	pre := fset.String("pre", "", "shell command to run before generating routes (e.g. a Markdown step)")
	poll := fset.Duration("poll", 300*time.Millisecond, "filesystem poll interval")
	appArgs := fset.String("args", "", "arguments passed to the built binary")
	fset.Parse(args)

	console.Setup(console.Options{})

	abs, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	if *module == "" {
		if *module, err = derivePagesImport(abs, *pagesDir); err != nil {
			return err
		}
	}

	d := &devServer{
		root:      abs,
		pages:     filepath.Join(abs, filepath.FromSlash(*pagesDir)),
		static:    filepath.Join(abs, filepath.FromSlash(*staticDir)),
		module:    *module,
		pkg:       *pkg,
		pre:       *pre,
		binary:    filepath.Join(abs, ".howl", "app"),
		appArgs:   strings.Fields(*appArgs),
		clients:   map[chan string]bool{},
		ready:     make(chan struct{}),
		firstPass: true,
		revision:  time.Now().UnixMilli(),
	}
	if d.childAddr, err = freePort(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(filepath.Dir(d.binary), 0o755); err != nil {
		return err
	}

	go d.serve(ctx, *addr)
	slog.Info("howl dev",
		slog.String("url", "http://localhost"+*addr),
		slog.String("watching", *root),
		slog.String("pages", *module),
	)

	d.rebuild("first build")
	d.watch(ctx, *poll)

	d.kill()
	return nil
}

// ---------------------------------------------------------------------------

type devServer struct {
	root, pages, static string
	module, pkg, pre    string
	binary              string
	childAddr           string
	appArgs             []string
	firstPass           bool

	mu       sync.Mutex
	child    *exec.Cmd
	buildErr string
	live     bool  // the child is up and answering
	revision int64 // monotonic; every reload-worthy change bumps it

	clientsMu sync.Mutex
	clients   map[chan string]bool
	ready     chan struct{}
}

// ---------------------------------------------------------------------------
// Watching
//
// Polling, not fsnotify. Walking a few hundred files costs well under a
// millisecond and keeps the module's dependency count where it is; fsnotify
// would also have to be taught about the atomic rename most editors use to
// save, which looks like "delete then create" rather than "write".
// ---------------------------------------------------------------------------

type stamp struct {
	mod  time.Time
	size int64
}

func (d *devServer) watch(ctx context.Context, every time.Duration) {
	prev := d.scan()
	tick := time.NewTicker(every)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		next := d.scan()
		changed := diff(prev, next)
		if len(changed) == 0 {
			continue
		}
		// An editor writing three files in one save should produce one rebuild,
		// not three. Keep draining until the tree stops moving.
		for {
			time.Sleep(120 * time.Millisecond)
			settled := d.scan()
			more := diff(next, settled)
			next = settled
			if len(more) == 0 {
				break
			}
			changed = append(changed, more...)
		}
		prev = next

		if onlyStatic(changed, d.static) {
			d.staticChanged(changed)
			continue
		}
		d.rebuild(reason(changed, d.root))
	}
}

func (d *devServer) scan() map[string]stamp {
	out := map[string]stamp{}
	filepath.WalkDir(d.root, func(path string, e fs.DirEntry, err error) error { //nolint:errcheck
		if err != nil {
			return nil
		}
		name := e.Name()
		if e.IsDir() {
			switch name {
			case ".git", ".howl", "node_modules", "dist", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !watched(name) {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return nil
		}
		out[path] = stamp{mod: info.ModTime(), size: info.Size()}
		return nil
	})
	return out
}

// watched skips everything this tool writes itself. Without that, generating
// the route table changes a file, which triggers a rebuild, which generates the
// route table: a loop that looks like the machine has caught fire.
func watched(name string) bool {
	switch {
	case strings.HasSuffix(name, "_templ.go"),
		strings.HasSuffix(name, "_gen.go"),
		name == "wasm_exec.js",
		strings.HasSuffix(name, ".wasm"),
		strings.HasPrefix(name, "."):
		return false
	}
	switch filepath.Ext(name) {
	case ".go", ".templ", ".css", ".js", ".md", ".html", ".json":
		return true
	}
	return false
}

func diff(a, b map[string]stamp) []string {
	var out []string
	for path, s := range b {
		if old, ok := a[path]; !ok || old != s {
			out = append(out, path)
		}
	}
	for path := range a {
		if _, ok := b[path]; !ok {
			out = append(out, path)
		}
	}
	return out
}

func onlyStatic(changed []string, staticDir string) bool {
	for _, p := range changed {
		if !strings.HasPrefix(p, staticDir+string(filepath.Separator)) {
			return false
		}
	}
	return len(changed) > 0
}

func reason(changed []string, root string) string {
	rel, err := filepath.Rel(root, changed[0])
	if err != nil {
		rel = changed[0]
	}
	if len(changed) > 1 {
		return fmt.Sprintf("%s +%d", rel, len(changed)-1)
	}
	return rel
}

// staticChanged handles the case that needs no Go at all. The child serves
// /static/ straight from disk in dev (HOWL_PUBLIC_DIR), so a stylesheet edit is
// already live — the browser just has to be told.
func (d *devServer) staticChanged(changed []string) {
	css := true
	for _, p := range changed {
		if filepath.Ext(p) != ".css" {
			css = false
			break
		}
	}
	if css {
		// Swapping the stylesheet keeps scroll position, focus and any open
		// dropdown — reloading the document for a colour change does not.
		slog.Info("css", slog.String("file", reason(changed, d.root)))
		d.broadcast("css")
		return
	}
	slog.Info("static", slog.String("file", reason(changed, d.root)))
	d.bump()
}

// ---------------------------------------------------------------------------
// Building
// ---------------------------------------------------------------------------

func (d *devServer) rebuild(why string) {
	start := time.Now()
	steps := []struct {
		name string
		run  func() ([]byte, error)
	}{
		{"pre", func() ([]byte, error) {
			if d.pre == "" {
				return nil, nil
			}
			return d.command("sh", "-c", d.pre)
		}},
		{"routes", func() ([]byte, error) {
			return d.command("go", "run", "github.com/mirairoad/howl-go/core/cmd/fsroutes",
				"-dir", d.relative(d.pages), "-module", d.module,
				"-out", filepath.Join(d.relative(d.pages), "fsroutes_gen.go"))
		}},
		{"templ", func() ([]byte, error) { return d.command("go", "tool", "templ", "generate") }},
		{"wasm", d.buildWasm},
		{"build", func() ([]byte, error) { return d.command("go", "build", "-o", d.binary, d.pkg) }},
	}

	for _, step := range steps {
		out, err := step.run()
		if err == nil {
			continue
		}
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		slog.Error("build failed", slog.String("step", step.name), slog.String("why", why))
		fmt.Fprintln(os.Stderr, msg)

		// Deliberately NOT killing the running child: the last binary that
		// compiled keeps serving. Reloading the browser onto a broken build
		// would replace a working page with a blank one, and the error is
		// already on screen in the overlay.
		d.mu.Lock()
		d.buildErr = step.name + ": " + msg
		d.mu.Unlock()
		d.broadcast("build-error")
		return
	}

	d.mu.Lock()
	recovered := d.buildErr != ""
	d.buildErr = ""
	d.mu.Unlock()

	built := time.Since(start)
	if err := d.restart(); err != nil {
		slog.Error("restart failed", slog.Any("err", err))
		return
	}
	slog.Info("rebuilt",
		slog.String("why", why),
		slog.Duration("build", console.Duration(built)),
		slog.Duration("total", console.Duration(time.Since(start))),
	)
	if recovered {
		d.broadcast("build-ok")
	}
	d.bump()
}

// buildWasm follows the framework's conventional ./wasm package when it is
// present. Client routes otherwise compile successfully on the server but fail
// at runtime with a missing views.wasm, which makes dev differ from `make` in a
// particularly confusing way.
func (d *devServer) buildWasm() ([]byte, error) {
	wasmDir := filepath.Join(d.root, "wasm")
	if info, err := os.Stat(wasmDir); err != nil || !info.IsDir() {
		return nil, nil
	}
	if err := os.MkdirAll(d.static, 0o755); err != nil {
		return nil, err
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(d.static, "views.wasm"), "./wasm")
	cmd.Dir = d.root
	cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, err
	}
	goroot, err := d.command("go", "env", "GOROOT")
	if err != nil {
		return goroot, err
	}
	source := filepath.Join(strings.TrimSpace(string(goroot)), "lib", "wasm", "wasm_exec.js")
	contents, err := os.ReadFile(source)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(d.static, "wasm_exec.js"), contents, 0o644); err != nil {
		return nil, err
	}
	return out, nil
}

func (d *devServer) command(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = d.root
	return cmd.CombinedOutput()
}

func (d *devServer) relative(p string) string {
	rel, err := filepath.Rel(d.root, p)
	if err != nil {
		return p
	}
	return rel
}

// ---------------------------------------------------------------------------
// The child process
// ---------------------------------------------------------------------------

func (d *devServer) restart() error {
	d.kill()

	cmd := exec.Command(d.binary, d.appArgs...)
	cmd.Dir = d.root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// The application needs no dev-mode code of its own: core/app reads these
	// three and configures itself. HOWL_ADDR puts the child on the port the
	// proxy expects, HOWL_PUBLIC_DIR serves static files from disk instead of
	// the embedded copy, and HOWL_DEV makes the shell publish the live-reload
	// endpoint to the browser.
	cmd.Env = append(os.Environ(),
		"HOWL_ADDR="+d.childAddr,
		"HOWL_PUBLIC_DIR="+d.static,
		"HOWL_DEV=1",
	)
	if err := cmd.Start(); err != nil {
		return err
	}

	d.mu.Lock()
	d.child = cmd
	d.live = false
	d.mu.Unlock()

	go cmd.Wait() //nolint:errcheck // exits are expected; the next build starts another

	if err := waitFor(d.childAddr, 15*time.Second); err != nil {
		return err
	}
	d.mu.Lock()
	d.live = true
	d.mu.Unlock()
	d.signalReady()
	return nil
}

func (d *devServer) kill() {
	d.mu.Lock()
	child := d.child
	d.child = nil
	d.live = false
	d.mu.Unlock()
	if child == nil || child.Process == nil {
		return
	}
	// Ask first, insist after. A server mid-response deserves the chance to
	// finish it; one ignoring SIGTERM must not hold the port hostage.
	child.Process.Signal(syscall.SIGTERM) //nolint:errcheck
	done := make(chan struct{})
	go func() { child.Process.Wait(); close(done) }() //nolint:errcheck
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		child.Process.Kill() //nolint:errcheck
	}
}

func (d *devServer) signalReady() {
	d.clientsMu.Lock()
	defer d.clientsMu.Unlock()
	select {
	case <-d.ready:
	default:
		close(d.ready)
	}
}

func waitFor(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("the application never listened on %s", addr)
}

func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	return l.Addr().String(), nil
}

// ---------------------------------------------------------------------------
// The stable front door
//
// The browser talks to this, never to the child. So the port in the address bar
// keeps answering across restarts, a request that arrives mid-rebuild waits for
// the new binary instead of failing, and the live-reload connection survives
// the restart it is reporting.
// ---------------------------------------------------------------------------

func (d *devServer) serve(ctx context.Context, addr string) {
	target, _ := url.Parse("http://" + d.childAddr)
	proxy := httputil.NewSingleHostReverseProxy(target)
	// -1 flushes as it goes: without it an SSE stream from the application is
	// buffered by the proxy and never arrives.
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		d.mu.Lock()
		buildErr := d.buildErr
		d.mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, waitingPage, htmlEscape(firstNonEmpty(buildErr, err.Error())))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /_howl/alive", d.alive)
	// The routes the widget lists. Read from the generated table, so it shows
	// what the application actually serves rather than a list maintained
	// alongside it — and served by the dev server, so a production build has
	// no endpoint for it at all.
	mux.HandleFunc("GET /_howl/routes.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		body, err := asJSON(readPageRoutes(d.root))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write([]byte(body)) //nolint:errcheck
	})
	mux.HandleFunc("GET /_howl/alive.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(aliveJS) //nolint:errcheck
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		d.waitUntilLive(r.Context(), 20*time.Second)
		proxy.ServeHTTP(w, r)
	})

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		srv.Close() //nolint:errcheck
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("dev server", slog.Any("err", err))
		os.Exit(1)
	}
}

// waitUntilLive holds a request while the child is restarting. A page that
// takes 800ms once is better than a connection-refused the user has to reload
// past — and it is the reason the browser never sees the restart at all.
func (d *devServer) waitUntilLive(ctx context.Context, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		live := d.live
		d.mu.Unlock()
		if live {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (d *devServer) alive(w http.ResponseWriter, r *http.Request) {
	stream, err := app.SSE(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ch := make(chan string, 8)
	d.clientsMu.Lock()
	d.clients[ch] = true
	d.clientsMu.Unlock()
	defer func() {
		d.clientsMu.Lock()
		delete(d.clients, ch)
		d.clientsMu.Unlock()
	}()

	// The same message greets a new connection and reloads an existing one: the
	// client remembers the first revision it sees and reloads when a later one
	// is higher. That is what makes a missed rebuild self-correcting — a laptop
	// that slept, a tab whose connection dropped, or a dev server restarted
	// while the browser sat there all come back, see a newer number, and
	// reload. A bare "reload" event cannot do that, because the event a browser
	// was not connected for is simply gone.
	if stream.Send("alive", d.currentRevision()) != nil {
		return
	}

	// A browser that connects while the build is broken should see the error,
	// not a blank page it has to guess about.
	d.mu.Lock()
	buildErr := d.buildErr
	d.mu.Unlock()
	if buildErr != "" {
		stream.Send("build-error", buildErr) //nolint:errcheck
	}

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			payload := ""
			switch ev {
			case "alive":
				payload = d.currentRevision()
			case "build-error":
				d.mu.Lock()
				payload = d.buildErr
				d.mu.Unlock()
			}
			if stream.Send(ev, payload) != nil {
				return
			}
		case <-ping.C:
			if stream.Send("ping", "") != nil {
				return
			}
		}
	}
}

// bump advances the revision and greets every connected browser with it. The
// clock seeds it so a restarted dev server always issues a higher number than
// the one it replaced; the +1 covers two rebuilds inside the same millisecond,
// which would otherwise be dropped as "not newer".
func (d *devServer) bump() {
	d.mu.Lock()
	next := time.Now().UnixMilli()
	if next <= d.revision {
		next = d.revision + 1
	}
	d.revision = next
	d.mu.Unlock()
	d.broadcast("alive")
}

func (d *devServer) currentRevision() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return strconv.FormatInt(d.revision, 10)
}

func (d *devServer) broadcast(event string) {
	d.clientsMu.Lock()
	defer d.clientsMu.Unlock()
	for ch := range d.clients {
		select {
		case ch <- event:
		default: // a client that cannot keep up will reconnect and re-sync
		}
	}
}

//go:embed alive.js
var aliveJS []byte

// ---------------------------------------------------------------------------

// derivePagesImport works out the import path of the page tree, so the common
// case needs no -module flag.
//
// go.mod is not necessarily in -dir: an app inside a larger module (every
// example in this repository) has its own directory but the module's go.mod
// sits further up, and the import path is the module path plus the walk back
// down. Getting this wrong produces a generated table that imports packages
// that do not exist, which reads as a confusing compile error rather than as a
// missing flag.
func derivePagesImport(root, pagesDir string) (string, error) {
	dir := root
	for {
		src, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			module := ""
			for _, line := range strings.Split(string(src), "\n") {
				if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
					module = strings.TrimSpace(rest)
					break
				}
			}
			if module == "" {
				return "", fmt.Errorf("%s has no module line (pass -module)", filepath.Join(dir, "go.mod"))
			}
			sub, err := filepath.Rel(dir, filepath.Join(root, filepath.FromSlash(pagesDir)))
			if err != nil {
				return "", err
			}
			return path.Join(module, filepath.ToSlash(sub)), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod above " + root + " (pass -module)")
		}
		dir = parent
	}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

const waitingPage = `<!doctype html>
<title>howl dev</title>
<style>body{font:14px ui-monospace,monospace;background:#111;color:#eee;padding:2rem}
pre{white-space:pre-wrap;color:#f88}</style>
<h1>The application is not answering</h1>
<pre>%s</pre>
<script>setTimeout(() => location.reload(), 1000)</script>
`
