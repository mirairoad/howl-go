// Package store holds the process-wide todo store the endpoints read and write.
//
// A leaf, on purpose: the generated table lives in the root apis package and
// imports every endpoint package, so an endpoint package importing the root
// would be a cycle. One package below both is where a shared dependency lives.
package store

import appstore "github.com/mirairoad/howl-go/examples/toy_app/client/store"

var current *appstore.Store

// Use wires the store in. main.go calls it once, before api.Register.
func Use(s *appstore.Store) { current = s }

// Get panics when unset rather than returning nil: a nil store surfaces as a
// confusing crash inside a handler on the first request, and this way the
// mistake is obvious at startup.
func Get() *appstore.Store {
	if current == nil {
		panic("apis: store.Use was never called")
	}
	return current
}

// Metrics is the fake dataset the dashboard renders. A real application would
// read a database here; the point of the toy app is that the browser gets this
// as JSON and renders the same templ components the server would.
func Metrics() appstore.Metrics {
	return appstore.Metrics{
		Cards: []appstore.Card{
			{Label: "Active sessions", Value: "12,847", Delta: 4.2},
			{Label: "p95 latency", Value: "184 ms", Delta: -11.5},
			{Label: "Error rate", Value: "0.31%", Delta: -2.0},
			{Label: "Throughput", Value: "9.2k/s", Delta: 8.7},
		},
		Rows: []appstore.Row{
			{Name: "sydney", Value: 48210}, {Name: "singapore", Value: 39104},
			{Name: "frankfurt", Value: 28755}, {Name: "us-east-1", Value: 91233},
			{Name: "us-west-2", Value: 40188}, {Name: "sao-paulo", Value: 12044},
			{Name: "tokyo", Value: 33590}, {Name: "mumbai", Value: 21877},
		},
	}
}
