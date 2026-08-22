package db

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"
)

// Report is the diff between the Go struct and what is actually stored: which
// declared fields the documents do not have yet, and which stored fields the
// struct no longer declares.
//
// This is what replaces a migration framework. Adding a field to the struct
// is not a schema change — nothing has to be altered, because the document is
// one JSON value — but the documents written before it exists do not have the
// key, and code that tells "absent" from "zero" needs to know. Removing a
// field is the same story in reverse: the data stays until something removes
// it. Report finds both; [Service.PatchWhere] backfills the first and
// [Service.DropField] clears the second.
type Report struct {
	// Missing are declared fields that some active documents do not carry.
	Missing []MissingField `json:"missing"`
	// Orphans are stored fields the struct no longer declares. They are read
	// back as nothing and written back untouched by every patch, so they cost
	// storage and nothing else — until someone renames a field and wonders
	// where the old data went.
	Orphans []OrphanField `json:"orphans"`
	// Total is the number of active documents considered.
	Total int64 `json:"total"`
	// Exact is true when the counts come from the whole collection rather
	// than a sample. Backends implementing [KeyCounter] answer exactly.
	Exact bool `json:"exact"`
}

// MissingField is a declared field absent from some stored documents, with
// the value a backfill would write.
type MissingField struct {
	Field string `json:"field"`
	// Default is what the struct's zero value plus [Defaulter] produces for
	// this field — exactly what a document created now would carry.
	Default json.RawMessage `json:"default"`
	// Docs is how many active documents lack the field.
	Docs int64 `json:"docs"`
}

// OrphanField is a stored field the struct no longer declares.
type OrphanField struct {
	Field string `json:"field"`
	Docs  int64  `json:"docs"`
}

// The envelope is never reported: it is the store's, and it is present on
// every document by construction.
var envelopeFields = []string{IDPath, VersionPath, MetaPath}

// The number of documents sampled when the backend cannot count keys itself.
const reportSample = 200

// Report diffs the struct against the stored documents. Backends that can
// count JSON keys in one query answer exactly; the rest sample 200 active
// documents, and the report says which happened.
func (s *Service[T, PT]) Report(ctx context.Context) (Report, error) {
	ctx, cancel := s.deadline(ctx, 1)
	defer cancel()
	defer s.trace(time.Now(), "report", "collection", s.name)

	counts, total, exact, err := s.keyCounts(ctx)
	if err != nil {
		return Report{}, err
	}

	defaults, err := s.defaultDocument()
	if err != nil {
		return Report{}, err
	}

	report := Report{Total: total, Exact: exact}
	for field := range s.declared {
		if slices.Contains(envelopeFields, field) {
			continue
		}
		if absent := total - counts[field]; absent > 0 {
			report.Missing = append(report.Missing, MissingField{
				Field:   field,
				Default: defaults[field],
				Docs:    absent,
			})
		}
	}
	for field, n := range counts {
		if s.declared[field] || slices.Contains(envelopeFields, field) {
			continue
		}
		report.Orphans = append(report.Orphans, OrphanField{Field: field, Docs: n})
	}

	sort.Slice(report.Missing, func(i, j int) bool { return report.Missing[i].Field < report.Missing[j].Field })
	sort.Slice(report.Orphans, func(i, j int) bool { return report.Orphans[i].Field < report.Orphans[j].Field })
	return report, nil
}

func (s *Service[T, PT]) keyCounts(ctx context.Context) (map[string]int64, int64, bool, error) {
	if counter, ok := s.backend.(KeyCounter); ok {
		counts, total, err := counter.KeyCounts(ctx, OpOptions{})
		return counts, total, true, err
	}
	rows, err := s.backend.FindMany(ctx, active(nil), FindOptions{Limit: reportSample})
	if err != nil {
		return nil, 0, false, err
	}
	counts := map[string]int64{}
	for _, raw := range rows {
		fields, ok := object(raw)
		if !ok {
			continue
		}
		for field := range fields {
			counts[field]++
		}
	}
	return counts, int64(len(rows)), false, nil
}

// defaultDocument is what Create would store for a document nobody filled in:
// the struct's zero value with [Defaulter] applied. Reporting that as the
// backfill value means the report and the backfill agree by construction.
func (s *Service[T, PT]) defaultDocument() (map[string]json.RawMessage, error) {
	var doc T
	p := PT(&doc)
	if d, ok := any(p).(Defaulter); ok {
		d.Defaults()
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("db: %s: encoding defaults: %w", s.name, err)
	}
	fields, _ := object(raw)
	return fields, nil
}

// Backfill writes the default for one missing field to every active document
// that lacks it, and returns how many it touched. It is the other half of
// [Service.Report]: the value written is the same one the report showed.
func (s *Service[T, PT]) Backfill(ctx context.Context, field string) (int64, error) {
	if !s.declared[field] || slices.Contains(envelopeFields, field) {
		return 0, fmt.Errorf("db: %s: %q is not a declared document field", s.name, field)
	}
	defaults, err := s.defaultDocument()
	if err != nil {
		return 0, err
	}
	value, ok := defaults[field]
	if !ok {
		return 0, fmt.Errorf("db: %s: %q has no default to backfill", s.name, field)
	}
	return s.PatchWhere(ctx, Exists(field, false), Set{field: value})
}

// DropField removes a top-level field from every document that has it, and
// returns how many changed. It refuses the envelope and anything the struct
// still declares — dropping a live field is a data loss with a compile error
// waiting behind it, so remove it from the struct first.
//
// This is the one write that skips validation, versioning and audit: it is
// storage maintenance, not an edit.
func (s *Service[T, PT]) DropField(ctx context.Context, field string) (int64, error) {
	if slices.Contains(envelopeFields, field) {
		return 0, fmt.Errorf("db: %s: %q is part of the envelope and cannot be dropped", s.name, field)
	}
	if s.declared[field] {
		return 0, fmt.Errorf("db: %s: %q is still declared on the document struct — remove the Go field first", s.name, field)
	}
	ctx, cancel := s.deadline(ctx, bulkTimeoutMult)
	defer cancel()
	defer s.trace(time.Now(), "drop_field", "field", field)

	n, err := s.backend.UnsetField(ctx, field, OpOptions{})
	if err != nil {
		return 0, err
	}
	s.invalidate(ctx, nil)
	return n, nil
}

// Columns lists the promoted columns physically present in storage, each
// flagged as still declared or an orphan. Backends with no column concept
// answer [ErrUnsupported].
func (s *Service[T, PT]) Columns(ctx context.Context) ([]Column, error) {
	admin, ok := s.backend.(SchemaAdmin)
	if !ok {
		return nil, ErrUnsupported
	}
	return admin.Columns(ctx)
}

// DropColumn drops an orphan promoted column and its index. Still-declared
// columns are refused: the backend's promote list is the source of truth, and
// this surface only cleans up what removing an entry leaves behind.
//
// purgeData also removes the matching top-level JSON key from every document.
// Without it the documents keep the data and only the index goes.
func (s *Service[T, PT]) DropColumn(ctx context.Context, column string, purgeData bool) error {
	admin, ok := s.backend.(SchemaAdmin)
	if !ok {
		return ErrUnsupported
	}
	ctx, cancel := s.deadline(ctx, bulkTimeoutMult)
	defer cancel()
	if err := admin.DropColumn(ctx, column, purgeData); err != nil {
		return err
	}
	if purgeData {
		s.invalidate(ctx, nil)
	}
	return nil
}
