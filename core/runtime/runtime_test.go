package runtime

import (
	"io/fs"
	"strings"
	"testing"
)

func TestAppJSNoSPAUsesAttributePresence(t *testing.T) {
	source, err := fs.ReadFile(Assets(), "app.js")
	if err != nil {
		t.Fatal(err)
	}

	js := string(source)
	if !strings.Contains(js, `a.hasAttribute("data-no-spa")`) {
		t.Fatal("SPA opt-out must detect an empty boolean data-no-spa attribute")
	}
	if strings.Contains(js, "a.dataset.noSpa") {
		t.Fatal("truthiness check ignores data-no-spa with an empty value")
	}
}
