package main

import "testing"

func TestWatchedSkipsGeneratedBuildArtifacts(t *testing.T) {
	for _, name := range []string{"fsroutes_gen.go", "api_gen.go", "app_templ.go", "views.wasm", "wasm_exec.js"} {
		if watched(name) {
			t.Errorf("watched(%q) = true; generated output would trigger a rebuild loop", name)
		}
	}
	for _, name := range []string{"main.go", "index.client.templ", "guard.js"} {
		if !watched(name) {
			t.Errorf("watched(%q) = false; source changes would be missed", name)
		}
	}
}
