package todos

import (
	"github.com/mirairoad/howl-go/core/api"
	appstore "github.com/mirairoad/howl-go/examples/toy_app/client/store"
	"github.com/mirairoad/howl-go/examples/toy_app/server/apis/store"
)

// List is what the browser hydrates from: the whole snapshot, because the
// client store applies ops locally and only needs a starting point.
var List = api.Define(api.Spec[api.None, api.None, appstore.Snapshot]{
	Name: "Todos",
	Handler: func(r *api.Request[api.None, api.None]) (appstore.Snapshot, error) {
		return store.Get().Snapshot(), nil
	},
})
