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
