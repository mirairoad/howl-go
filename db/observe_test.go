package db_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mirairoad/howl-go/core/observe"
	"github.com/mirairoad/howl-go/db"
	"github.com/mirairoad/howl-go/db/memdb"
)

type Note struct {
	db.Doc
	Title string `json:"title"`
	Body  string `json:"body"`
}

func notes(t *testing.T, cache db.Cache) (*db.Service[Note, *Note], *observe.Recorder, context.Context) {
	t.Helper()
	svc, err := memdb.NewService[Note](db.Options{Collection: "notes", Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	rec := &observe.Recorder{}
	return svc, rec, observe.With(context.Background(), rec)
}

// find returns the one span with this name, failing if there is not exactly one.
func one(t *testing.T, rec *observe.Recorder, name string) *observe.Recorded {
	t.Helper()
	got := rec.Named(name)
	if len(got) != 1 {
		t.Fatalf("%d spans named %q; have %v", len(got), name, names(rec))
	}
	return got[0]
}

func names(rec *observe.Recorder) []string {
	var out []string
	for _, s := range rec.Spans() {
		out = append(out, s.Name)
	}
	return out
}

// Every operation is a span, the backend call is a span inside it, and the
// difference between them is what the service itself cost.
func TestOperationSpanWrapsAStorageSpan(t *testing.T) {
	svc, rec, ctx := notes(t, db.Cache{})
	created, err := svc.Create(ctx, Note{Title: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, created.ID); err != nil {
		t.Fatal(err)
	}

	create := one(t, rec, "db.create notes")
	if create.Attrs[observe.Kind] != "db" || create.Attrs["db.collection.name"] != "notes" ||
		create.Attrs["db.operation.name"] != "create" || create.Attrs["db.system.name"] != "mem" {
		t.Errorf("create attributes: %v", create.Attrs)
	}
	insert := one(t, rec, "mem.insert notes")
	if insert.Parent != create || insert.Attrs[observe.Kind] != "db.storage" {
		t.Errorf("the insert is not a storage span under the create: %+v", insert)
	}
	get := one(t, rec, "db.get notes")
	if findOne := one(t, rec, "mem.find_one notes"); findOne.Parent != get {
		t.Error("the read is not under the get")
	}
	for _, s := range rec.Spans() {
		if !s.Ended {
			t.Errorf("span %q never ended", s.Name)
		}
	}
}

// A patch retries under an optimistic lock. The count says how many times,
// which is the difference between a slow write and one about to give up.
func TestPatchRecordsItsAttempts(t *testing.T) {
	svc, rec, ctx := notes(t, db.Cache{})
	created, _ := svc.Create(ctx, Note{Title: "first"})
	if _, err := svc.Patch(ctx, created.ID, func(n *Note) { n.Title = "second" }); err != nil {
		t.Fatal(err)
	}
	if got := one(t, rec, "db.patch notes").Attrs["howl.db.attempts"]; got != 1 {
		t.Errorf("howl.db.attempts = %v on an uncontended patch, want 1", got)
	}
}

// One is its own span with the Find it delegates to inside it: a trace should
// say which of the two the caller wrote.
func TestOneIsItsOwnSpanAboveFind(t *testing.T) {
	svc, rec, ctx := notes(t, db.Cache{})
	svc.Create(ctx, Note{Title: "first"}) //nolint:errcheck
	if _, err := svc.One(ctx, db.Query{Where: db.Eq("title", "first")}); err != nil {
		t.Fatal(err)
	}
	got, find := one(t, rec, "db.one notes"), one(t, rec, "db.find notes")
	if find.Parent != got {
		t.Error("the find is not under the one")
	}
	if got.Attrs["howl.db.rows"] != int64(1) || find.Attrs["howl.db.rows"] != int64(1) {
		t.Errorf("rows: one=%v find=%v", got.Attrs["howl.db.rows"], find.Attrs["howl.db.rows"])
	}
}

// Cache lookups are counted per operation, not reported one event at a time:
// GetMany of many ids is one span with two numbers on it.
func TestCacheLookupsAreCountedPerOperation(t *testing.T) {
	svc, rec, ctx := notes(t, db.Cache{TTL: time.Minute})
	var ids []string
	for _, title := range []string{"a", "b", "c"} {
		n, err := svc.Create(ctx, Note{Title: title})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, n.ID)
	}
	// Cold: three misses on one span. Warm: three hits, and no storage call.
	if _, err := svc.GetMany(ctx, ids); err != nil {
		t.Fatal(err)
	}
	cold := rec.Named("db.get_many notes")[0]
	if cold.Attrs["howl.cache.misses"] != 3 || cold.Attrs["howl.cache.hits"] != nil {
		t.Errorf("cold: %v", cold.Attrs)
	}
	if cold.Attrs["howl.db.rows"] != int64(3) {
		t.Errorf("cold rows: %v", cold.Attrs["howl.db.rows"])
	}
	if len(cold.Events) != 0 {
		t.Errorf("one event per lookup is back: %v", cold.Events)
	}

	warm := &observe.Recorder{}
	if _, err := svc.GetMany(observe.With(context.Background(), warm), ids); err != nil {
		t.Fatal(err)
	}
	hot := warm.Named("db.get_many notes")[0]
	if hot.Attrs["howl.cache.hits"] != 3 || hot.Attrs["howl.cache.misses"] != nil {
		t.Errorf("warm: %v", hot.Attrs)
	}
	for _, s := range warm.Spans() {
		if s.Attrs[observe.Kind] == "db.storage" {
			t.Errorf("a cached batch still went to storage: %q", s.Name)
		}
	}
}

// An operation inside a transaction consults no cache in either direction. A
// hit rate that looks wrong for such a collection is this, not a broken cache.
func TestSessionIsOnTheSpan(t *testing.T) {
	svc, rec, ctx := notes(t, db.Cache{TTL: time.Minute})
	created, _ := svc.Create(ctx, Note{Title: "first"})
	if _, err := svc.Get(ctx, created.ID, db.Session(txHandle{})); err != nil && !errors.Is(err, db.ErrNotFound) {
		t.Fatal(err)
	}
	for _, s := range rec.Named("db.get notes") {
		if s.Attrs["howl.db.session"] != true {
			t.Errorf("session not recorded: %v", s.Attrs)
		}
	}
}

type txHandle struct{}

// A set-wide write says whether the backend did it in one statement or the
// service walked the rows, because that is the difference between one query
// and a thousand.
func TestBulkSaysWhichPathItTook(t *testing.T) {
	svc, rec, ctx := notes(t, db.Cache{})
	for _, title := range []string{"a", "b"} {
		svc.Create(ctx, Note{Title: title}) //nolint:errcheck
	}
	n, err := svc.PatchWhere(ctx, db.Exists("title", true), db.Set{"body": "filled"})
	if err != nil {
		t.Fatal(err)
	}
	span := one(t, rec, "db.patch_where notes")
	if span.Attrs["howl.db.rows"] != n {
		t.Errorf("rows = %v, the call returned %d", span.Attrs["howl.db.rows"], n)
	}
	path, _ := span.Attrs["howl.db.bulk"].(string)
	if path != "native" && path != "fallback" {
		t.Errorf("howl.db.bulk = %q", path)
	}
	// Whichever path, the storage work is a span under the operation.
	var storage int
	for _, s := range rec.Spans() {
		if s.Parent == span && s.Attrs[observe.Kind] == "db.storage" {
			storage++
		}
	}
	if storage == 0 {
		t.Errorf("no storage span under the bulk write; have %v", names(rec))
	}
}

// Maintenance is an operation like any other: it can be slow, and it can fail.
func TestMaintenanceIsTraced(t *testing.T) {
	svc, rec, ctx := notes(t, db.Cache{})
	svc.Create(ctx, Note{Title: "first"}) //nolint:errcheck
	if _, err := svc.Report(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DropField(ctx, "legacy"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"db.report notes", "db.drop_field notes"} {
		one(t, rec, want)
	}
	if drop := one(t, rec, "db.drop_field notes"); drop.Attrs["db.operation.name"] != "drop_field" {
		t.Errorf("drop_field attributes: %v", drop.Attrs)
	}
}

// A read that waited on another caller's query did no query of its own. The
// span has to say so, or it reads as a slow database. Made deterministic with
// a backend that takes its time: without the delay the memory store answers
// before the second caller arrives, and nothing ever coalesces.
func TestCoalescedReadIsMarked(t *testing.T) {
	fast, _, ctx := notes(t, db.Cache{})
	created, err := fast.Create(ctx, Note{Title: "first"})
	if err != nil {
		t.Fatal(err)
	}

	rec := &observe.Recorder{}
	ctx = observe.With(context.Background(), rec)
	svc, err := db.NewService[Note, *Note](slowReads{fast.Backend(), 50 * time.Millisecond},
		db.Options{Collection: "notes", Cache: db.Cache{TTL: time.Minute}})
	if err != nil {
		t.Fatal(err)
	}

	const callers = 8
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			if _, err := svc.Get(ctx, created.ID); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	var coalesced, storage int
	for _, sp := range rec.Spans() {
		if sp.Name == "db.get notes" && sp.Attrs["howl.db.coalesced"] == true {
			coalesced++
		}
		if sp.Attrs[observe.Kind] == "db.storage" {
			storage++
		}
	}
	if storage != 1 {
		t.Errorf("%d storage reads for %d concurrent callers; the flight did not collapse them", storage, callers)
	}
	if coalesced != callers-1 {
		t.Errorf("%d spans marked coalesced, want %d — one leader, the rest waiting", coalesced, callers-1)
	}
}

// slowReads is any backend with a delay in front of its single-document read.
// Embedding rather than implementing: the other eight methods are the inner
// backend's, and a method added to the interface later stays compiling here.
type slowReads struct {
	db.Backend
	delay time.Duration
}

func (s slowReads) FindOne(ctx context.Context, where db.M, o db.OpOptions) (json.RawMessage, error) {
	time.Sleep(s.delay)
	return s.Backend.FindOne(ctx, where, o)
}

// The filter reaches the span, redacted: field names and operators always,
// values only when they cannot be personal data. This is what makes a trace
// able to say which documents a set-wide write touched.
func TestFilterIsRecordedAndRedacted(t *testing.T) {
	svc, rec, ctx := notes(t, db.Cache{})
	if _, err := svc.Find(ctx, db.Query{
		Where: db.And(db.Eq("email", "ada@example.com"), db.Eq("org_id", "0193a5c2-5f4e-7b3a-9c1d-2e6f8a0b4c7d")),
		Sort:  db.Desc("created"),
		Limit: 20,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := one(t, rec, "db.find notes").Attrs["db.query.text"].(string)
	for _, want := range []string{`"email"`, `"$eq"`, `"org_id":{"$eq":"0193a5c2-5f4e-7b3a-9c1d-2e6f8a0b4c7d"}`, `"limit":20`, `"sort":{"created":-1}`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "ada@example.com") || strings.Contains(got, "example.com") {
		t.Fatalf("an address reached the span: %s", got)
	}
}

// A set-wide write records both halves: which documents, and what was done
// to them.
func TestSetWideWriteRecordsTheDamage(t *testing.T) {
	svc, rec, ctx := notes(t, db.Cache{})
	svc.Create(ctx, Note{Title: "first"}) //nolint:errcheck
	if _, err := svc.PatchWhere(ctx, db.Exists("title", true), db.Set{"body": "filled", "archived": true}); err != nil {
		t.Fatal(err)
	}
	span := one(t, rec, "db.patch_where notes")
	where, _ := span.Attrs["db.query.text"].(string)
	set, _ := span.Attrs["howl.db.set"].(string)
	if !strings.Contains(where, `"title":{"$exists":true}`) {
		t.Errorf("filter: %s", where)
	}
	if !strings.Contains(set, `"archived":true`) || !strings.Contains(set, `"body":"?"`) {
		t.Errorf("set: %s", set)
	}
}

// The id is the one value kept by name: it is how the document is found again.
func TestDocumentIdIsRecorded(t *testing.T) {
	svc, rec, ctx := notes(t, db.Cache{})
	created, _ := svc.Create(ctx, Note{Title: "first"})
	if _, err := svc.Get(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"db.create notes", "db.get notes", "db.delete notes"} {
		if got := one(t, rec, name).Attrs["howl.db.id"]; got != created.ID {
			t.Errorf("%s: howl.db.id = %v, want %s", name, got, created.ID)
		}
	}
}

// Off means off: not the values, not the field names, not the id.
func TestTraceOffRecordsNoQuery(t *testing.T) {
	svc, err := memdb.NewService[Note](db.Options{Collection: "notes", Trace: observe.Off})
	if err != nil {
		t.Fatal(err)
	}
	rec := &observe.Recorder{}
	ctx := observe.With(context.Background(), rec)
	created, _ := svc.Create(ctx, Note{Title: "first"})
	svc.Get(ctx, created.ID)                                //nolint:errcheck
	svc.Find(ctx, db.Query{Where: db.Eq("title", "first")}) //nolint:errcheck
	for _, s := range rec.Spans() {
		for _, key := range []string{"db.query.text", "howl.db.id", "howl.db.set"} {
			if v, ok := s.Attrs[key]; ok {
				t.Errorf("%s recorded %s = %v with Trace off", s.Name, key, v)
			}
		}
		// The operation itself is still traced — this is about values, not
		// about whether the database shows up at all.
		if s.Attrs["db.collection.name"] != "notes" {
			t.Errorf("%s lost its identity: %v", s.Name, s.Attrs)
		}
	}
}

// The whole point of keeping timestamps: a set-wide delete says what range it
// swept, which is the question asked the moment somebody notices the row count.
func TestRangeQueryIsLegible(t *testing.T) {
	svc, rec, ctx := notes(t, db.Cache{})
	cutoff := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := svc.DeleteWhere(ctx, db.Lt("meta.created_at", cutoff)); err != nil {
		t.Fatal(err)
	}
	got, _ := one(t, rec, "db.delete_where notes").Attrs["db.query.text"].(string)
	if !strings.Contains(got, `"meta.created_at":{"$lt":"2026-01-02T03:04:05Z"}`) {
		t.Errorf("the range is not legible: %s", got)
	}
}
