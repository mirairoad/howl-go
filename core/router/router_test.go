package router

import (
	"context"
	"testing"
)

// The root link must not be current everywhere. Treating "/" as a prefix lights
// up two sidebar entries at once, and it is the one case where the server and
// the client runtime disagreed — the client excluded "/", this did not.
func TestUnderTreatsRootAsExact(t *testing.T) {
	for _, tc := range []struct {
		current, prefix string
		want            bool
	}{
		{"/", "/", true},
		{"/logs", "/", false},
		{"/logs/42", "/logs", true},
		{"/logs", "/logs", true},
		{"/logsomething", "/logs", false},
		{"/metrics", "/logs", false},
	} {
		ctx := WithCurrent(context.Background(), tc.current)
		if got := Under(ctx, tc.prefix); got != tc.want {
			t.Errorf("Under(%q, %q) = %v, want %v", tc.current, tc.prefix, got, tc.want)
		}
	}
}

// Per-route client data is derived from the table, so a `//howl:data` line is
// the only place it is written down.
func TestPageDataIsDerivedFromTheTable(t *testing.T) {
	rs := []Route{
		{Pattern: "/dashboard", Client: true},
		{Pattern: "/dashboard/metrics", Client: true, Data: "/api/metrics"},
		{Pattern: "/blog/{article_id}", Client: true, Data: "/api/article"},
	}
	got := PageData(rs)
	if len(got) != 2 {
		t.Fatalf("PageData = %v, want the two routes that declared one", got)
	}
	if got["/dashboard/metrics"] != "/api/metrics" || got["/blog/{article_id}"] != "/api/article" {
		t.Errorf("PageData = %v", got)
	}
	if _, ok := got["/dashboard"]; ok {
		t.Error("a route with no //howl:data was given an endpoint")
	}
}

// Nil rather than an empty map: it is omitempty in the client config, and a
// site where no route declares one should publish no key at all.
func TestPageDataIsAbsentWhenUnused(t *testing.T) {
	if got := PageData([]Route{{Pattern: "/", Client: true}}); got != nil {
		t.Errorf("PageData = %v, want nil", got)
	}
}
