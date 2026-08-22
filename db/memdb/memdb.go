// Package memdb is an in-process [db.Backend]: documents in a map, the filter
// grammar evaluated in Go.
//
// It exists for two reasons. It runs the conformance suite on every `go test`
// with nothing installed, so the contract is checked even where Postgres is
// not; and it is the test double an application uses to exercise its own
// service methods without a database. It is not a storage engine — there is
// no persistence, no index, and every query is a scan.
package memdb

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/mirairoad/howl-go/db"
)

// Backend holds one collection.
type Backend struct {
	mu   sync.RWMutex
	docs map[string]map[string]any
}

// New returns an empty backend.
func New() *Backend { return &Backend{docs: map[string]map[string]any{}} }

// Prefix is the cache namespace. It differs from the SQL backends' so a test
// that swaps the backend cannot silently read the other's keys.
func (b *Backend) Prefix() string { return "mem" }

// NewID returns a UUIDv7, the same ids the real backends generate.
func (b *Backend) NewID() string { return db.NewID() }

func (b *Backend) Insert(_ context.Context, id string, doc json.RawMessage, _ db.OpOptions) error {
	var value map[string]any
	if err := json.Unmarshal(doc, &value); err != nil {
		return err
	}
	value["id"] = id
	b.mu.Lock()
	defer b.mu.Unlock()
	b.docs[id] = value
	return nil
}

func (b *Backend) FindOne(_ context.Context, where db.M, _ db.OpOptions) (json.RawMessage, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, id := range b.sortedIDs() {
		if matches(b.docs[id], where) {
			return json.Marshal(b.docs[id])
		}
	}
	return nil, db.ErrNotFound
}

func (b *Backend) FindMany(_ context.Context, where db.M, o db.FindOptions) ([]json.RawMessage, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var hits []map[string]any
	for _, id := range b.sortedIDs() {
		if matches(b.docs[id], where) {
			hits = append(hits, b.docs[id])
		}
	}
	if len(o.Sort) > 0 {
		sort.SliceStable(hits, func(i, j int) bool { return less(hits[i], hits[j], o.Sort) })
	}
	if o.Skip > 0 {
		if o.Skip >= len(hits) {
			return nil, nil
		}
		hits = hits[o.Skip:]
	}
	if o.Limit > 0 && o.Limit < len(hits) {
		hits = hits[:o.Limit]
	}

	out := make([]json.RawMessage, 0, len(hits))
	for _, doc := range hits {
		raw, err := json.Marshal(doc)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

func (b *Backend) Count(_ context.Context, where db.M, _ db.OpOptions) (int64, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var n int64
	for _, doc := range b.docs {
		if matches(doc, where) {
			n++
		}
	}
	return n, nil
}

func (b *Backend) UpdatePaths(_ context.Context, id string, paths map[string]any, o db.UpdateOptions) (json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	doc, ok := b.docs[id]
	if !ok {
		return nil, db.ErrNotFound
	}
	if o.ExpectedVersion != 0 {
		if current, ok := number(doc["version"]); !ok || int64(current) != o.ExpectedVersion {
			return nil, db.ErrNotFound
		}
	}
	apply(doc, paths, o)
	return json.Marshal(doc)
}

func (b *Backend) UpdatePathsWhere(_ context.Context, where db.M, paths map[string]any, o db.UpdateOptions) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var n int64
	for _, id := range b.sortedIDs() {
		if !matches(b.docs[id], where) {
			continue
		}
		apply(b.docs[id], paths, o)
		n++
	}
	return n, nil
}

func (b *Backend) DeleteOne(_ context.Context, id string, _ db.OpOptions) (json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	doc, ok := b.docs[id]
	if !ok {
		return nil, db.ErrNotFound
	}
	delete(b.docs, id)
	return json.Marshal(doc)
}

func (b *Backend) UnsetField(_ context.Context, field string, _ db.OpOptions) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var n int64
	for _, doc := range b.docs {
		if _, ok := doc[field]; ok {
			delete(doc, field)
			n++
		}
	}
	return n, nil
}

// KeyCounts answers [db.KeyCounter] exactly — every document is in memory, so
// there is nothing to sample.
func (b *Backend) KeyCounts(_ context.Context, _ db.OpOptions) (map[string]int64, int64, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	counts := map[string]int64{}
	var total int64
	for _, doc := range b.docs {
		if deleted(doc) {
			continue
		}
		total++
		for field := range doc {
			counts[field]++
		}
	}
	return counts, total, nil
}

// sortedIDs gives every scan a stable order. Ids are UUIDv7, so that order is
// creation order — the same default the SQL backends have from their primary
// key, which is what keeps an unsorted find comparable across backends.
func (b *Backend) sortedIDs() []string {
	ids := slices.Collect(maps.Keys(b.docs))
	slices.Sort(ids)
	return ids
}

func deleted(doc map[string]any) bool {
	meta, _ := doc["meta"].(map[string]any)
	return meta != nil && meta["deleted_at"] != nil
}

// apply performs the same deep-set the SQL backends do in one statement:
// missing parents are created, the version moves unless suppressed, and the
// unsets run last.
func apply(doc map[string]any, paths map[string]any, o db.UpdateOptions) {
	for path, value := range paths {
		set(doc, strings.Split(path, "."), normalize(value))
	}
	for _, path := range o.Unset {
		unset(doc, strings.Split(path, "."))
	}
	if !o.NoBump {
		current, _ := number(doc["version"])
		doc["version"] = current + 1
	}
}

func set(node map[string]any, segments []string, value any) {
	for _, segment := range segments[:len(segments)-1] {
		child, _ := node[segment].(map[string]any)
		if child == nil {
			child = map[string]any{}
			node[segment] = child
		}
		node = child
	}
	node[segments[len(segments)-1]] = value
}

func unset(node map[string]any, segments []string) {
	for _, segment := range segments[:len(segments)-1] {
		child, _ := node[segment].(map[string]any)
		if child == nil {
			return
		}
		node = child
	}
	delete(node, segments[len(segments)-1])
}

// normalize puts a filter or update value into the same shape a stored
// document has, so comparisons do not have to know whether a number arrived
// as an int from Go or a float from JSON.
func normalize(value any) any {
	switch v := value.(type) {
	case nil:
		return nil
	case json.RawMessage:
		var decoded any
		if json.Unmarshal(v, &decoded) == nil {
			return decoded
		}
		return string(v)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var decoded any
	if json.Unmarshal(raw, &decoded) != nil {
		return value
	}
	return decoded
}

// NewService returns a service backed by a fresh in-memory collection — the
// one-liner a test needs to exercise domain methods without a database.
func NewService[T any, PT db.Document[T]](o db.Options) (*db.Service[T, PT], error) {
	if o.Collection == "" {
		o.Collection = "memory"
	}
	return db.NewService[T, PT](New(), o)
}
