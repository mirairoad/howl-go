module github.com/mirairoad/howl-go/examples/toy_app/desktop

go 1.25.0

require (
	github.com/mirairoad/howl-go v0.0.0
	github.com/mirairoad/howl-go/desktop v0.0.0
)

require (
	github.com/a-h/templ v0.3.1020 // indirect
	github.com/webview/webview_go v0.0.0-20240831120633-6173450d4dd6 // indirect
)

replace github.com/mirairoad/howl-go => ../../..

replace github.com/mirairoad/howl-go/desktop => ../../../desktop
