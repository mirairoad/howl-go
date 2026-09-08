package todos

import (
	"github.com/mirairoad/howl-go/core/api"
	appstore "github.com/mirairoad/howl-go/examples/toy_app/client/store"
	"github.com/mirairoad/howl-go/examples/toy_app/server/apis/store"
)

// Limit is a rule only the server can enforce: how many todos its memory
// holds. The browser applied the op before asking, so a refusal here is what
// store.Commit rolls back — the row the user saw for a moment disappears and
// the page says why. Small on purpose, so the rollback is easy to reach.
const Limit = 8

// Sync applies a batch the browser has ALREADY applied and re-rendered. This is
// bookkeeping, not the critical path — which is why it answers with the fresh
// snapshot rather than an acknowledgement: the client can reconcile if it wants
// to, and ignore it if it does not.
//
// A refused op is a 400 with the reason, and nothing after it in the batch is
// applied: the client rolls back exactly the op it sent, so the batch must
// fail as a unit or the two sides disagree about what happened.
//
// The body type is []store.Op rather than a named type declared here. A type in
// an endpoint package cannot be imported by the generated client, because this
// package uses api.Define, which does not exist in the wasm build.
var Sync = api.Define(api.Spec[api.None, []appstore.Op, appstore.Snapshot]{
	Name: "Sync Todos",
	Handler: func(r *api.Request[api.None, []appstore.Op]) (appstore.Snapshot, error) {
		s := store.Get()
		for _, op := range r.Body {
			if op.Kind == "add" && s.Count() >= Limit {
				return appstore.Snapshot{}, api.BadRequest("server memory holds 8 todos — delete one first")
			}
			s.Apply(op)
		}
		return s.Snapshot(), nil
	},
})
