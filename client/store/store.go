// Package store is the domain model, and it is deliberately free of anything
// server-shaped: no net/http, no database, no filesystem. That is the whole
// point — it compiles for the server AND for GOOS=js GOARCH=wasm, so the
// browser runs the same mutation logic the server runs, byte for byte.
//
// Normally "local-first" costs you a second implementation of every rule, in
// TypeScript, kept in sync by hand. Here there is one implementation.
package store

import (
	"context"
	"sync"
)

// ---------------------------------------------------------------------------
// Domain types. They live here, not in a view package, so page packages can
// import them while the generated route table imports the page packages.
// ---------------------------------------------------------------------------

type Todo struct {
	ID   int    `json:"id"`
	Text string `json:"text"`
}

type Card struct {
	Label string  `json:"label"`
	Value string  `json:"value"`
	Delta float64 `json:"delta"`
}

type Row struct {
	Name  string `json:"name"`
	Value int    `json:"value"`
}

type Metrics struct {
	Cards []Card `json:"cards"`
	Rows  []Row  `json:"rows"`
}

// Meta is per-request scalars the pages display.
type Meta struct {
	RenderedAt string
	GoVersion  string
	Region     string
}

// Pages take no arguments — the route table needs one uniform signature — so
// their data arrives through the context templ already threads into Render.
type ctxKey int

const (
	metricsKey ctxKey = iota
	todosKey
	metaKey
)

func WithMetrics(ctx context.Context, m Metrics) context.Context {
	return context.WithValue(ctx, metricsKey, m)
}

func MetricsFrom(ctx context.Context) Metrics {
	m, _ := ctx.Value(metricsKey).(Metrics)
	return m
}

func WithTodos(ctx context.Context, t []Todo) context.Context {
	return context.WithValue(ctx, todosKey, t)
}

func TodosFrom(ctx context.Context) []Todo {
	t, _ := ctx.Value(todosKey).([]Todo)
	return t
}

func WithMeta(ctx context.Context, m Meta) context.Context {
	return context.WithValue(ctx, metaKey, m)
}

func MetaFrom(ctx context.Context) Meta {
	m, _ := ctx.Value(metaKey).(Meta)
	return m
}

type Store struct {
	mu     sync.Mutex
	nextID int
	items  []Todo
}

// Snapshot is the wire format: whole-state serialisation, used to hydrate the
// browser from the server and to reconcile back the other way.
type Snapshot struct {
	NextID int    `json:"nextId"`
	Items  []Todo `json:"items"`
}

// Op is a single mutation. The client applies it locally for an instant render
// and ships the same value to the server, which applies it identically.
type Op struct {
	Kind string `json:"kind"` // "add" | "del"
	Text string `json:"text,omitempty"`
	ID   int    `json:"id,omitempty"`
}

func New() *Store { return &Store{} }

func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Todo, len(s.items))
	copy(out, s.items)
	return Snapshot{NextID: s.nextID, Items: out}
}

func (s *Store) Restore(sn Snapshot) {
	s.mu.Lock()
	s.nextID = sn.NextID
	s.items = append(s.items[:0], sn.Items...)
	s.mu.Unlock()
	Notify()
}

func (s *Store) List() []Todo { return s.Snapshot().Items }

func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

func (s *Store) Add(text string) Todo {
	s.mu.Lock()
	s.nextID++
	t := Todo{ID: s.nextID, Text: text}
	s.items = append(s.items, t)
	s.mu.Unlock()
	Notify()
	return t
}

func (s *Store) Del(id int) {
	s.mu.Lock()
	for i, t := range s.items {
		if t.ID == id {
			s.items = append(s.items[:i], s.items[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	Notify()
}

// Apply runs one op. Server and client call this same method.
func (s *Store) Apply(op Op) {
	switch op.Kind {
	case "add":
		s.Add(op.Text)
	case "del":
		s.Del(op.ID)
	}
}

// Article backs the dynamic /blog/{article_id} route.
type Article struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Blurb string `json:"blurb"`
	Body  string `json:"body"`
}

func Articles() []Article {
	return []Article{
		{ID: "fs-routing", Title: "Routes from the filesystem",
			Blurb: "A directory tree becomes a Go route table at build time.",
			Body:  "tools/fsroutes walks client/pages, reads each .templ file for its component and //howl: directives, and writes fsroutes_gen.go. Nothing is registered by hand."},
		{ID: "no-underscores", Title: "Why not _layout.templ",
			Blurb: "Go ignores files starting with an underscore.",
			Body:  "The Go toolchain skips any file or directory whose name begins with _ or . — so _layout.templ would generate _layout_templ.go and the compiler would never see it. Reserved basenames are the portable form of the same idea."},
		{ID: "one-source", Title: "One component, three runtimes",
			Blurb: "The same templ renders on the server, to disk, and in wasm.",
			Body:  "A templ.Component is just Render(ctx, io.Writer). It cannot tell whether the writer is an HTTP response, a file, or a strings.Builder inside WebAssembly."},
	}
}

func ArticleByID(id string) (Article, bool) {
	for _, a := range Articles() {
		if a.ID == id {
			return a, true
		}
	}
	return Article{}, false
}
