package todos

import (
	"github.com/mirairoad/howl-go/core/api"
	appstore "github.com/mirairoad/howl-go/examples/toy_app/client/store"
	"github.com/mirairoad/howl-go/examples/toy_app/server/apis/store"
)

// Sync applies a batch the browser has ALREADY applied and re-rendered. This is
// bookkeeping, not the critical path — which is why it answers with the fresh
// snapshot rather than an acknowledgement: the client can reconcile if it wants
// to, and ignore it if it does not.
//
// The body type is []store.Op rather than a named type declared here. A type in
// an endpoint package cannot be imported by the generated client, because this
// package uses api.Define, which does not exist in the wasm build.
var Sync = api.Define(api.Spec[api.None, []appstore.Op, appstore.Snapshot]{
	Name: "Sync Todos",
	Handler: func(r *api.Request[api.None, []appstore.Op]) (appstore.Snapshot, error) {
		for _, op := range r.Body {
			store.Get().Apply(op)
		}
		return store.Get().Snapshot(), nil
	},
})
