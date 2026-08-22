package memdb

import (
	"strings"

	"github.com/mirairoad/howl-go/db"
)

// matches evaluates the neutral filter grammar against one document. It is
// the Go twin of the SQL compilers, and the reason the conformance suite can
// state a single expected answer for every backend.
func matches(doc map[string]any, where db.M) bool {
	for key, condition := range where {
		switch key {
		case db.OpAnd:
			for _, branch := range branches(condition) {
				if !matches(doc, branch) {
					return false
				}
			}
		case db.OpOr:
			list := branches(condition)
			if len(list) == 0 {
				return false
			}
			hit := false
			for _, branch := range list {
				if matches(doc, branch) {
					hit = true
					break
				}
			}
			if !hit {
				return false
			}
		default:
			if strings.HasPrefix(key, "$") {
				return false
			}
			if !fieldMatches(doc, key, condition) {
				return false
			}
		}
	}
	return true
}

func branches(value any) []db.M {
	switch list := value.(type) {
	case []db.M:
		return list
	case []any:
		out := make([]db.M, 0, len(list))
		for _, item := range list {
			switch branch := item.(type) {
			case db.M:
				out = append(out, branch)
			case map[string]any:
				out = append(out, db.M(branch))
			}
		}
		return out
	}
	return nil
}

// operators reports whether a condition is an operator object rather than a
// value to compare against. A map with no $-prefixed key is a sub-document
// being matched whole, which is how Mongo reads it too.
func operators(condition any) (map[string]any, bool) {
	var fields map[string]any
	switch typed := condition.(type) {
	case db.M:
		fields = typed
	case map[string]any:
		fields = typed
	default:
		return nil, false
	}
	for key := range fields {
		if strings.HasPrefix(key, "$") {
			return fields, true
		}
	}
	return nil, false
}

func fieldMatches(doc map[string]any, path string, condition any) bool {
	value, present := lookup(doc, path)
	ops, isOps := operators(condition)
	if !isOps {
		return equal(value, present, condition)
	}
	for op, operand := range ops {
		if !operator(value, present, op, operand) {
			return false
		}
	}
	return true
}

func operator(value any, present bool, op string, operand any) bool {
	switch op {
	case db.OpEq:
		return equal(value, present, operand)
	case db.OpNe:
		return !equal(value, present, operand)
	case db.OpIn:
		for _, candidate := range list(operand) {
			if equal(value, present, candidate) {
				return true
			}
		}
		return false
	case db.OpNin:
		for _, candidate := range list(operand) {
			if equal(value, present, candidate) {
				return false
			}
		}
		return true
	case db.OpGt, db.OpGte, db.OpLt, db.OpLte:
		if !present {
			return false
		}
		order, ok := compare(value, normalize(operand))
		if !ok {
			return false
		}
		switch op {
		case db.OpGt:
			return order > 0
		case db.OpGte:
			return order >= 0
		case db.OpLt:
			return order < 0
		default:
			return order <= 0
		}
	case db.OpExists:
		want, _ := operand.(bool)
		return present == want
	}
	return false
}

// equal implements the one rule that is not obvious: a comparison against
// null matches both a stored null and an absent key. Every read in the store
// depends on it — an active document is one whose meta.deleted_at is null,
// and a document written before soft delete existed has no such key at all.
func equal(value any, present bool, operand any) bool {
	operand = normalize(operand)
	if operand == nil {
		return !present || value == nil
	}
	if !present {
		return false
	}
	order, ok := compare(value, operand)
	return ok && order == 0
}

func list(operand any) []any {
	switch values := operand.(type) {
	case []any:
		return values
	case nil:
		return nil
	}
	if normalized, ok := normalize(operand).([]any); ok {
		return normalized
	}
	return nil
}

func lookup(doc map[string]any, path string) (any, bool) {
	node := any(doc)
	for _, segment := range strings.Split(path, ".") {
		object, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		node, ok = object[segment]
		if !ok {
			return nil, false
		}
	}
	return node, true
}

func compare(a, b any) (int, bool) {
	if x, ok := number(a); ok {
		if y, ok := number(b); ok {
			switch {
			case x < y:
				return -1, true
			case x > y:
				return 1, true
			default:
				return 0, true
			}
		}
		return 0, false
	}
	if x, ok := a.(string); ok {
		if y, ok := b.(string); ok {
			return strings.Compare(x, y), true
		}
		return 0, false
	}
	if x, ok := a.(bool); ok {
		if y, ok := b.(bool); ok {
			switch {
			case x == y:
				return 0, true
			case y:
				return -1, true
			default:
				return 1, true
			}
		}
	}
	return 0, false
}

func number(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	}
	return 0, false
}

// less orders two documents by the sort terms. A missing value goes last
// ascending and first descending — the placement the SQL backends spell out
// explicitly, because their dialects disagree about it by default.
//
// A stored null counts as missing here. The SQL backends only agree with that
// where the path is promoted to a column; an unpromoted JSON null orders by
// the dialect's own rule for it, which is the one ordering question this
// contract does not answer.
func less(a, b map[string]any, sort db.Sort) bool {
	for _, key := range sort {
		x, hasX := lookup(a, key.Field)
		y, hasY := lookup(b, key.Field)
		if x == nil {
			hasX = false
		}
		if y == nil {
			hasY = false
		}
		if !hasX || !hasY {
			if hasX == hasY {
				continue
			}
			return hasX != key.Desc
		}
		order, ok := compare(x, y)
		if !ok || order == 0 {
			continue
		}
		if key.Desc {
			return order > 0
		}
		return order < 0
	}
	return false
}
