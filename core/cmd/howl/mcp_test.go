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

func TestConventionsUnknownSection(t *testing.T) {
	if got := conventions("no such thing"); !strings.HasPrefix(got, "no section matching") {
		t.Errorf("got %q", got)
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
