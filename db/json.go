package db

import (
	"encoding/json"
	"reflect"
	"strings"
)

// object decodes raw as a JSON object, reporting false for anything else
// (including null, a string, or an array).
func object(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	trimmed := strings.TrimLeft(string(raw), " \t\r\n")
	if !strings.HasPrefix(trimmed, "{") {
		return nil, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}
	return m, true
}

// jsonEqual compares two JSON values by meaning rather than by bytes. The
// stored side comes back from Postgres normalised (keys sorted, whitespace
// gone, numbers re-rendered) and the candidate side comes from
// encoding/json in Go struct-field order, so a byte comparison reports every
// unchanged field as changed.
func jsonEqual(a, b json.RawMessage) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// diff turns a before/after pair of documents into the dotted paths to set and
// to remove — the update the backend can apply in one statement.
//
// It recurses into objects present on both sides instead of writing the whole
// object, because a deep-set of a.b leaves a.c alone: two writers touching
// different fields of the same sub-document do not clobber each other. It
// stops recursing as soon as a key disappears from an object, and rewrites
// that object whole, since a deep-set has no way to express a removal below
// the top level.
//
// declared is the set of top-level JSON names the document struct itself
// declares. A stored key outside that set is an orphan — data from an older
// version of the struct — and must survive an ordinary patch untouched; only
// [Service.DropField] removes those, deliberately.
func diff(before, after json.RawMessage, declared map[string]bool) (map[string]any, []string, error) {
	b, ok := object(before)
	if !ok {
		return nil, nil, errNotAnObject
	}
	a, ok := object(after)
	if !ok {
		return nil, nil, errNotAnObject
	}

	set := map[string]any{}
	var unset []string

	for key, av := range a {
		// id and version belong to storage: id never changes, and version is
		// incremented by the backend inside the same statement.
		if key == IDPath || key == VersionPath {
			continue
		}
		bv, had := b[key]
		if had && jsonEqual(bv, av) {
			continue
		}
		if had {
			if bo, isObj := object(bv); isObj {
				if ao, isObj := object(av); isObj {
					diffObject(key, bo, ao, av, set)
					continue
				}
			}
		}
		set[key] = av
	}

	for key := range b {
		if _, present := a[key]; !present && declared[key] {
			unset = append(unset, key)
		}
	}
	return set, unset, nil
}

func diffObject(prefix string, before, after map[string]json.RawMessage, afterRaw json.RawMessage, set map[string]any) {
	for key := range before {
		if _, present := after[key]; !present {
			set[prefix] = afterRaw
			return
		}
	}
	for key, av := range after {
		path := prefix + "." + key
		bv, had := before[key]
		if had && jsonEqual(bv, av) {
			continue
		}
		if had {
			if bo, isObj := object(bv); isObj {
				if ao, isObj := object(av); isObj {
					diffObject(path, bo, ao, av, set)
					continue
				}
			}
		}
		set[path] = av
	}
}

// applySet deep-sets dotted paths into a document, creating missing parent
// objects on the way — the same rule the backends' deep-set follows, applied
// in process so [Service.PatchFields] can validate the result before writing.
func applySet(doc json.RawMessage, values Set) (json.RawMessage, error) {
	var root map[string]any
	if err := json.Unmarshal(doc, &root); err != nil {
		return nil, err
	}
	for path, value := range values {
		segments := strings.Split(path, ".")
		target := root
		for _, segment := range segments[:len(segments)-1] {
			child, _ := target[segment].(map[string]any)
			if child == nil {
				child = map[string]any{}
				target[segment] = child
			}
			target = child
		}
		target[segments[len(segments)-1]] = value
	}
	return json.Marshal(root)
}

// prune drops every top-level key and dot-path not named in keep. The
// envelope always survives: a document whose version came back zero would
// fail its next optimistic lock.
func prune(doc json.RawMessage, keep []string) json.RawMessage {
	tree := map[string]any{}
	for _, path := range append([]string{IDPath, VersionPath, MetaPath}, keep...) {
		node := tree
		segments := strings.Split(path, ".")
		for i, segment := range segments {
			if i == len(segments)-1 {
				node[segment] = true
				break
			}
			child, _ := node[segment].(map[string]any)
			if child == nil {
				// A broader path already selected this whole sub-document.
				if node[segment] == true {
					break
				}
				child = map[string]any{}
				node[segment] = child
			}
			node = child
		}
	}
	pruned, err := pruneNode(doc, tree)
	if err != nil {
		return doc
	}
	return pruned
}

func pruneNode(raw json.RawMessage, keep map[string]any) (json.RawMessage, error) {
	fields, ok := object(raw)
	if !ok {
		return raw, nil
	}
	out := make(map[string]json.RawMessage, len(keep))
	for key, want := range keep {
		value, present := fields[key]
		if !present {
			continue
		}
		if child, nested := want.(map[string]any); nested {
			sub, err := pruneNode(value, child)
			if err != nil {
				return nil, err
			}
			out[key] = sub
			continue
		}
		out[key] = value
	}
	return json.Marshal(out)
}

// declaredFields returns the top-level JSON names a document struct declares,
// following embedded structs the way encoding/json does — which is how the
// envelope's own names arrive here.
func declaredFields(t reflect.Type) map[string]bool {
	names := map[string]bool{}
	collectFields(t, names)
	return names
}

func collectFields(t reflect.Type, into map[string]bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := range t.NumField() {
		f := t.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if tag == "-" {
			continue
		}
		if f.Anonymous && tag == "" {
			collectFields(f.Type, into)
			continue
		}
		if !f.IsExported() {
			continue
		}
		if tag == "" {
			tag = f.Name
		}
		into[tag] = true
	}
}
