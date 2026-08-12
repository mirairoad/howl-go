// Package console is the colourful development logger: a log/slog handler that
// prints aligned, tinted lines to a terminal and structured JSON to anything
// else.
//
//	console.Setup(console.Options{})   // installs as slog.Default
//
//	09:25:03.412 INFO  listening   url=http://localhost:9000 routes=8
//	09:25:04.118 INFO  http        GET /docs/routing 200 5007B 1.2ms id=c9b37c21
//	09:25:04.902 WARN  http        GET /nope 404 42B 210µs id=2f20b450
//	09:25:07.310 ERROR http        POST /v1/traces 500 31B 4.1ms id=a820891e
//
// howl (TypeScript) does this by patching `globalThis.console` with a
// timestamp, a PID and a per-method colour. Go has no console to patch and
// does not need one: `log/slog` is already the seam every library logs
// through, so a handler reaches your code, the framework's code and your
// dependencies' code alike — without anyone importing this package.
//
// The rule for when to colour is "is a human reading this right now":
//
//	stdout is a terminal   -> tinted, aligned, human columns
//	stdout is a pipe/file  -> JSON, one object per line, for whatever ingests it
//
// so the same binary is pleasant in a terminal and parseable under systemd,
// Docker or a log shipper, with nothing to configure. NO_COLOR is honoured;
// FORCE_COLOR / CLICOLOR_FORCE override the detection the other way.
package console

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Options configures the handler. The zero value is the right thing for an
// application: colour when a terminal is attached, JSON when it is not.
type Options struct {
	// Level is the minimum level to emit. Defaults to slog.LevelInfo.
	Level slog.Leveler
	// Color forces tinted output on or off. nil auto-detects.
	Color *bool
	// JSON forces structured output on or off. nil auto-detects: JSON exactly
	// when the writer is not a terminal.
	JSON *bool
	// TimeFormat defaults to "15:04:05.000" — the time of day, to the
	// millisecond. A date is noise in a dev terminal and is stamped by the
	// collector everywhere else.
	TimeFormat string
}

// Setup builds a handler over os.Stdout and installs it as slog.Default, so
// every package that logs through slog — including core/app and core/mw —
// lands in the same format. Returns the logger for direct use.
func Setup(o Options) *slog.Logger {
	l := slog.New(New(os.Stdout, o))
	slog.SetDefault(l)
	return l
}

// New builds a handler over w.
func New(w io.Writer, o Options) slog.Handler {
	if o.Level == nil {
		o.Level = slog.LevelInfo
	}
	if o.TimeFormat == "" {
		o.TimeFormat = "15:04:05.000"
	}
	tty := isTerminal(w)
	if o.JSON == nil {
		json := !tty
		o.JSON = &json
	}
	if *o.JSON {
		return slog.NewJSONHandler(w, &slog.HandlerOptions{Level: o.Level})
	}
	if o.Color == nil {
		color := colorByEnv(tty)
		o.Color = &color
	}
	return &handler{w: w, opts: o, mu: &sync.Mutex{}}
}

// colorByEnv follows the conventions rather than inventing any: NO_COLOR wins
// outright (a user who set it means it), FORCE_COLOR turns it on where the
// detection cannot tell — a CI log viewer that renders escapes but is not a
// terminal — and otherwise the terminal decides.
func colorByEnv(tty bool) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("FORCE_COLOR") != "" || os.Getenv("CLICOLOR_FORCE") != "" {
		return true
	}
	return tty
}

// isTerminal reports whether w is a character device. os.File.Stat is enough
// for this; a dependency on x/term would buy nothing but a dependency.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// ---------------------------------------------------------------------------
// Colours
// ---------------------------------------------------------------------------

const (
	reset  = "\033[0m"
	bold   = "\033[1m"
	dim    = "\033[2m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	blue   = "\033[34m"
	purple = "\033[35m"
	cyan   = "\033[36m"
)

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

type handler struct {
	w      io.Writer
	opts   Options
	mu     *sync.Mutex // shared by every clone: one writer, one lock
	attrs  []slog.Attr
	groups []string
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.opts.Level.Level()
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr(nil), h.attrs...), as...)
	return &c
}

func (h *handler) WithGroup(name string) slog.Handler {
	c := *h
	c.groups = append(append([]string(nil), h.groups...), name)
	return &c
}

// known attributes get rendered positionally, in this order, before everything
// else: a request line reads as a request line rather than as five key=value
// pairs. Any record that does not carry them is unaffected.
var positional = []string{"method", "path", "status", "bytes", "took"}

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder

	b.WriteString(h.tint(dim, r.Time.Format(h.opts.TimeFormat)))
	b.WriteByte(' ')
	b.WriteString(h.tint(levelColor(r.Level), pad(r.Level.String(), 5)))
	b.WriteByte(' ')
	b.WriteString(h.tint(bold, pad(r.Message, 11)))

	// Collect every attribute, then split it into the positional ones and the
	// rest, so the caller's ordering never changes the shape of the line.
	byKey := map[string]slog.Value{}
	var rest []slog.Attr
	add := func(a slog.Attr) {
		key := a.Key
		if len(h.groups) > 0 {
			key = strings.Join(h.groups, ".") + "." + key
		}
		if len(h.groups) == 0 && contains(positional, key) {
			byKey[key] = a.Value
			return
		}
		rest = append(rest, slog.Attr{Key: key, Value: a.Value})
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(func(a slog.Attr) bool { add(a); return true })

	for _, key := range positional {
		v, ok := byKey[key]
		if !ok {
			continue
		}
		b.WriteByte(' ')
		b.WriteString(h.positional(key, v))
	}
	for _, a := range rest {
		b.WriteByte(' ')
		b.WriteString(h.tint(dim, a.Key+"="))
		b.WriteString(value(a.Value))
	}
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *handler) positional(key string, v slog.Value) string {
	switch key {
	case "method":
		return h.tint(bold, v.String())
	case "path":
		return v.String()
	case "status":
		code := int(v.Int64())
		return h.tint(statusColor(code), strconv.Itoa(code))
	case "bytes":
		return h.tint(dim, size(v.Int64()))
	case "took":
		return h.tint(dim, value(v))
	}
	return value(v)
}

func (h *handler) tint(color, s string) string {
	if !*h.opts.Color || color == "" {
		return s
	}
	return color + s + reset
}

func levelColor(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return red
	case l >= slog.LevelWarn:
		return yellow
	case l >= slog.LevelInfo:
		return cyan
	default:
		return purple
	}
}

// statusColor is the reason the request line is worth reading at a glance: a
// wall of green with one red row is a signal; a wall of identical grey is not.
func statusColor(code int) string {
	switch {
	case code >= 500:
		return red
	case code >= 400:
		return yellow
	case code >= 300:
		return blue
	case code >= 200:
		return green
	}
	return ""
}

func value(v slog.Value) string {
	if v.Kind() == slog.KindDuration {
		return v.Duration().String()
	}
	s := v.String()
	if strings.ContainsAny(s, " \"") {
		return strconv.Quote(s)
	}
	return s
}

func size(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fkB", float64(n)/(1<<10))
	}
	return strconv.FormatInt(n, 10) + "B"
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Duration is a convenience for logging a duration rounded to something a
// human reads: 1.2ms, not 1.203847ms.
func Duration(d time.Duration) time.Duration {
	switch {
	case d > time.Second:
		return d.Round(time.Millisecond)
	case d > time.Millisecond:
		return d.Round(10 * time.Microsecond)
	}
	return d.Round(time.Microsecond)
}
