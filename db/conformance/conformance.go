// Package conformance is the behavioural suite every backend runs against its
// real storage.
//
// A contract described in prose drifts: the first backend defines what the
// words meant, and the second one implements what it read. Running the same
// assertions against Postgres and against a map is how "soft delete is the
// default" and "null matches an absent key" stay the same sentence in both.
//
//	func TestConformance(t *testing.T) {
//		conformance.Run(t, func(t *testing.T) *db.Service[conformance.Doc, *conformance.Doc] {
//			…
//		})
//	}
package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/mirairoad/howl-go/db"
)

// Doc is the document every case in the suite is written against: a required
// field, a defaulted field, a number to compare, a slice, and a sub-document
// to reach into with a dot-path.
type Doc struct {
	db.Doc
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Score   int64    `json:"score"`
	Tags    []string `json:"tags"`
	Profile Profile  `json:"profile"`
}

// Profile is the nested document, so the suite can prove a deep-set leaves
// its siblings alone.
type Profile struct {
	Plan  string `json:"plan"`
	Seats int64  `json:"seats"`
}

// Defaults fills the fields a caller may leave out.
func (d *Doc) Defaults() {
	if d.Kind == "" {
		d.Kind = "widget"
	}
	if d.Tags == nil {
		d.Tags = []string{}
	}
}

// Validate rejects a document with no name.
func (d *Doc) Validate() error {
	if d.Name == "" {
		return errors.New("name is required")
	}
	return nil
}

// Service is the type under test.
type Service = *db.Service[Doc, *Doc]

// Run executes the suite. The factory returns a service over an empty
// collection, and is called once per case so no case can see another's
// documents.
func Run(t *testing.T, factory func(*testing.T) Service) {
	t.Helper()
	cases := []struct {
		name string
		run  func(*testing.T, Service)
	}{
		{"CreateStampsTheEnvelope", createStampsTheEnvelope},
		{"CreateAppliesDefaults", createAppliesDefaults},
		{"CreateValidates", createValidates},
		{"GetMissingIsNotFound", getMissingIsNotFound},
		{"GetMany", getMany},
		{"PatchWritesAndBumps", patchWritesAndBumps},
		{"PatchPreservesSiblings", patchPreservesSiblings},
		{"PatchValidates", patchValidates},
		{"PatchFields", patchFields},
		{"PatchClearsAField", patchClearsAField},
		{"PatchLeavesOrphansAlone", patchLeavesOrphansAlone},
		{"SoftDeleteHides", softDeleteHides},
		{"DeleteTwiceIsNotFound", deleteTwiceIsNotFound},
		{"Restore", restore},
		{"HardDelete", hardDelete},
		{"FilterOperators", filterOperators},
		{"NullMatchesAbsent", nullMatchesAbsent},
		{"SortLimitSkip", sortLimitSkip},
		{"SortsMissingFieldsLast", sortsMissingFieldsLast},
		{"Count", count},
		{"Project", project},
		{"BulkWrites", bulkWrites},
		{"BulkRefusesEmptyFilter", bulkRefusesEmptyFilter},
		{"ReportAndDropField", reportAndDropField},
		{"BackfillClearsTheReport", backfillClearsTheReport},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.run(t, factory(t)) })
	}
}

func createStampsTheEnvelope(t *testing.T, s Service) {
	ctx := t.Context()
	doc := create(t, s, Doc{Name: "first"}, db.By("ada"))

	if doc.ID == "" {
		t.Fatal("create left the id empty")
	}
	if doc.Version != 1 {
		t.Errorf("version = %d, want 1", doc.Version)
	}
	if doc.Meta.CreatedBy != "ada" || doc.Meta.UpdatedBy != "ada" {
		t.Errorf("meta actor = %q/%q, want ada", doc.Meta.CreatedBy, doc.Meta.UpdatedBy)
	}
	if doc.Meta.CreatedAt == 0 || doc.Meta.UpdatedAt == 0 {
		t.Error("create left a timestamp at zero")
	}
	if doc.Meta.IsDeleted() {
		t.Error("a new document is deleted")
	}

	got, err := s.Get(ctx, doc.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "first" || got.ID != doc.ID || got.Version != 1 {
		t.Errorf("round trip = %+v, want the created document", got)
	}
}

func createAppliesDefaults(t *testing.T, s Service) {
	doc := create(t, s, Doc{Name: "defaulted"})
	if doc.Kind != "widget" {
		t.Errorf("kind = %q, want the default widget", doc.Kind)
	}
	if doc.Tags == nil {
		t.Error("tags stayed nil; Defaults should have set an empty slice")
	}
}

func createValidates(t *testing.T, s Service) {
	_, err := s.Create(t.Context(), Doc{Score: 1})
	if !errors.Is(err, db.ErrInvalid) {
		t.Fatalf("create with no name: %v, want ErrInvalid", err)
	}
	n, err := s.Count(t.Context(), db.Query{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("a rejected create stored %d documents", n)
	}
}

func getMissingIsNotFound(t *testing.T, s Service) {
	if _, err := s.Get(t.Context(), "nope"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("get of an absent id: %v, want ErrNotFound", err)
	}
	if _, err := s.Get(t.Context(), ""); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("get of an empty id: %v, want ErrNotFound", err)
	}
}

func getMany(t *testing.T, s Service) {
	a := create(t, s, Doc{Name: "a"})
	b := create(t, s, Doc{Name: "b"})

	got, err := s.GetMany(t.Context(), []string{a.ID, b.ID, "absent"})
	if err != nil {
		t.Fatalf("get many: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d documents, want 2", len(got))
	}
	if got[a.ID].Name != "a" || got[b.ID].Name != "b" {
		t.Errorf("get many returned %+v", got)
	}
}

func patchWritesAndBumps(t *testing.T, s Service) {
	ctx := t.Context()
	doc := create(t, s, Doc{Name: "before", Score: 1}, db.By("ada"))

	updated, err := s.Patch(ctx, doc.ID, func(d *Doc) {
		d.Name = "after"
		d.Score = 42
	}, db.By("grace"))
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if updated.Name != "after" || updated.Score != 42 {
		t.Errorf("patch produced %+v", updated)
	}
	if updated.Version != 2 {
		t.Errorf("version = %d, want 2", updated.Version)
	}
	if updated.Meta.CreatedBy != "ada" {
		t.Errorf("created_by = %q, want the original ada", updated.Meta.CreatedBy)
	}
	if updated.Meta.UpdatedBy != "grace" {
		t.Errorf("updated_by = %q, want grace", updated.Meta.UpdatedBy)
	}
	if updated.Meta.CreatedAt != doc.Meta.CreatedAt {
		t.Error("patch moved created_at")
	}

	stored, err := s.Get(ctx, doc.ID)
	if err != nil {
		t.Fatalf("get after patch: %v", err)
	}
	if stored.Name != "after" || stored.Version != 2 {
		t.Errorf("stored document = %+v, want the patched one", stored)
	}
}

func patchPreservesSiblings(t *testing.T, s Service) {
	ctx := t.Context()
	doc := create(t, s, Doc{Name: "nested", Profile: Profile{Plan: "free", Seats: 3}})

	updated, err := s.Patch(ctx, doc.ID, func(d *Doc) { d.Profile.Seats = 9 })
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if updated.Profile.Plan != "free" {
		t.Errorf("profile.plan = %q; a deep-set clobbered a sibling", updated.Profile.Plan)
	}
	if updated.Profile.Seats != 9 {
		t.Errorf("profile.seats = %d, want 9", updated.Profile.Seats)
	}
}

func patchValidates(t *testing.T, s Service) {
	ctx := t.Context()
	doc := create(t, s, Doc{Name: "keep"})

	if _, err := s.Patch(ctx, doc.ID, func(d *Doc) { d.Name = "" }); !errors.Is(err, db.ErrInvalid) {
		t.Fatalf("patch to an invalid document: %v, want ErrInvalid", err)
	}
	stored, err := s.Get(ctx, doc.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Name != "keep" || stored.Version != 1 {
		t.Errorf("a rejected patch changed the document: %+v", stored)
	}
}

func patchFields(t *testing.T, s Service) {
	ctx := t.Context()
	doc := create(t, s, Doc{Name: "sparse", Profile: Profile{Plan: "free", Seats: 1}})

	updated, err := s.PatchFields(ctx, doc.ID, db.Set{"profile.plan": "pro", "score": 7})
	if err != nil {
		t.Fatalf("patch fields: %v", err)
	}
	if updated.Profile.Plan != "pro" || updated.Profile.Seats != 1 || updated.Score != 7 {
		t.Errorf("patch fields produced %+v", updated)
	}
	if _, err := s.PatchFields(ctx, doc.ID, db.Set{"name": ""}); !errors.Is(err, db.ErrInvalid) {
		t.Errorf("patch fields skipped validation: %v", err)
	}
}

func patchClearsAField(t *testing.T, s Service) {
	ctx := t.Context()
	doc := create(t, s, Doc{Name: "clearable", Kind: "gadget", Score: 5})

	updated, err := s.Patch(ctx, doc.ID, func(d *Doc) { d.Score = 0 })
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if updated.Score != 0 {
		t.Errorf("score = %d, want it cleared", updated.Score)
	}
	stored, err := s.Get(ctx, doc.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Score != 0 {
		t.Errorf("stored score = %d; the cleared value did not reach storage", stored.Score)
	}
}

// A field the struct no longer declares must survive an ordinary patch: it is
// data from an older version of the type, and only DropField removes it.
func patchLeavesOrphansAlone(t *testing.T, s Service) {
	ctx := t.Context()
	doc := create(t, s, Doc{Name: "with orphan"})
	writeOrphan(t, s, doc.ID, "legacy_flag", true)

	if _, err := s.Patch(ctx, doc.ID, func(d *Doc) { d.Name = "patched" }); err != nil {
		t.Fatalf("patch: %v", err)
	}
	report, err := s.Report(ctx)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(report.Orphans) != 1 || report.Orphans[0].Field != "legacy_flag" {
		t.Errorf("orphans = %+v; the patch removed an undeclared field", report.Orphans)
	}
}

func softDeleteHides(t *testing.T, s Service) {
	ctx := t.Context()
	doc := create(t, s, Doc{Name: "doomed"}, db.By("ada"))

	if err := s.Delete(ctx, doc.ID, db.By("grace")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, doc.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("get of a deleted document: %v, want ErrNotFound", err)
	}

	deleted, err := s.Get(ctx, doc.ID, db.Deleted())
	if err != nil {
		t.Fatalf("get with Deleted: %v", err)
	}
	if !deleted.Meta.IsDeleted() {
		t.Error("meta.deleted_at was not stamped")
	}
	if deleted.Meta.DeletedBy == nil || *deleted.Meta.DeletedBy != "grace" {
		t.Errorf("deleted_by = %v, want grace", deleted.Meta.DeletedBy)
	}
	if deleted.Version != doc.Version {
		t.Errorf("version = %d, want %d — a delete is not an edit", deleted.Version, doc.Version)
	}

	found, err := s.Find(ctx, db.Query{})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("find returned %d deleted documents", len(found))
	}
	n, err := s.Count(ctx, db.Query{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
	withDeleted, err := s.Find(ctx, db.Query{Deleted: true})
	if err != nil {
		t.Fatalf("find with Deleted: %v", err)
	}
	if len(withDeleted) != 1 {
		t.Errorf("find with Deleted returned %d, want 1", len(withDeleted))
	}
}

func deleteTwiceIsNotFound(t *testing.T, s Service) {
	ctx := t.Context()
	doc := create(t, s, Doc{Name: "once"})
	if err := s.Delete(ctx, doc.ID); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := s.Delete(ctx, doc.ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("second delete: %v, want ErrNotFound", err)
	}
}

func restore(t *testing.T, s Service) {
	ctx := t.Context()
	doc := create(t, s, Doc{Name: "back"})
	if err := s.Delete(ctx, doc.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	restored, err := s.Restore(ctx, doc.ID, db.By("ada"))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.Meta.IsDeleted() {
		t.Error("restore left the document deleted")
	}
	if restored.Meta.DeletedBy != nil {
		t.Errorf("deleted_by = %v, want nil", restored.Meta.DeletedBy)
	}
	if _, err := s.Get(ctx, doc.ID); err != nil {
		t.Errorf("get after restore: %v", err)
	}

	// Restoring a live document is a no-op, not an error and not a write.
	again, err := s.Restore(ctx, doc.ID)
	if err != nil {
		t.Fatalf("restore of a live document: %v", err)
	}
	if again.Version != restored.Version {
		t.Errorf("version moved from %d to %d on a no-op restore", restored.Version, again.Version)
	}
}

func hardDelete(t *testing.T, s Service) {
	ctx := t.Context()
	doc := create(t, s, Doc{Name: "gone"})
	if err := s.Delete(ctx, doc.ID, db.Hard()); err != nil {
		t.Fatalf("hard delete: %v", err)
	}
	if _, err := s.Get(ctx, doc.ID, db.Deleted()); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("get with Deleted after a hard delete: %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, doc.ID, db.Hard()); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("second hard delete: %v, want ErrNotFound", err)
	}
}

func filterOperators(t *testing.T, s Service) {
	ctx := t.Context()
	create(t, s, Doc{Name: "a", Kind: "alpha", Score: 10, Profile: Profile{Plan: "free"}})
	create(t, s, Doc{Name: "b", Kind: "beta", Score: 20, Profile: Profile{Plan: "pro"}})
	create(t, s, Doc{Name: "c", Kind: "gamma", Score: 30, Profile: Profile{Plan: "pro"}})

	for _, c := range []struct {
		name  string
		where db.M
		want  []string
	}{
		{"eq", db.Eq("kind", "beta"), []string{"b"}},
		{"bare equality", db.M{"kind": "beta"}, []string{"b"}},
		{"ne", db.Ne("kind", "beta"), []string{"a", "c"}},
		{"in", db.In("kind", []string{"alpha", "gamma"}), []string{"a", "c"}},
		{"nin", db.Nin("kind", []string{"alpha", "gamma"}), []string{"b"}},
		{"gt", db.Gt("score", 10), []string{"b", "c"}},
		{"gte", db.Gte("score", 20), []string{"b", "c"}},
		{"lt", db.Lt("score", 30), []string{"a", "b"}},
		{"lte", db.Lte("score", 10), []string{"a"}},
		{"exists true", db.Exists("kind", true), []string{"a", "b", "c"}},
		{"exists false", db.Exists("missing_field", false), []string{"a", "b", "c"}},
		{"and", db.And(db.Eq("profile.plan", "pro"), db.Gt("score", 20)), []string{"c"}},
		{"or", db.Or(db.Eq("kind", "alpha"), db.Eq("kind", "gamma")), []string{"a", "c"}},
		{"dot path", db.Eq("profile.plan", "pro"), []string{"b", "c"}},
		{"two ops on one field", db.M{"score": db.M{db.OpGt: 10, db.OpLt: 30}}, []string{"b"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			found, err := s.Find(ctx, db.Query{Where: c.where, Sort: db.Asc("name")})
			if err != nil {
				t.Fatalf("find: %v", err)
			}
			if got := names(found); !equalStrings(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// A comparison against null matches a stored null and an absent key alike.
// This is the rule soft delete is built on, so it is worth its own case.
func nullMatchesAbsent(t *testing.T, s Service) {
	ctx := t.Context()
	doc := create(t, s, Doc{Name: "plain"})

	found, err := s.Find(ctx, db.Query{Where: db.Eq("absent_field", nil)})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("equality against null matched %d documents, want 1", len(found))
	}

	writeOrphan(t, s, doc.ID, "sometimes", nil)
	found, err = s.Find(ctx, db.Query{Where: db.Eq("sometimes", nil)})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(found) != 1 {
		t.Errorf("a stored null did not match an equality against null")
	}
	found, err = s.Find(ctx, db.Query{Where: db.Exists("sometimes", true)})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(found) != 1 {
		t.Errorf("a stored null is present, so $exists true must match it")
	}
}

func sortLimitSkip(t *testing.T, s Service) {
	ctx := t.Context()
	create(t, s, Doc{Name: "a", Score: 30})
	create(t, s, Doc{Name: "b", Score: 10})
	create(t, s, Doc{Name: "c", Score: 20})

	asc, err := s.Find(ctx, db.Query{Sort: db.Asc("score")})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got := names(asc); !equalStrings(got, []string{"b", "c", "a"}) {
		t.Errorf("ascending = %v", got)
	}

	desc, err := s.Find(ctx, db.Query{Sort: db.Desc("score")})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got := names(desc); !equalStrings(got, []string{"a", "c", "b"}) {
		t.Errorf("descending = %v", got)
	}

	page, err := s.Find(ctx, db.Query{Sort: db.Asc("score"), Limit: 1, Skip: 1})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got := names(page); !equalStrings(got, []string{"c"}) {
		t.Errorf("limit+skip = %v, want [c]", got)
	}

	one, err := s.One(ctx, db.Query{Sort: db.Desc("score")})
	if err != nil {
		t.Fatalf("one: %v", err)
	}
	if one.Name != "a" {
		t.Errorf("one = %q, want a", one.Name)
	}
	if _, err := s.One(ctx, db.Query{Where: db.Eq("name", "absent")}); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("one with no match: %v, want ErrNotFound", err)
	}
}

// Ordering has to be the same answer on every backend, and null placement is
// where the dialects disagree by default: Postgres puts nulls last ascending,
// SQLite puts them first. A document written before a field existed does not
// have the key, so this is not a corner case — it is what every collection
// looks like the day after somebody adds a field.
func sortsMissingFieldsLast(t *testing.T, s Service) {
	ctx := t.Context()
	create(t, s, Doc{Name: "a", Score: 10})
	create(t, s, Doc{Name: "b", Score: 20})
	absent := create(t, s, Doc{Name: "c"})
	if _, err := s.Backend().UpdatePaths(ctx, absent.ID, nil, db.UpdateOptions{
		Unset:  []string{"score"},
		NoBump: true,
	}); err != nil {
		t.Fatalf("removing the field: %v", err)
	}

	asc, err := s.Find(ctx, db.Query{Sort: db.Asc("score")})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got := names(asc); !equalStrings(got, []string{"a", "b", "c"}) {
		t.Errorf("ascending = %v, want the document without the field last", got)
	}

	desc, err := s.Find(ctx, db.Query{Sort: db.Desc("score")})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got := names(desc); !equalStrings(got, []string{"c", "b", "a"}) {
		t.Errorf("descending = %v, want the document without the field first", got)
	}
}

func count(t *testing.T, s Service) {
	ctx := t.Context()
	create(t, s, Doc{Name: "a", Kind: "x"})
	create(t, s, Doc{Name: "b", Kind: "x"})
	create(t, s, Doc{Name: "c", Kind: "y"})

	all, err := s.Count(ctx, db.Query{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if all != 3 {
		t.Errorf("count = %d, want 3", all)
	}
	some, err := s.Count(ctx, db.Query{Where: db.Eq("kind", "x")})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if some != 2 {
		t.Errorf("filtered count = %d, want 2", some)
	}
}

func project(t *testing.T, s Service) {
	ctx := t.Context()
	doc := create(t, s, Doc{Name: "projected", Kind: "gadget", Score: 5})

	found, err := s.Find(ctx, db.Query{Where: db.Eq("id", doc.ID), Project: []string{"name"}})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("got %d documents, want 1", len(found))
	}
	if found[0].Name != "projected" {
		t.Errorf("name = %q, want the projected field", found[0].Name)
	}
	if found[0].Kind != "" || found[0].Score != 0 {
		t.Errorf("unprojected fields came back set: %+v", found[0])
	}
	if found[0].ID != doc.ID || found[0].Version != 1 {
		t.Errorf("the envelope must survive a projection: %+v", found[0].Doc)
	}
}

func bulkWrites(t *testing.T, s Service) {
	ctx := t.Context()
	create(t, s, Doc{Name: "a", Kind: "x"})
	create(t, s, Doc{Name: "b", Kind: "x"})
	keep := create(t, s, Doc{Name: "c", Kind: "y"})

	n, err := s.PatchWhere(ctx, db.Eq("kind", "x"), db.Set{"score": 99}, db.By("ada"))
	if err != nil {
		t.Fatalf("patch where: %v", err)
	}
	if n != 2 {
		t.Errorf("patched %d documents, want 2", n)
	}
	patched, err := s.Find(ctx, db.Query{Where: db.Eq("score", 99)})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(patched) != 2 {
		t.Errorf("%d documents carry the new score, want 2", len(patched))
	}
	for _, doc := range patched {
		if doc.Version != 2 {
			t.Errorf("%s version = %d, want 2", doc.Name, doc.Version)
		}
		if doc.Meta.UpdatedBy != "ada" {
			t.Errorf("%s updated_by = %q, want ada", doc.Name, doc.Meta.UpdatedBy)
		}
	}

	deleted, err := s.DeleteWhere(ctx, db.Eq("kind", "x"), db.By("ada"))
	if err != nil {
		t.Fatalf("delete where: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted %d documents, want 2", deleted)
	}
	remaining, err := s.Find(ctx, db.Query{})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(remaining) != 1 || remaining[0].ID != keep.ID {
		t.Errorf("remaining = %v, want only c", names(remaining))
	}
}

func bulkRefusesEmptyFilter(t *testing.T, s Service) {
	ctx := t.Context()
	create(t, s, Doc{Name: "safe"})
	if _, err := s.DeleteWhere(ctx, nil); err == nil {
		t.Error("DeleteWhere accepted an empty filter")
	}
	if _, err := s.PatchWhere(ctx, db.M{}, db.Set{"score": 1}); err == nil {
		t.Error("PatchWhere accepted an empty filter")
	}
	if n, _ := s.Count(ctx, db.Query{}); n != 1 {
		t.Errorf("count = %d; a refused bulk still wrote", n)
	}
}

func reportAndDropField(t *testing.T, s Service) {
	ctx := t.Context()
	doc := create(t, s, Doc{Name: "reported"})
	writeOrphan(t, s, doc.ID, "legacy_flag", true)

	report, err := s.Report(ctx)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if report.Total != 1 {
		t.Errorf("total = %d, want 1", report.Total)
	}
	if len(report.Orphans) != 1 || report.Orphans[0].Field != "legacy_flag" {
		t.Fatalf("orphans = %+v, want legacy_flag", report.Orphans)
	}
	if report.Orphans[0].Docs != 1 {
		t.Errorf("orphan doc count = %d, want 1", report.Orphans[0].Docs)
	}
	if len(report.Missing) != 0 {
		t.Errorf("missing = %+v, want none", report.Missing)
	}

	if _, err := s.DropField(ctx, "name"); err == nil {
		t.Error("DropField removed a declared field")
	}
	if _, err := s.DropField(ctx, "meta"); err == nil {
		t.Error("DropField removed part of the envelope")
	}

	n, err := s.DropField(ctx, "legacy_flag")
	if err != nil {
		t.Fatalf("drop field: %v", err)
	}
	if n != 1 {
		t.Errorf("dropped from %d documents, want 1", n)
	}
	report, err = s.Report(ctx)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(report.Orphans) != 0 {
		t.Errorf("orphans = %+v after the drop", report.Orphans)
	}
}

func backfillClearsTheReport(t *testing.T, s Service) {
	ctx := t.Context()
	doc := create(t, s, Doc{Name: "old"})
	// A document written before `kind` existed: remove the key outright.
	if _, err := s.Backend().UpdatePaths(ctx, doc.ID, nil, db.UpdateOptions{
		Unset:  []string{"kind"},
		NoBump: true,
	}); err != nil {
		t.Fatalf("preparing the document: %v", err)
	}

	report, err := s.Report(ctx)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(report.Missing) != 1 || report.Missing[0].Field != "kind" {
		t.Fatalf("missing = %+v, want kind", report.Missing)
	}
	if string(report.Missing[0].Default) != `"widget"` {
		t.Errorf("default = %s, want the value Defaults produces", report.Missing[0].Default)
	}

	n, err := s.Backfill(ctx, "kind")
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 1 {
		t.Errorf("backfilled %d documents, want 1", n)
	}
	report, err = s.Report(ctx)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(report.Missing) != 0 {
		t.Errorf("missing = %+v after the backfill", report.Missing)
	}
	stored, err := s.Get(ctx, doc.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Kind != "widget" {
		t.Errorf("kind = %q after the backfill, want widget", stored.Kind)
	}
}

// ============================================================
// Helpers
// ============================================================

func create(t *testing.T, s Service, doc Doc, options ...db.Option) Doc {
	t.Helper()
	created, err := s.Create(t.Context(), doc, options...)
	if err != nil {
		t.Fatalf("create %q: %v", doc.Name, err)
	}
	return created
}

// writeOrphan puts a key the document struct does not declare into storage,
// which is the only way to produce the state a schema report exists to find.
func writeOrphan(t *testing.T, s Service, id, field string, value any) {
	t.Helper()
	if _, err := s.Backend().UpdatePaths(context.Background(), id,
		map[string]any{field: value}, db.UpdateOptions{NoBump: true}); err != nil {
		t.Fatalf("writing the orphan field: %v", err)
	}
}

func names(docs []Doc) []string {
	out := make([]string, len(docs))
	for i, doc := range docs {
		out[i] = doc.Name
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
