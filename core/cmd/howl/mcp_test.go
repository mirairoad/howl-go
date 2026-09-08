package main

import (
	"strings"
	"testing"
)

// A section with subheadings must come back whole. Breaking at the first
// heading of any depth returned "Making a page interactive" as its intro and
// none of the rules — 870 bytes of 7800 — which is worse than returning
// nothing, because the answer looks complete.
func TestConventionsKeepsSubsections(t *testing.T) {
	got := conventions("Making a page interactive")
	for _, want := range []string{
		"### The store: two files, two different rules",
		"### The three wires, in the order they run",
		"### The lifecycle contract",
		"### What does not exist",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("section is missing %q", want)
		}
	}
	// ...and must still stop at the next section of the same level.
	if strings.Contains(got, "\n## Client render safety") {
		t.Error("section ran past its sibling heading")
	}
}

func TestConventionsSubsectionOnItsOwn(t *testing.T) {
	got := conventions("The lifecycle contract")
	if !strings.HasPrefix(strings.TrimSpace(got), "### The lifecycle contract") {
		t.Fatalf("wrong section:\n%.120s", got)
	}
	if strings.Contains(got, "### Overlays") {
		t.Error("a subsection must stop at its next sibling")
	}
}

func TestConventionsNativeWindow(t *testing.T) {
	got := conventions("Native window")
	for _, want := range []string{"desktop.Run", "Attach", "a.Serve", "go:embed"} {
		if !strings.Contains(got, want) {
			t.Errorf("Native window section is missing %q", want)
		}
	}
}

func TestConventionsUnknownSection(t *testing.T) {
	if got := conventions("no such thing"); !strings.HasPrefix(got, "no section matching") {
		t.Errorf("got %q", got)
	}
}

// Every frontend topic has the same five parts, and that shape is the point:
// an agent that reads "Rules" without "Wrong" writes the wrong thing with
// confidence. The test is what keeps a topic from being added without it.
func TestFrontendTopicsAreStructured(t *testing.T) {
	topics := []string{"checklist", "page", "store", "events", "list", "form", "modal", "filter", "navigation", "animation", "islands"}
	for _, topic := range topics {
		got := frontend(topic)
		if !strings.HasPrefix(strings.TrimSpace(got), "## "+topic) {
			t.Errorf("%s: wrong section:\n%.80s", topic, got)
			continue
		}
		for _, part := range []string{"**When**", "**Rules**", "**Wrong**", "**Check**"} {
			if !strings.Contains(got, part) {
				t.Errorf("%s is missing %s", topic, part)
			}
		}
		if topic != "checklist" && topic != "navigation" && topic != "islands" && !strings.Contains(got, "**Example**") {
			t.Errorf("%s is missing an example", topic)
		}
		if strings.Count(got, "\n## ") > 0 {
			t.Errorf("%s ran into the next topic", topic)
		}
	}
	// The two that models get wrong most carry the rule that fixes it.
	if got := frontend("modal"); !strings.Contains(got, "one signal") || !strings.Contains(got, "showModal") {
		t.Error("the modal recipe must state the one-signal rule and name showModal as the wrong turn")
	}
	if got := frontend("store"); !strings.Contains(got, "dom.Embedded") || !strings.Contains(got, "templ.JSONScript") {
		t.Error("the store recipe must show the embedded snapshot on both sides")
	}
}

// The JSON view cuts at the bold markers, so every field of every topic is
// filled — a client rendering "Wrong" beside "Example" never gets an empty pane.
func TestFrontendJSONHasEveryPart(t *testing.T) {
	out, err := frontendJSON("modal")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"when": "`, `"rules": "`, `"example": "`, `"wrong": "`, `"check": "`} {
		if !strings.Contains(out, key) || strings.Contains(out, key+`"`) {
			t.Errorf("modal JSON is missing or empty at %s", key)
		}
	}
	if !strings.Contains(out, "editing") {
		t.Error("the modal example should reach the JSON view")
	}
	if _, err := frontendJSON("hooks"); err == nil {
		t.Error("an unknown topic must be an error in JSON mode, not an empty object")
	}
	all, err := frontendJSON("")
	if err != nil || strings.Count(all, `"topic": "`) != len(frontendTopics) {
		t.Errorf("all topics: err=%v count=%d", err, strings.Count(all, `"topic": "`))
	}
}

func TestFrontendUnknownTopic(t *testing.T) {
	if got := frontend("hooks"); !strings.HasPrefix(got, "no section matching") || !strings.Contains(got, "modal") {
		t.Errorf("an unknown topic should list the real ones, got %q", got)
	}
}

// A '#' at the start of a line inside a code fence is a shell comment, not an
// h1, and must not end the section it sits in.
func TestHeadingLevelIgnoresFences(t *testing.T) {
	if got := headingLevel("# rebuild everything", true); got != 0 {
		t.Errorf("fenced line read as a level-%d heading", got)
	}
	if got := headingLevel("### Layouts", false); got != 3 {
		t.Errorf("got level %d, want 3", got)
	}
	if got := headingLevel("#not-a-heading", false); got != 0 {
		t.Errorf("got level %d, want 0", got)
	}
}
