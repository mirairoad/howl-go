package apis

import (
	"github.com/mirairoad/howl-go/core/api"
	appstore "github.com/mirairoad/howl-go/examples/toy_app/client/store"
	"github.com/mirairoad/howl-go/examples/toy_app/server/apis/store"
)

// Metrics is the dashboard's data. Once the wasm renderer is up this is the
// only thing that crosses the wire for /dashboard/metrics — the markup is
// rendered in the browser from the same templ components the server uses.
var Metrics = api.Define(api.Spec[api.None, api.None, appstore.Metrics]{
	Name: "Metrics",
	Handler: func(r *api.Request[api.None, api.None]) (appstore.Metrics, error) {
		return store.Metrics(), nil
	},
})
