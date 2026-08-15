package console

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func on(b bool) *bool { return &b }

func line(t *testing.T, o Options, log func(*slog.Logger)) string {
	t.Helper()
	var buf bytes.Buffer
	if o.Color == nil {
		o.Color = on(false)
	}
	if o.JSON == nil {
		o.JSON = on(false)
	}
	log(slog.New(New(&buf, o)))
	return strings.TrimRight(buf.String(), "\n")
}

func TestRequestLineReadsAsARequest(t *testing.T) {
	got := line(t, Options{}, func(l *slog.Logger) {
		l.Info("http",
			slog.String("method", "GET"),
			slog.String("path", "/docs/routing"),
			slog.Int("status", 200),
			slog.Int("bytes", 5007),
			slog.Duration("took", 1200*time.Microsecond),
			slog.String("id", "c9b37c21"),
		)
	})

	// The five request attributes are positional and ordered, whatever order
	// the caller passed them in; everything else stays key=value.
	if !strings.Contains(got, "GET /docs/routing 200 4.9kB 1.2ms") {
		t.Fatalf("request line = %q", got)
	}
	if !strings.Contains(got, "id=c9b37c21") {
		t.Fatalf("id missing: %q", got)
	}
	if !strings.Contains(got, "INFO") {
		t.Fatalf("level missing: %q", got)
	}
}

func TestAttributeOrderDoesNotChangeTheLine(t *testing.T) {
	a := line(t, Options{}, func(l *slog.Logger) {
		l.Info("http", slog.String("method", "GET"), slog.Int("status", 200), slog.String("path", "/x"))
	})
	b := line(t, Options{}, func(l *slog.Logger) {
		l.Info("http", slog.Int("status", 200), slog.String("path", "/x"), slog.String("method", "GET"))
	})
	if a != b {
		t.Fatalf("same record rendered two ways:\n%s\n%s", a, b)
	}
}

func TestColorFollowsStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		color  string
		name   string
	}{
		{200, green, "ok"},
		{404, yellow, "client error"},
		{500, red, "server error"},
	} {
		got := line(t, Options{Color: on(true)}, func(l *slog.Logger) {
			l.Info("http", slog.String("method", "GET"), slog.String("path", "/x"), slog.Int("status", tc.status))
		})
		if !strings.Contains(got, tc.color) {
			t.Errorf("%s (%d): expected colour %q in %q", tc.name, tc.status, tc.color, got)
		}
	}
}

func TestColorOffProducesNoEscapes(t *testing.T) {
	got := line(t, Options{Color: on(false)}, func(l *slog.Logger) {
		l.Error("http", slog.Int("status", 500))
	})
	if strings.Contains(got, "\033") {
		t.Fatalf("escape sequences leaked into uncoloured output: %q", got)
	}
}

// Not a terminal means something is going to parse this.
func TestNonTerminalWriterGetsJSON(t *testing.T) {
	var buf bytes.Buffer
	slog.New(New(&buf, Options{})).Info("listening", slog.String("url", "http://localhost:9000"))

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %q", buf.String())
	}
	if got["url"] != "http://localhost:9000" {
		t.Fatalf("attrs lost: %v", got)
	}
}

func TestLevelFilters(t *testing.T) {
	got := line(t, Options{Level: slog.LevelWarn}, func(l *slog.Logger) {
		l.Info("quiet")
		l.Warn("loud")
	})
	if strings.Contains(got, "quiet") || !strings.Contains(got, "loud") {
		t.Fatalf("level filter wrong: %q", got)
	}
}

func TestWithAttrsAndGroups(t *testing.T) {
	got := line(t, Options{}, func(l *slog.Logger) {
		l.With(slog.String("app", "guard")).WithGroup("db").Info("query", slog.String("table", "events"))
	})
	if !strings.Contains(got, "app=guard") || !strings.Contains(got, "db.table=events") {
		t.Fatalf("line = %q", got)
	}
}

func TestSizesAreHumanReadable(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{{42, "42B"}, {5007, "4.9kB"}, {5 << 20, "5.0MB"}} {
		if got := size(tc.n); got != tc.want {
			t.Errorf("size(%d) = %s, want %s", tc.n, got, tc.want)
		}
	}
}

func TestNoColorEnvWins(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("FORCE_COLOR", "1")
	if colorByEnv(true) {
		t.Fatal("NO_COLOR must win — a user who set it meant it")
	}
}
