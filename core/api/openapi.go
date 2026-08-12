//go:build !(js && wasm)

package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// OpenAPI
//
// Generated from the same types the handlers are written against, at startup,
// from the same table that was registered. There is no annotation to keep in
// sync and no second description of the API to drift: if the document is wrong,
// the endpoint is wrong.
//
// This is the part Go makes easier than TypeScript. zod schemas are runtime
// values that a generator has to walk and translate; here Q, B and R are real
// types, and reflect already knows their fields, their json tags and their
// query tags.
//
// What it deliberately cannot express: Validate(). "limit must be between 1 and
// 100" is arbitrary Go, and inventing a `maximum: 100` that the handler does
// not actually enforce would be worse than saying nothing.
// ---------------------------------------------------------------------------

// Info is the document's header.
type Info struct {
	Title       string
	Version     string
	Description string
}

// Document builds the OpenAPI 3.1 document for a set of routes.
func Document(info Info, routes []Route) map[string]any {
	if info.Title == "" {
		info.Title = "API"
	}
	if info.Version == "" {
		info.Version = "0.0.0"
	}

	schemas := map[string]any{}
	paths := map[string]any{}

	for _, rt := range routes {
		item, _ := paths[rt.Path].(map[string]any)
		if item == nil {
			item = map[string]any{}
			paths[rt.Path] = item
		}

		op := map[string]any{
			"operationId": operationID(rt),
			"summary":     rt.Name,
			"responses":   responses(rt, schemas),
		}
		if tag := tagFor(rt.Path); tag != "" {
			op["tags"] = []string{tag}
		}
		if params := parameters(rt); len(params) > 0 {
			op["parameters"] = params
		}
		if body := requestBody(rt, schemas); body != nil {
			op["requestBody"] = body
		}
		if len(rt.Roles) > 0 {
			// The scheme is nominal: howl-go does not know how an application
			// authenticates, only that this endpoint asked for roles. Saying
			// which ones is more useful than pretending to know the mechanism.
			op["security"] = []any{map[string]any{"roles": rt.Roles}}
			op["description"] = "Requires: " + strings.Join(rt.Roles, ", ")
		}
		item[strings.ToLower(rt.Method)] = op
	}

	doc := map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       info.Title,
			"version":     info.Version,
			"description": info.Description,
		},
		"paths": paths,
	}
	components := map[string]any{}
	if len(schemas) > 0 {
		components["schemas"] = schemas
	}
	if usesRoles(routes) {
		components["securitySchemes"] = map[string]any{
			"roles": map[string]any{"type": "apiKey", "in": "header", "name": "Authorization"},
		}
	}
	if len(components) > 0 {
		doc["components"] = components
	}
	return doc
}

// OpenAPI serves the document. Built once at startup, because the routes cannot
// change afterwards — and served rather than written to a file, so it can never
// be stale relative to the binary answering the requests.
func OpenAPI(info Info, routes ...Route) http.HandlerFunc {
	doc := Document(info, routes)
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		body = []byte(`{"error":"could not build the OpenAPI document"}`)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body) //nolint:errcheck
	}
}

// Docs serves a reader for the document at specURL.
//
// Hand-written rather than a vendored Redoc or Swagger UI: those are ~1 MB to
// render a list of endpoints, and this framework would then be shipping a
// megabyte of JavaScript to describe why it does not ship megabytes of
// JavaScript.
func Docs(specURL string) http.HandlerFunc {
	page := strings.ReplaceAll(docsHTML, "{{spec}}", specURL)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page)) //nolint:errcheck
	}
}

// ---------------------------------------------------------------------------

func operationID(rt Route) string {
	if rt.Name != "" {
		var b strings.Builder
		for _, f := range strings.FieldsFunc(rt.Name, func(r rune) bool { return r == ' ' || r == '-' || r == '_' }) {
			b.WriteString(strings.ToUpper(f[:1]) + f[1:])
		}
		return strings.ToLower(rt.Method) + b.String()
	}
	return strings.ToLower(rt.Method) + strings.ReplaceAll(rt.Path, "/", "_")
}

// tagFor groups endpoints by their first path segment after the prefix, which
// is the same thing their directory already says.
func tagFor(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) > 1 && parts[0] == "api" {
		return parts[1]
	}
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

func usesRoles(routes []Route) bool {
	for _, rt := range routes {
		if len(rt.Roles) > 0 {
			return true
		}
	}
	return false
}

// parameters covers both halves of the input that is not a body: the
// {placeholders} in the path, and the query struct's tagged fields.
func parameters(rt Route) []any {
	var out []any
	for _, seg := range strings.Split(rt.Path, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out = append(out, map[string]any{
				"name": seg[1 : len(seg)-1], "in": "path", "required": true,
				"schema": map[string]any{"type": "string"},
			})
		}
	}

	t := rt.schema.query
	if t == nil || t.Kind() != reflect.Struct || isNone(t) {
		return out
	}
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name := field.Tag.Get("query")
		if name == "-" {
			continue
		}
		if name == "" {
			name = strings.ToLower(field.Name)
		}
		param := map[string]any{"name": name, "in": "query", "schema": schemaFor(field.Type, nil)}
		if doc := field.Tag.Get("doc"); doc != "" {
			param["description"] = doc
		}
		out = append(out, param)
	}
	return out
}

func requestBody(rt Route, schemas map[string]any) any {
	t := rt.schema.body
	if t == nil || isNone(t) {
		return nil
	}
	return map[string]any{
		"required": true,
		"content":  map[string]any{"application/json": map[string]any{"schema": schemaFor(t, schemas)}},
	}
}

func responses(rt Route, schemas map[string]any) map[string]any {
	t := rt.schema.response
	if t == nil || isNone(t) {
		return map[string]any{"204": map[string]any{"description": "No content"}}
	}
	out := map[string]any{
		"200": map[string]any{
			"description": "OK",
			"content":     map[string]any{"application/json": map[string]any{"schema": schemaFor(t, schemas)}},
		},
	}
	// Every endpoint can fail the same way, and a caller generating from this
	// document should know the shape it will get when one does.
	errorSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"error":          map[string]any{"type": "string"},
			"field":          map[string]any{"type": "string"},
			"correlation_id": map[string]any{"type": "string"},
		},
	}
	out["400"] = map[string]any{"description": "Invalid input",
		"content": map[string]any{"application/json": map[string]any{"schema": errorSchema}}}
	if len(rt.Roles) > 0 {
		out["401"] = map[string]any{"description": "Unauthorized",
			"content": map[string]any{"application/json": map[string]any{"schema": errorSchema}}}
	}
	return out
}

var timeType = reflect.TypeOf(time.Time{})

// schemaFor walks a Go type into JSON Schema. Named structs become components
// and are referenced, so a type used by six endpoints is described once —
// which is also what stops a recursive type from recursing forever.
func schemaFor(t reflect.Type, schemas map[string]any) map[string]any {
	if t == nil {
		return map[string]any{}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return schemaFor(t.Elem(), schemas)
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string", "format": "byte"}
		}
		return map[string]any{"type": "array", "items": schemaFor(t.Elem(), schemas)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": schemaFor(t.Elem(), schemas)}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Interface:
		return map[string]any{} // any: no constraint is honest here
	case reflect.Struct:
		if t == timeType {
			return map[string]any{"type": "string", "format": "date-time"}
		}
		if schemas == nil || t.Name() == "" {
			return objectSchema(t, schemas)
		}
		name := t.Name()
		if _, done := schemas[name]; !done {
			schemas[name] = map[string]any{} // placeholder first: the type may reference itself
			schemas[name] = objectSchema(t, schemas)
		}
		return map[string]any{"$ref": "#/components/schemas/" + name}
	}
	return map[string]any{}
}

func objectSchema(t reflect.Type, schemas map[string]any) map[string]any {
	properties := map[string]any{}
	var required []string

	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		if field.Anonymous && field.Type.Kind() == reflect.Struct {
			// An embedded struct is flattened on the wire, so flatten it here.
			embedded := objectSchema(field.Type, schemas)
			for name, schema := range embedded["properties"].(map[string]any) {
				properties[name] = schema
			}
			continue
		}
		name, opts, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		properties[name] = schemaFor(field.Type, schemas)
		// A field without omitempty and without a pointer is always present in
		// the response, which is what "required" means to a client generator.
		if !strings.Contains(opts, "omitempty") && field.Type.Kind() != reflect.Pointer {
			required = append(required, name)
		}
	}

	out := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		sort.Strings(required)
		out["required"] = required
	}
	return out
}

func isNone(t reflect.Type) bool { return t == reflect.TypeOf(None{}) }

// docsHTML is a reader for the document: endpoints grouped by tag, each one
// expandable into its parameters, body and response schema. No dependency —
// the whole point of the layer is that the document is accurate, not that it
// is rendered by a megabyte of someone else's JavaScript.
const docsHTML = `<!doctype html>
<html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>API</title>
<style>
:root{--bg:#0f1115;--fg:#e6e6ea;--dim:#8b8b96;--line:#23262e;--card:#161920;
 --get:#4ade80;--post:#60a5fa;--put:#fbbf24;--patch:#c084fc;--delete:#fb7185}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.6 ui-sans-serif,system-ui,sans-serif}
header{padding:2rem 2rem 1rem;border-bottom:1px solid var(--line)}
h1{margin:0;font-size:1.4rem}
header p{margin:.4rem 0 0;color:var(--dim)}
main{padding:1.5rem 2rem 4rem;max-width:60rem}
h2{margin:2rem 0 .75rem;font-size:.75rem;text-transform:uppercase;letter-spacing:.16em;color:var(--dim)}
details{background:var(--card);border:1px solid var(--line);border-radius:10px;margin-bottom:.5rem}
summary{padding:.7rem 1rem;cursor:pointer;display:flex;gap:.75rem;align-items:center;list-style:none}
summary::-webkit-details-marker{display:none}
.m{font:600 11px ui-monospace,monospace;padding:.15rem .45rem;border-radius:5px;background:#0b0d11;min-width:3.6rem;text-align:center}
.get{color:var(--get)}.post{color:var(--post)}.put{color:var(--put)}.patch{color:var(--patch)}.delete{color:var(--delete)}
.path{font-family:ui-monospace,monospace}
.name{color:var(--dim);margin-left:auto;font-size:.85em}
.body{padding:0 1rem 1rem;border-top:1px solid var(--line)}
h3{font-size:.7rem;text-transform:uppercase;letter-spacing:.14em;color:var(--dim);margin:1rem 0 .4rem}
table{width:100%;border-collapse:collapse;font-size:.9em}
td{padding:.25rem .5rem .25rem 0;vertical-align:top}
td:first-child{font-family:ui-monospace,monospace;white-space:nowrap}
.t{color:var(--dim);font-family:ui-monospace,monospace}
pre{background:#0b0d11;border-radius:8px;padding:.75rem;overflow:auto;font-size:.85em;margin:0}
.roles{color:var(--put);font-size:.8em}
a{color:var(--post)}
</style></head><body>
<header><h1 id="title">API</h1><p id="sub"></p></header>
<main id="out">Loading <code>{{spec}}</code>…</main>
<script type="module">
const res = await fetch("{{spec}}");
const doc = await res.json();
document.title = doc.info.title;
title.textContent = doc.info.title;
sub.innerHTML = (doc.info.description || "") + ' <a href="{{spec}}">' + doc.info.version + " · openapi.json</a>";

const deref = (s) => {
  if (!s) return s;
  if (s.$ref) return deref(doc.components?.schemas?.[s.$ref.split("/").pop()]);
  return s;
};
// Render a schema as the JSON it describes: a shape you can read at a glance
// beats a table of types you have to reassemble in your head.
const example = (s, depth = 0) => {
  s = deref(s);
  if (!s || depth > 4) return null;
  if (s.type === "array") return [example(s.items, depth + 1)];
  if (s.type === "object" || s.properties) {
    const o = {};
    for (const [k, v] of Object.entries(s.properties || {})) o[k] = example(v, depth + 1);
    return s.properties ? o : {};
  }
  if (s.format === "date-time") return "2026-01-01T00:00:00Z";
  return { string: "string", integer: 0, number: 0, boolean: false }[s.type] ?? null;
};

const groups = {};
for (const [path, item] of Object.entries(doc.paths)) {
  for (const [method, op] of Object.entries(item)) {
    (groups[op.tags?.[0] ?? "api"] ??= []).push({ path, method, op });
  }
}
out.innerHTML = "";
for (const tag of Object.keys(groups).sort()) {
  const h = document.createElement("h2"); h.textContent = tag; out.append(h);
  for (const { path, method, op } of groups[tag].sort((a, b) => a.path.localeCompare(b.path))) {
    const d = document.createElement("details");
    const params = (op.parameters || []).map((p) =>
      "<tr><td>" + p.name + "</td><td class=t>" + (p.schema?.type ?? "") +
      (p.in === "path" ? " · path" : "") + "</td><td>" + (p.description ?? "") + "</td></tr>").join("");
    const body = op.requestBody?.content?.["application/json"]?.schema;
    const ok = op.responses?.["200"]?.content?.["application/json"]?.schema;
    d.innerHTML =
      "<summary><span class='m " + method + "'>" + method.toUpperCase() + "</span>" +
      "<span class=path>" + path + "</span><span class=name>" + (op.summary ?? "") + "</span></summary>" +
      "<div class=body>" +
      (op.description ? "<p class=roles>" + op.description + "</p>" : "") +
      (params ? "<h3>Parameters</h3><table>" + params + "</table>" : "") +
      (body ? "<h3>Request body</h3><pre>" + JSON.stringify(example(body), null, 2) + "</pre>" : "") +
      "<h3>Response " + (ok ? "200" : "204") + "</h3>" +
      (ok ? "<pre>" + JSON.stringify(example(ok), null, 2) + "</pre>" : "<p class=t>No content</p>") +
      "</div>";
    out.append(d);
  }
}
</script></body></html>`
