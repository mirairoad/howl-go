package observe

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The rule the whole package exists for: an email address does not reach a
// span, in any mode, at any depth, under any field name, in any position.
func TestAnEmailIsNeverRecorded(t *testing.T) {
	const addr = "ada@example.com"
	cases := []any{
		map[string]any{"email": addr},
		map[string]any{"login": addr},                           // an innocent field name
		map[string]any{"$or": []any{map[string]any{"x": addr}}}, // nested in a branch
		map[string]any{"contacts": []any{addr, addr, addr}},
		map[string]any{addr: "x"},                      // in key position
		map[string]any{"a": map[string]any{"b": addr}}, // deeper
		json.RawMessage(`{"user":{"email":"` + addr + `"}}`),
		struct {
			Email string `json:"email"`
			Name  string `json:"name"`
		}{addr, "Ada"},
	}
	for _, mode := range []Mode{Safe, Full} {
		for i, v := range cases {
			got := Render(v, mode)
			if strings.Contains(got, addr) || strings.Contains(got, "example.com") {
				t.Errorf("case %d mode %d leaked an address: %s", i, mode, got)
			}
		}
	}
}

// Safe keeps what tells you which row, and drops what tells you who.
func TestSafeKeepsIdentifiersAndDropsProse(t *testing.T) {
	got := Render(map[string]any{
		"_id":     "0193a5c2-5f4e-7b3a-9c1d-2e6f8a0b4c7d", // UUIDv7, howl's own
		"legacy":  "01ARZ3NDEKTSV4RRFFQ69G5FAV",           // ULID
		"digest":  "d41d8cd98f00b204e9800998ecf8427e",     // hex
		"seq":     "40188",
		"name":    "Ada Lovelace",
		"note":    "call her about the engine",
		"active":  true,
		"version": 3,
		"deleted": nil,
	}, Safe)
	for _, want := range []string{
		`"_id":"0193a5c2-5f4e-7b3a-9c1d-2e6f8a0b4c7d"`,
		`"legacy":"01ARZ3NDEKTSV4RRFFQ69G5FAV"`,
		`"digest":"d41d8cd98f00b204e9800998ecf8427e"`,
		`"seq":"40188"`, `"active":true`, `"version":3`, `"deleted":null`,
		`"name":"?"`, `"note":"?"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "Ada") || strings.Contains(got, "engine") {
		t.Errorf("prose survived Safe: %s", got)
	}
}

// Full is for a development machine, and it still refuses the field names that
// can never be recorded — whatever is under them, however deep.
func TestFullStillRefusesSensitiveFields(t *testing.T) {
	got := Render(map[string]any{
		"title":       "quarterly report",
		"password":    "hunter2",
		"api_key":     "sk-live-123",
		"credentials": map[string]any{"user": "ada", "totp": "999111"},
		"phone":       "+61 400 000 000",
		"keyword":     "monkey", // neither "key" nor "monkey" is a sensitive word
	}, Full)
	if !strings.Contains(got, `"title":"quarterly report"`) || !strings.Contains(got, `"keyword":"monkey"`) {
		t.Errorf("Full dropped ordinary values: %s", got)
	}
	for _, leak := range []string{"hunter2", "sk-live-123", "999111", "400 000"} {
		if strings.Contains(got, leak) {
			t.Errorf("Full leaked %q: %s", leak, got)
		}
	}
	if !strings.Contains(got, `"credentials":{"totp":"?","user":"?"}`) {
		t.Errorf("a sensitive field's whole subtree should go: %s", got)
	}
}

// Off is off.
func TestOffRecordsNothing(t *testing.T) {
	if got := Render(map[string]any{"a": 1}, Off); got != "" {
		t.Errorf("Off returned %q", got)
	}
}

// Shape survives whatever the values do: the field names, the operators and
// the size of an `in` clause are what make a trace answer "which query".
func TestShapeIsKeptAndBounded(t *testing.T) {
	ids := make([]any, 500)
	for i := range ids {
		ids[i] = "0193a5c2-5f4e-7b3a-9c1d-2e6f8a0b4c7d"
	}
	got := Render(map[string]any{"org_id": map[string]any{"$in": ids}}, Safe)
	if !strings.Contains(got, `"org_id":{"$in":[`) {
		t.Errorf("the shape is gone: %s", got)
	}
	if !strings.Contains(got, `"+492 more"`) {
		t.Errorf("the count of what was elided is the point: %s", got)
	}
	if len(got) > maxRendered+8 {
		t.Errorf("rendered %d bytes", len(got))
	}
}

func TestRenderIsStableAndBounded(t *testing.T) {
	v := map[string]any{"b": 1, "a": 2, "c": 3}
	if Render(v, Safe) != Render(v, Safe) || Render(v, Safe) != `{"a":2,"b":1,"c":3}` {
		t.Errorf("key order is not stable: %s", Render(v, Safe))
	}
	long := map[string]any{"note": strings.Repeat("x", 4000)}
	if got := Render(long, Full); len(got) > maxRendered+8 || !strings.HasSuffix(got, "…") {
		t.Errorf("a long value was not cut: %d bytes", len(got))
	}
	// Depth is bounded too: a filter can nest as far as the caller likes.
	deep := any("leaf")
	for range 20 {
		deep = map[string]any{"$and": []any{deep}}
	}
	if got := Render(deep, Full); !strings.Contains(got, `"…"`) {
		t.Errorf("depth was not bounded: %s", got)
	}
}

func TestSensitiveMatchesWordsNotSubstrings(t *testing.T) {
	yes := []string{
		"email", "Email", "user_email", "api_key", "key", "billing.card", "dob", "SSN",
		// camelCase is how a JSON document usually spells it, and splitting
		// only on punctuation made every one of these ordinary.
		"userEmail", "UserEmail", "dateOfBirth", "phoneNumber", "apiKey", "APIKey",
		"customer.homeAddress", "sessionToken",
	}
	for _, f := range yes {
		if !Sensitive(f) {
			t.Errorf("%q should be sensitive", f)
		}
	}
	for _, no := range []string{"keyword", "monkey", "emailed_at_count", "title", "org_id", "passenger", "keyboardLayout"} {
		if Sensitive(no) {
			t.Errorf("%q should not be sensitive", no)
		}
	}
}

// The gap camelCase left was not only a Full-mode one: a phone number written
// as digits is a string Safe keeps, because digits are how an id looks. The
// field name is the only thing that can stop it.
func TestDigitsUnderACamelCaseContactFieldAreNotKept(t *testing.T) {
	for _, mode := range []Mode{Safe, Full} {
		got := Render(map[string]any{
			"phoneNumber": "61400000000",
			"orderId":     "40188",
		}, mode)
		if strings.Contains(got, "61400000000") {
			t.Errorf("mode %d kept a phone number: %s", mode, got)
		}
		if !strings.Contains(got, `"orderId":"40188"`) {
			t.Errorf("mode %d dropped an ordinary id: %s", mode, got)
		}
	}
}

// Rendering is not free, so the framework asks first.
func TestEnabledFollowsTheTracer(t *testing.T) {
	SetDefault(nil)
	if Enabled(context.Background()) {
		t.Error("the no-op tracer reports enabled")
	}
	if !Enabled(With(context.Background(), &Recorder{})) {
		t.Error("a context tracer reports disabled")
	}
	SetDefault(&Recorder{})
	defer SetDefault(nil)
	if !Enabled(context.Background()) {
		t.Error("an installed tracer reports disabled")
	}
}

// A timestamp is a moment, and a moment names no one. Keeping it is what makes
// a range query legible: the difference between knowing that a bulk delete
// scanned by date and knowing what it deleted.
func TestSafeKeepsTimestampsButNotBareDates(t *testing.T) {
	got := Render(map[string]any{
		"created":   "2026-01-02T03:04:05Z",
		"expires":   "2026-01-02T03:04:05.123456789Z",
		"seen":      "2026-01-02T03:04:05+10:00",
		"lowercase": "2026-01-02t03:04:05z",
		"day":       "1985-03-12", // a date is how a birthday is written
		"partial":   "2026-01-02T03:04",
		"nearly":    "2026-01-02T03:04:05.Z",
		"prose":     "tomorrow at three",
	}, Safe)
	for _, want := range []string{
		`"created":"2026-01-02T03:04:05Z"`,
		`"expires":"2026-01-02T03:04:05.123456789Z"`,
		`"seen":"2026-01-02T03:04:05+10:00"`,
		`"lowercase":"2026-01-02t03:04:05z"`,
		`"day":"?"`, `"partial":"?"`, `"nearly":"?"`, `"prose":"?"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}

// The field name still wins. A date of birth stored as a time.Time marshals to
// exactly the shape above, so the only thing standing between it and the span
// is its name — in every spelling.
func TestADateOfBirthIsStillRedacted(t *testing.T) {
	const born = "1985-03-12T00:00:00Z"
	for _, field := range []string{"dob", "DOB", "date_of_birth", "dateOfBirth", "birthDate", "bornOn", "customer.birthday"} {
		for _, mode := range []Mode{Safe, Full} {
			if got := Render(map[string]any{field: born}, mode); strings.Contains(got, "1985") {
				t.Errorf("%s in mode %d: %s", field, mode, got)
			}
		}
	}
}
