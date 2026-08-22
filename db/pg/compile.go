package pg

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/mirairoad/howl-go/db"
)

// Type is the storage type a promoted column carries, shared with the other
// SQL backends so an options literal moves between them unchanged.
type Type = db.PromoteType

// The promotable types, re-exported so pg.Bigint reads naturally beside
// pg.Options.
const (
	Text    = db.Text
	Bigint  = db.Bigint
	Numeric = db.Numeric
	Boolean = db.Boolean
)

// column is one promoted document path and the generated column behind it.
type column struct {
	name     string
	segments []string
	typ      Type
}

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// assertIdent refuses anything that could escape quoting. Identifiers here
// come from developer configuration and from filter keys, never from a
// request body — but a filter key reaching this function from a decoded JSON
// query is one refactor away, so it is checked rather than trusted.
func assertIdent(segment string) (string, error) {
	if !identifier.MatchString(segment) {
		return "", fmt.Errorf("pg: invalid identifier %q", segment)
	}
	return segment, nil
}

func jsonbPath(segments []string) (string, error) {
	var b strings.Builder
	b.WriteString("doc")
	for _, segment := range segments {
		if _, err := assertIdent(segment); err != nil {
			return "", err
		}
		b.WriteString("->'")
		b.WriteString(segment)
		b.WriteString("'")
	}
	return b.String(), nil
}

// compiler turns the neutral filter grammar into a parametrized WHERE clause.
//
// Routing is the whole trick: id hits the primary key, a promoted path hits
// its typed generated column — a real B-tree index with real planner
// statistics — and everything else compiles to JSONB operators on doc. A
// query is fast exactly where someone promoted the path it filters on, and
// still correct everywhere else.
type compiler struct {
	promoted map[string]column
	params   []any
	start    int
}

type target struct {
	expr     string
	isColumn bool
	segments []string
	typ      Type
}

func (c *compiler) push(value any) string {
	c.params = append(c.params, value)
	return "$" + strconv.Itoa(c.start+len(c.params)-1)
}

func (c *compiler) target(path string) (target, error) {
	if path == "id" {
		return target{expr: "id", isColumn: true, segments: []string{"id"}, typ: Text}, nil
	}
	segments := strings.Split(path, ".")
	for _, segment := range segments {
		if _, err := assertIdent(segment); err != nil {
			return target{}, err
		}
	}
	if promoted, ok := c.promoted[path]; ok {
		return target{
			expr:     `"` + promoted.name + `"`,
			isColumn: true,
			segments: segments,
			typ:      promoted.typ,
		}, nil
	}
	expr, err := jsonbPath(segments)
	if err != nil {
		return target{}, err
	}
	return target{expr: expr, segments: segments}, nil
}

// value binds one operand. A column takes the Go value straight through the
// driver; a JSONB expression takes the value's JSON encoding, cast, so that
// the comparison happens between two jsonb values rather than between jsonb
// and text.
func (c *compiler) value(t target, v any) (string, error) {
	if t.isColumn {
		return c.push(v), nil
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return c.push(string(encoded)) + "::jsonb", nil
}

// nullMatch is Mongo's null rule in SQL: a comparison against null matches a
// stored null and an absent key alike. A generated column is SQL NULL in both
// cases; a JSONB expression is SQL NULL only when the key is absent, so the
// stored-null case is spelled out.
func nullMatch(t target) string {
	if t.isColumn {
		return t.expr + " IS NULL"
	}
	return "(" + t.expr + " IS NULL OR " + t.expr + " = 'null'::jsonb)"
}

func (c *compiler) field(path string, condition any) (string, error) {
	t, err := c.target(path)
	if err != nil {
		return "", err
	}
	ops, isOps := operatorObject(condition)
	if !isOps {
		if condition == nil {
			return nullMatch(t), nil
		}
		bound, err := c.value(t, condition)
		if err != nil {
			return "", err
		}
		return t.expr + " = " + bound, nil
	}

	parts := make([]string, 0, len(ops))
	for _, op := range sortedKeys(ops) {
		part, err := c.operator(t, op, ops[op])
		if err != nil {
			return "", err
		}
		parts = append(parts, part)
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return "(" + strings.Join(parts, " AND ") + ")", nil
}

func (c *compiler) operator(t target, op string, operand any) (string, error) {
	switch op {
	case db.OpEq:
		if operand == nil {
			return nullMatch(t), nil
		}
		bound, err := c.value(t, operand)
		return t.expr + " = " + bound, err

	case db.OpNe:
		if operand == nil {
			return "NOT " + nullMatch(t), nil
		}
		// IS DISTINCT FROM rather than <>: a document without the field must
		// match $ne, and <> against SQL NULL is NULL, which does not.
		bound, err := c.value(t, operand)
		return t.expr + " IS DISTINCT FROM " + bound, err

	case db.OpIn, db.OpNin:
		return c.membership(t, op, operand)

	case db.OpGt, db.OpGte, db.OpLt, db.OpLte:
		bound, err := c.value(t, operand)
		if err != nil {
			return "", err
		}
		return t.expr + " " + comparison[op] + " " + bound, nil

	case db.OpExists:
		// Presence is a JSONB question even for a promoted path: a NULL column
		// cannot tell a stored null (present) from an absent key.
		expr, err := jsonbPath(t.segments)
		if err != nil {
			return "", err
		}
		if want, _ := operand.(bool); want {
			return expr + " IS NOT NULL", nil
		}
		return expr + " IS NULL", nil
	}
	return "", fmt.Errorf("pg: unsupported filter operator %q — the grammar is "+
		"$eq $ne $in $nin $gt $gte $lt $lte $or $and $exists; anything richer belongs behind SQL()", op)
}

var comparison = map[string]string{db.OpGt: ">", db.OpGte: ">=", db.OpLt: "<", db.OpLte: "<="}

// membership compiles $in and $nin. The values are bound one parameter each
// rather than as an array: an array bind would need a driver that encodes Go
// slices, and this package has no driver dependency to lean on.
func (c *compiler) membership(t target, op string, operand any) (string, error) {
	values, ok := operand.([]any)
	if !ok {
		return "", fmt.Errorf("pg: %s expects a list, got %T", op, operand)
	}
	bound := make([]string, 0, len(values))
	hasNull := false
	for _, v := range values {
		if v == nil {
			hasNull = true
			continue
		}
		text, err := c.value(t, v)
		if err != nil {
			return "", err
		}
		bound = append(bound, text)
	}

	var parts []string
	if op == db.OpIn {
		if len(bound) > 0 {
			parts = append(parts, t.expr+" IN ("+strings.Join(bound, ", ")+")")
		}
		if hasNull {
			parts = append(parts, nullMatch(t))
		}
		if len(parts) == 0 {
			return "FALSE", nil
		}
		return join(parts, " OR "), nil
	}

	if len(bound) > 0 {
		notIn := t.expr + " NOT IN (" + strings.Join(bound, ", ") + ")"
		// An absent field matches $nin, but NOT IN over SQL NULL yields NULL —
		// so absence is allowed explicitly, unless the list contains null, in
		// which case absence is what the caller excluded.
		if hasNull {
			parts = append(parts, notIn)
		} else {
			parts = append(parts, "("+t.expr+" IS NULL OR "+notIn+")")
		}
	}
	if hasNull {
		parts = append(parts, "NOT "+nullMatch(t))
	}
	if len(parts) == 0 {
		return "TRUE", nil
	}
	return join(parts, " AND "), nil
}

func (c *compiler) compile(where db.M) (string, error) {
	if len(where) == 0 {
		return "TRUE", nil
	}
	parts := make([]string, 0, len(where))
	for _, key := range sortedKeys(where) {
		value := where[key]
		switch key {
		case db.OpAnd, db.OpOr:
			separator := " AND "
			empty := "TRUE"
			if key == db.OpOr {
				separator, empty = " OR ", "FALSE"
			}
			branches, err := branchList(value)
			if err != nil {
				return "", err
			}
			if len(branches) == 0 {
				parts = append(parts, empty)
				continue
			}
			texts := make([]string, 0, len(branches))
			for _, branch := range branches {
				text, err := c.compile(branch)
				if err != nil {
					return "", err
				}
				texts = append(texts, "("+text+")")
			}
			parts = append(parts, "("+strings.Join(texts, separator)+")")
		default:
			if strings.HasPrefix(key, "$") {
				return "", fmt.Errorf("pg: unsupported top-level operator %q", key)
			}
			text, err := c.field(key, value)
			if err != nil {
				return "", err
			}
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, " AND "), nil
}

// compileWhere renders a filter as a WHERE expression and its parameters,
// numbered from start so the clause can follow parameters already bound
// earlier in the same statement.
func compileWhere(where db.M, promoted map[string]column, start int) (string, []any, error) {
	c := &compiler{promoted: promoted, start: start}
	text, err := c.compile(where)
	if err != nil {
		return "", nil, err
	}
	return text, c.params, nil
}

func operatorObject(condition any) (map[string]any, bool) {
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

func branchList(value any) ([]db.M, error) {
	switch list := value.(type) {
	case []db.M:
		return list, nil
	case []any:
		out := make([]db.M, 0, len(list))
		for _, item := range list {
			switch branch := item.(type) {
			case db.M:
				out = append(out, branch)
			case map[string]any:
				out = append(out, db.M(branch))
			default:
				return nil, fmt.Errorf("pg: $and/$or branch is %T, want a filter", item)
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("pg: $and/$or expects a list of filters, got %T", value)
}

func join(parts []string, separator string) string {
	if len(parts) == 1 {
		return parts[0]
	}
	return "(" + strings.Join(parts, separator) + ")"
}

// sortedKeys makes the generated SQL deterministic, which is what lets the
// compiler be tested by comparing strings — and what keeps a prepared
// statement cache from seeing a new query per map iteration order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
