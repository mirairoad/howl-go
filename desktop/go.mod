module github.com/mirairoad/howl-go/desktop

go 1.25.0

require (
	github.com/mirairoad/howl-go v0.3.0
	github.com/webview/webview_go v0.0.0-20240831120633-6173450d4dd6
)

require github.com/a-h/templ v0.3.1020 // indirect

// The requirement above must name a real published version even though this
// replace makes it unused locally. A replace only applies in the main module,
// so an application depending on this one reads the require line and nothing
// else — and `v0.0.0` is not a revision, so the whole build fails with
// "unknown revision v0.0.0" before it compiles a line.
replace github.com/mirairoad/howl-go => ..
