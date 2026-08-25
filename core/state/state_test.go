package state

import (
	"context"
	"encoding/json"
	"testing"
)

func TestHydrateInstallsTypedState(t *testing.T) {
	type appState struct {
		Version string `json:"version"`
	}
	ctx, err := Hydrate[appState](context.Background(), json.RawMessage(`{"version":"v0.2.1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := Get[appState](ctx).Version; got != "v0.2.1" {
		t.Fatalf("Version = %q", got)
	}
}

func TestHydrateRejectsWrongShape(t *testing.T) {
	type appState struct {
		Version string `json:"version"`
	}
	if _, err := Hydrate[appState](context.Background(), json.RawMessage(`{"version":7}`)); err == nil {
		t.Fatal("invalid bootstrap state was accepted")
	}
}
