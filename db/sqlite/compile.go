package sqlite

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/mirairoad/howl-go/db"
)

// Type is the storage type a promoted column carries, shared with the other
// SQL backends so an options literal moves between them unchanged. SQLite has
// affinities rather than types; these map onto them.
type Type = db.PromoteType

// The promotable types, re-exported so sqlite.Bigint reads naturally beside
// sqlite.Options.
const (
	Text    = db.Text
	Bigint  = db.Bigint
	Numeric = db.Numeric
	Boolean = db.Boolean
)

// affinity maps a shared promote type onto the SQLite column affinity that
// makes comparisons behave. SQLite has no bigint or boolean; both are
// INTEGER, which is what JSON numbers and JSON booleans extract as anyway.
func affinity(t Type) string {
	switch t {
	case db.Bigint, db.Boolean:
		return "INTEGER"
	case db.Numeric:
		return "NUMERIC"
	default:
		return "TEXT"
	}
}

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
		return "", fmt.Errorf("sqlite: invalid identifier %q", segment)
	}
	return segment, nil
}

// jsonPath renders a document path as the JSONPath literal SQLite's json
// functions take: '$.meta.deleted_at'.
func jsonPath(segments []string) (string, error) {
	for _, segment := range segments {
		if _, err := assertIdent(segment); err != nil {
			return "", err
		}
	}
	return "'$." + strings.Join(segments, ".") + "'", nil
}

// compiler turns the neutral filter grammar into a parametrized WHERE clause.
//
// Routing is the same as the Postgres compiler — id hits the primary key, a
// promoted path hits its generated column, everything else reads the document
// — but the dialect is simpler in two ways that remove whole cases:
//
//   - `->>` returns natively typed values, so a numeric comparison needs no
//     cast and the parameter binds as itself.
//   - SQLite maps a JSON null AND an absent key both to SQL NULL, which is
//     exactly Mongo's null-equality rule. Postgres needs that spelled out; here
//     `IS NULL` is the whole story.
//
// What it loses is $exists, which can no longer be asked of `->>` for the same
// reason — json_type() is the form that separates a stored null from a missing
// key.
type compiler struct {
	promoted map[string]column
	params   []any
}

type target struct {
	expr     string
	segments []string
}

// push binds a parameter. SQLite has no boolean storage class and JSON
// booleans extract as 0/1, so the parameter side is converted to match rather
// than relying on the driver to guess.
func (c *compiler) push(value any) string {
	if b, ok := value.(bool); ok {
		value = map[bool]int{true: 1, false: 0}[b]
	}
	c.params = append(c.params, value)
	return "?"
}

func (c *compiler) target(path string) (target, error) {
	if path == "id" {
		return target{expr: "id", segments: []string{"id"}}, nil
	}
	segments := strings.Split(path, ".")
	for _, segment := range segments {
		if _, err := assertIdent(segment); err != nil {
			return target{}, err
		}
	}
	if promoted, ok := c.promoted[path]; ok {
		return target{expr: `"` + promoted.name + `"`, segments: segments}, nil
	}
	literal, err := jsonPath(segments)
	if err != nil {
		return target{}, err
	}
	return target{expr: "doc->>" + literal, segments: segments}, nil
}

func (c *compiler) field(path string, condition any) (string, error) {
	t, err := c.target(path)
	if err != nil {
		return "", err
	}
	ops, isOps := operatorObject(condition)
	if !isOps {
		return c.equality(t, condition)
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

func (c *compiler) equality(t target, value any) (string, error) {
	if value == nil {
		return t.expr + " IS NULL", nil
	}
	// A composite operand is compared as JSON: `->` renders the stored value
	// the same way json() minifies the parameter, so the two are comparable
	// text. Key order matters, which is Mongo's rule for the same reason.
	if composite(value) {
		literal, err := jsonPath(t.segments)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		return "doc->" + literal + " = json(" + c.push(string(encoded)) + ")", nil
	}
	return t.expr + " = " + c.push(value), nil
}

func (c *compiler) operator(t target, op string, operand any) (string, error) {
	switch op {
	case db.OpEq:
		return c.equality(t, operand)

	case db.OpNe:
		if operand == nil {
			return t.expr + " IS NOT NULL", nil
		}
		// IS NOT rather than <>: a document without the field must match $ne,
		// and <> against SQL NULL is NULL, which does not.
		return t.expr + " IS NOT " + c.push(operand), nil

	case db.OpIn, db.OpNin:
		return c.membership(t, op, operand)

	case db.OpGt, db.OpGte, db.OpLt, db.OpLte:
		return t.expr + " " + comparison[op] + " " + c.push(operand), nil

	case db.OpExists:
		// `->>` maps a stored null and an absent key both to SQL NULL, so it
		// cannot answer this one. json_type() can.
		literal, err := jsonPath(t.segments)
		if err != nil {
			return "", err
		}
		if want, _ := operand.(bool); want {
			return "json_type(doc, " + literal + ") IS NOT NULL", nil
		}
		return "json_type(doc, " + literal + ") IS NULL", nil
	}
	return "", fmt.Errorf("sqlite: unsupported filter operator %q — the grammar is "+
		"$eq $ne $in $nin $gt $gte $lt $lte $or $and $exists; anything richer belongs behind SQL()", op)
}

var comparison = map[string]string{db.OpGt: ">", db.OpGte: ">=", db.OpLt: "<", db.OpLte: "<="}

func (c *compiler) membership(t target, op string, operand any) (string, error) {
	values, ok := operand.([]any)
	if !ok {
		return "", fmt.Errorf("sqlite: %s expects a list, got %T", op, operand)
	}
	bound := make([]string, 0, len(values))
	hasNull := false
	for _, v := range values {
		if v == nil {
			hasNull = true
			continue
		}
		if composite(v) {
			return "", fmt.Errorf("sqlite: %s takes scalars only — an IN list of documents "+
				"has no indexable form here; use SQL() for that", op)
		}
		bound = append(bound, c.push(v))
	}

	var parts []string
	if op == db.OpIn {
		if len(bound) > 0 {
			parts = append(parts, t.expr+" IN ("+strings.Join(bound, ", ")+")")
		}
		if hasNull {
			parts = append(parts, t.expr+" IS NULL")
		}
		if len(parts) == 0 {
			return "0", nil
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
		parts = append(parts, t.expr+" IS NOT NULL")
	}
	if len(parts) == 0 {
		return "1", nil
	}
	return join(parts, " AND "), nil
}

func (c *compiler) compile(where db.M) (string, error) {
	if len(where) == 0 {
		return "1", nil
	}
	parts := make([]string, 0, len(where))
	for _, key := range sortedKeys(where) {
		value := where[key]
		switch key {
		case db.OpAnd, db.OpOr:
			separator, empty := " AND ", "1"
			if key == db.OpOr {
				separator, empty = " OR ", "0"
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
				return "", fmt.Errorf("sqlite: unsupported top-level operator %q", key)
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

// compileWhere renders a filter as a WHERE expression and its parameters.
// SQLite placeholders are positional `?`, so unlike the Postgres compiler
// there is no starting offset — the caller just puts its own parameters
// first.
func compileWhere(where db.M, promoted map[string]column) (string, []any, error) {
	c := &compiler{promoted: promoted}
	text, err := c.compile(where)
	if err != nil {
		return "", nil, err
	}
	return text, c.params, nil
}

// composite reports whether a value encodes as a JSON object or array, which
// is what separates a scalar comparison from a whole-value one.
func composite(value any) bool {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) == 0 {
		return false
	}
	return encoded[0] == '{' || encoded[0] == '['
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
				return nil, fmt.Errorf("sqlite: $and/$or branch is %T, want a filter", item)
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("sqlite: $and/$or expects a list of filters, got %T", value)
}

func join(parts []string, separator string) string {
	if len(parts) == 1 {
		return parts[0]
	}
	return "(" + strings.Join(parts, separator) + ")"
}

// sortedKeys makes the generated SQL deterministic, which is what lets the
// compiler be tested by comparing strings — and what keeps SQLite's prepared
// statement cache from seeing a new query per map iteration order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
