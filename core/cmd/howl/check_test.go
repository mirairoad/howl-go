package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// project writes a throwaway module from a map of relative paths to contents.
func project(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const goodShell = `package pages

templ App(title, head string) {
	<html>
		<head>
			<title>{ title }</title>
			@templ.JSONScript("howl-client", router.ClientConfig(ctx))
			<!--page-head-->
			@templ.Raw(head)
			<!--/page-head-->
		</head>
		<body>
			<main id="outlet" data-route={ router.Current(ctx) }>{ children... }</main>
		</body>
	</html>
}
`

func rules(result checkResult) map[string]Diagnostic {
	out := map[string]Diagnostic{}
	for _, d := range result.Diagnostics {
		out[d.Rule] = d
	}
	return out
}

func TestCleanProjectPasses(t *testing.T) {
	root := project(t, map[string]string{"client/pages/app.templ": goodShell})
	result := runCheck(root, false)
	if !result.OK || result.Warnings != 0 {
		t.Fatalf("clean project reported %#v", result.Diagnostics)
	}
}

// The layering rule the whole page tree depends on. Breaking it produces either
// an import cycle or net/http linked into the wasm build — neither of which
// says "a page imported core/app".
func TestPageImportingAppIsAnError(t *testing.T) {
	root := project(t, map[string]string{
		"client/pages/app.templ": goodShell,
		"client/pages/index.templ": `package pages

import "github.com/mirairoad/howl-go/core/app"

templ Page() { <p>{ app.Canonical("/") }</p> }
`,
	})
	found := rules(runCheck(root, false))
	d, ok := found["page-imports-app"]
	if !ok || d.Level != "error" {
		t.Fatalf("diagnostics = %#v", found)
	}
	if d.Line != 3 {
		t.Fatalf("line = %d, want 3", d.Line)
	}
}

func TestTemplMountIsAnError(t *testing.T) {
	root := project(t, map[string]string{
		"client/pages/app.templ":   goodShell,
		"client/pages/index.templ": "package pages\n\ntempl Mount() {\n\t<p>no</p>\n}\n",
	})
	if _, ok := rules(runCheck(root, false))["templ-mount"]; !ok {
		t.Fatal("`templ Mount()` was accepted; it cannot compile to anything useful")
	}
}

func TestShellContractIsChecked(t *testing.T) {
	root := project(t, map[string]string{
		"client/pages/app.templ": `package pages

templ App(title, head string) {
	<html><body><main>{ children... }</main></body></html>
}
`,
	})
	found := rules(runCheck(root, false))
	for _, rule := range []string{"shell-outlet", "shell-data-route", "shell-page-head", "shell-client-config"} {
		if _, ok := found[rule]; !ok {
			t.Errorf("%s not reported for a shell missing everything", rule)
		}
	}
}

// A shell that only mentions howl-client in prose is not publishing it. This
// was a real false negative: examples/hello passed while shipping the older
// config, because the comment explaining the migration contained the string.
func TestProseInACommentIsNotConfiguration(t *testing.T) {
	root := project(t, map[string]string{
		"client/pages/app.templ": `package pages

templ App(title, head string) {
	<html>
		<head>
			<!-- on main this is @templ.JSONScript("howl-client", router.ClientConfig(ctx)) -->
			@templ.JSONScript("howl-wasm-routes", router.NeedsWasm(router.Routes(ctx)))
			<!--page-head-->
			@templ.Raw(head)
			<!--/page-head-->
		</head>
		<body><main id="outlet" data-route={ router.Current(ctx) }>{ children... }</main></body>
	</html>
}
`,
	})
	d, ok := rules(runCheck(root, false))["shell-client-config"]
	if !ok {
		t.Fatal("a shell publishing the older config passed because a comment mentioned the new one")
	}
	if d.Level != "warning" {
		t.Fatalf("level = %s — the older form still works, so this is a migration warning", d.Level)
	}
}

// Declaring roles with nothing wired to interpret them lets every caller
// through. The framework panics at registration; this catches it before the
// program runs at all.
func TestRolesWithoutAuthorize(t *testing.T) {
	endpoint := `package apis

import "github.com/mirairoad/howl-go/core/api"

var Purge = api.Define(api.Spec[api.None, api.None, api.None]{
	Name:  "Purge",
	Roles: []string{"admin"},
	Handler: func(r *api.Request[api.None, api.None]) (api.None, error) { return api.None{}, nil },
})
`
	root := project(t, map[string]string{"server/apis/purge.post.api.go": endpoint})
	if _, ok := rules(runCheck(root, false))["roles-unwired"]; !ok {
		t.Fatal("roles with no Authorize anywhere in the module was accepted")
	}

	wired := project(t, map[string]string{
		"server/apis/purge.post.api.go": endpoint,
		"main.go": `package main

func main() {
	api.Register(mux, api.Config{Authorize: bearer(token)}, apis.FsApiRoutes()...)
}
`,
	})
	if _, ok := rules(runCheck(wired, false))["roles-unwired"]; ok {
		t.Fatal("roles reported as unwired even though Authorize is configured")
	}
}

// Declaring a typed query and then reading the raw one is legal Go that defeats
// the entire layer, so it is a warning rather than a silent difference in
// behaviour between what the endpoint documents and what it does.
func TestRawQueryInAnEndpoint(t *testing.T) {
	root := project(t, map[string]string{
		"server/apis/events.api.go": `package apis

var List = api.Define(api.Spec[Filter, api.None, []Event]{
	Name: "Events",
	Handler: func(r *api.Request[Filter, api.None]) ([]Event, error) {
		signal := r.HTTP.URL.Query().Get("signal")
		return find(signal)
	},
})
`,
	})
	if _, ok := rules(runCheck(root, false))["typed-query"]; !ok {
		t.Fatal("an endpoint bypassing its own declared query went unreported")
	}
}

func TestTwoEndpointsInOneFile(t *testing.T) {
	root := project(t, map[string]string{
		"server/apis/pair.api.go": `package apis

var One = api.Define(api.Spec[api.None, api.None, api.None]{Name: "One"})
var Two = api.Define(api.Spec[api.None, api.None, api.None]{Name: "Two"})
`,
	})
	if _, ok := rules(runCheck(root, false))["one-endpoint-per-file"]; !ok {
		t.Fatal("two endpoints in one file were accepted; the file name can only describe one URL")
	}
}

// A field with a query tag is an input, not a wire shape. Flagging it taught
// the rule to cry wolf on every endpoint that takes a filter.
func TestQueryTagsAreNotMissingJSONTags(t *testing.T) {
	root := project(t, map[string]string{
		"server/apis/series.api.go": "package apis\n\ntype Query struct {\n\tName  string `query:\"name\"`\n\tLimit int    `query:\"limit\"`\n}\n",
	})
	for _, d := range runCheck(root, false).Diagnostics {
		if d.Rule == "json-tag" {
			t.Fatalf("query fields reported as missing json tags: %#v", d)
		}
	}
}

func TestGeneratedFileWithoutItsHeader(t *testing.T) {
	root := project(t, map[string]string{
		"client/pages/fsroutes_gen.go": "package pages\n\nfunc FsClientRoutes() []router.Route { return nil }\n",
	})
	if _, ok := rules(runCheck(root, false))["edited-generated"]; !ok {
		t.Fatal("a generated file that lost its header went unreported")
	}
}

// ---------------------------------------------------------------------------
// The MCP server
// ---------------------------------------------------------------------------

func exchange(t *testing.T, root string, requests ...string) []map[string]any {
	t.Helper()
	in := strings.NewReader(strings.Join(requests, "\n") + "\n")
	var out bytes.Buffer
	if err := serveMCP(in, &out, root); err != nil {
		t.Fatal(err)
	}
	var responses []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("response is not JSON: %q", line)
		}
		responses = append(responses, v)
	}
	return responses
}

func TestMCPHandshake(t *testing.T) {
	root := project(t, map[string]string{"client/pages/app.templ": goodShell})
	responses := exchange(t, root,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	)

	// Two requests, one notification: a notification must not be answered, and
	// some clients treat a reply to one as fatal.
	if len(responses) != 2 {
		t.Fatalf("got %d responses for 2 requests and 1 notification", len(responses))
	}
	result := responses[0]["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocolVersion = %v — the client's version should be echoed", result["protocolVersion"])
	}
	if _, ok := result["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatal("tools capability not advertised")
	}

	tools := responses[1]["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"howl_conventions", "howl_check", "howl_routes", "howl_endpoints", "howl_scaffold"} {
		if !names[want] {
			t.Errorf("tool %s missing from tools/list", want)
		}
	}
}

func TestMCPCheckToolReturnsDiagnostics(t *testing.T) {
	root := project(t, map[string]string{
		"client/pages/app.templ":   goodShell,
		"client/pages/index.templ": "package pages\n\ntempl Mount() {\n\t<p>no</p>\n}\n",
	})
	responses := exchange(t, root,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"howl_check","arguments":{"build":false}}}`,
	)
	content := responses[0]["result"].(map[string]any)["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "templ-mount") {
		t.Fatalf("check tool did not report the rule: %s", text)
	}
}

func TestMCPConventionsServesTheFile(t *testing.T) {
	responses := exchange(t, t.TempDir(),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"howl_conventions","arguments":{}}}`,
	)
	text := responses[0]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "howl-go") || !strings.Contains(text, "Routing conventions") {
		t.Fatalf("conventions tool returned %d bytes that do not look like llms.txt", len(text))
	}
}

func TestMCPUnknownToolIsAResultNotATransportError(t *testing.T) {
	responses := exchange(t, t.TempDir(),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"howl_nope","arguments":{}}}`,
	)
	result := responses[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("result = %v — a failing tool must come back as an error result the model can read", result)
	}
	if _, isTransportError := responses[0]["error"]; isTransportError {
		t.Fatal("a bad tool name was reported as a JSON-RPC error")
	}
}

func TestMCPSurvivesAMalformedLine(t *testing.T) {
	responses := exchange(t, t.TempDir(),
		`not json at all`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
	)
	if len(responses) != 2 {
		t.Fatalf("got %d responses; the session died on a bad line", len(responses))
	}
	if responses[0]["error"].(map[string]any)["code"].(float64) != -32700 {
		t.Fatalf("first response = %v, want a parse error", responses[0])
	}
}

// ---------------------------------------------------------------------------
// Scaffolding
// ---------------------------------------------------------------------------

func TestScaffoldPageNaming(t *testing.T) {
	root := t.TempDir()
	if _, err := scaffold(root, request{Kind: "page", Path: "/blog/{article_id}", Client: true}); err != nil {
		t.Fatal(err)
	}
	// Brackets are impossible in a Go file name, so the parameter becomes the
	// stem with .dyn — and .client because the browser renders it too.
	want := filepath.Join(root, "client/pages/blog/article_id.dyn.client.templ")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected %s; got %s", want, tree(t, root))
	}
}

func TestScaffoldEndpointNaming(t *testing.T) {
	root := t.TempDir()
	if _, err := scaffold(root, request{Kind: "endpoint", Path: "/api/settings/purge", Name: "Purge", Method: "POST", Roles: []string{"admin"}}); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "server/apis/settings/purge.post.api.go")
	body, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("expected %s; got %s", want, tree(t, root))
	}
	for _, needle := range []string{"package settings", `Roles: []string{"admin"}`, "api.Define(api.Spec["} {
		if !strings.Contains(string(body), needle) {
			t.Errorf("scaffolded endpoint is missing %q:\n%s", needle, body)
		}
	}
}

func TestScaffoldRefusesToOverwrite(t *testing.T) {
	root := t.TempDir()
	if _, err := scaffold(root, request{Kind: "page", Path: "/reports"}); err != nil {
		t.Fatal(err)
	}
	if _, err := scaffold(root, request{Kind: "page", Path: "/reports"}); err == nil {
		t.Fatal("scaffold overwrote an existing file")
	}
}

func tree(t *testing.T, root string) string {
	t.Helper()
	var out []string
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error { //nolint:errcheck
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, rel)
		}
		return nil
	})
	return strings.Join(out, ", ")
}

// A page importing db is the same mistake as importing core/app, and it fails
// the same two ways: the page tree stops being leaves, and database/sql lands
// in the wasm build.
func TestPageImportingDBIsAnError(t *testing.T) {
	root := project(t, map[string]string{
		"client/pages/app.templ": goodShell,
		"client/pages/reports/index.templ": `package reports

import "github.com/mirairoad/howl-go/db"

templ Reports() {
	<h1>{ db.IDPath }</h1>
}
`,
	})
	found := rules(runCheck(root, false))
	d, ok := found["page-imports-db"]
	if !ok {
		t.Fatalf("page-imports-db not reported: %+v", found)
	}
	if d.Level != "error" || d.Line == 0 {
		t.Errorf("diagnostic = %+v", d)
	}
}

// Defaults on a value receiver satisfies the interface, so the service calls
// it — and every default is written to a copy. Nothing fails; the field is
// just empty forever, which is exactly the class of bug this command exists
// for.
func TestCollectionValueReceiverDefaultsIsAnError(t *testing.T) {
	root := project(t, map[string]string{
		"client/pages/app.templ": goodShell,
		"server/store/users.go": `package store

import "github.com/mirairoad/howl-go/db"

type User struct {
	db.Doc
	Plan string ` + "`json:\"plan\"`" + `
}

func (u User) Defaults() {
	if u.Plan == "" {
		u.Plan = "free"
	}
}
`,
	})
	found := rules(runCheck(root, false))
	d, ok := found["collection-value-receiver"]
	if !ok {
		t.Fatalf("collection-value-receiver not reported: %+v", found)
	}
	if d.Level != "error" {
		t.Errorf("level = %q, want error", d.Level)
	}
	if !strings.Contains(d.Fix, "*User") {
		t.Errorf("fix = %q, want the pointer receiver", d.Fix)
	}
}

func TestPointerReceiverDefaultsPasses(t *testing.T) {
	root := project(t, map[string]string{
		"client/pages/app.templ": goodShell,
		"server/store/users.go": `package store

import "github.com/mirairoad/howl-go/db"

type User struct {
	db.Doc
	Plan string ` + "`json:\"plan\"`" + `
}

func (u *User) Defaults() { u.Plan = "free" }
`,
	})
	if _, reported := rules(runCheck(root, false))["collection-value-receiver"]; reported {
		t.Error("a pointer receiver was reported")
	}
}

// A value-receiver Defaults on a struct that is not a document is somebody
// else's business.
func TestValueReceiverOnANonDocumentIsIgnored(t *testing.T) {
	root := project(t, map[string]string{
		"client/pages/app.templ": goodShell,
		"server/store/config.go": `package store

import "github.com/mirairoad/howl-go/db"

type Settings struct {
	Theme string
}

func (s Settings) Defaults() { s.Theme = "dark" }

type User struct {
	db.Doc
}
`,
	})
	if _, reported := rules(runCheck(root, false))["collection-value-receiver"]; reported {
		t.Error("a non-document struct was reported")
	}
}

// ---------------------------------------------------------------------------
// Reactivity and lifecycle
// ---------------------------------------------------------------------------

// The leak the docs warn about, made into a rule. A Mount that subscribes and
// never releases keeps working — it just also keeps firing at the DOM of every
// page that has since been replaced, one more listener per visit.
func TestMountThatRegistersNeedsUnmount(t *testing.T) {
	root := project(t, map[string]string{
		"client/pages/app.templ": goodShell,
		"client/pages/todos/index.client.templ": `package todos

templ Page() {
	<ul id="list"></ul>
}

var stop func()

func Mount() {
	stop = signal.Effect(repaint)
	dom.Root().Query("[data-add]").On("click", add)
}
`,
	})
	got := rules(runCheck(root, false))
	d, ok := got["mount-without-unmount"]
	if !ok {
		t.Fatalf("no diagnostic; got %#v", got)
	}
	if d.Level != "error" {
		t.Errorf("level = %q, want error", d.Level)
	}
}

// Mount that only reads is fine. The metrics page in the toy app logs a row
// count and fetches once; demanding an Unmount there would be ceremony, and a
// rule that fires on correct code is a rule people turn off.
func TestMountThatRegistersNothingIsFine(t *testing.T) {
	root := project(t, map[string]string{
		"client/pages/app.templ": goodShell,
		"client/pages/metrics/index.client.templ": `package metrics

templ Page() {
	<table></table>
}

func Mount() {
	dom.Log("rows:", len(dom.Root().QueryAll("tr")))
}
`,
	})
	if result := runCheck(root, false); !result.OK || result.Warnings != 0 {
		t.Fatalf("reported %#v", result.Diagnostics)
	}
}

// A stop func that is never bound cannot be called, so the subscription it
// represents outlives the page by definition.
func TestDiscardedEffectIsAnError(t *testing.T) {
	root := project(t, map[string]string{
		"client/pages/app.templ": goodShell,
		"client/pages/x/index.client.templ": `package x

templ Page() {
	<div></div>
}

func Mount() {
	signal.Effect(repaint)
}

func Unmount() {}
`,
	})
	if _, ok := rules(runCheck(root, false))["effect-not-released"]; !ok {
		t.Fatal("a discarded stop func was accepted")
	}
}

// Signals are package-level. A handler writing one is not a slow path or a
// stale read — it is two requests writing one variable.
func TestServerImportingSignalIsAnError(t *testing.T) {
	root := project(t, map[string]string{
		"client/pages/app.templ": goodShell,
		"server/apis/metrics.api.go": `package apis

import "github.com/mirairoad/howl-go/core/signal"

var Live = signal.Of(0)
`,
	})
	if _, ok := rules(runCheck(root, false))["server-imports-signal"]; !ok {
		t.Fatal("server-side signal write was accepted")
	}
}

// The store is imported by pages, and pages compile for wasm. database/sql in
// here surfaces as a link error naming a package the author never wrote down.
func TestStoreMustCompileForWasm(t *testing.T) {
	root := project(t, map[string]string{
		"client/pages/app.templ": goodShell,
		"client/store/todos.go": `package store

import "database/sql"

var db *sql.DB
`,
	})
	if _, ok := rules(runCheck(root, false))["store-not-portable"]; !ok {
		t.Fatal("a server-only import in the store was accepted")
	}
}

func TestClientTemplateCannotReadProcessRuntime(t *testing.T) {
	root := project(t, map[string]string{
		"go.mod":                 "module example.com/app\n\ngo 1.25\n",
		"client/pages/app.templ": goodShell,
		"client/pages/dashboard/index.client.templ": `package dashboard

import "runtime"

templ Page() { <span>{ runtime.Version() }</span> }
`,
	})
	d, ok := rules(runCheck(root, false))["client-runtime-global"]
	if !ok || d.Level != "error" {
		t.Fatalf("runtime global was accepted: %#v", d)
	}
}

func TestClientSafetyTraversesLayoutAndSharedComponents(t *testing.T) {
	root := project(t, map[string]string{
		"go.mod":                 "module example.com/app\n\ngo 1.25\n",
		"client/pages/app.templ": goodShell,
		"client/pages/dashboard/layout.templ": `package dashboard

import "example.com/app/client/ui"

templ Layout() { @ui.Footer() { children... } }
`,
		"client/pages/dashboard/index.client.templ": `package dashboard

templ Page() { <p>ok</p> }
`,
		"client/ui/footer.templ": `package ui

import "example.com/app/internal/build"

templ Footer() { <span>{ build.Tag() }</span> }
`,
		"internal/build/build.go": `package build

func Tag() string { return "dev" }
`,
	})
	d, ok := rules(runCheck(root, false))["client-imports-server-package"]
	if !ok || d.Level != "error" {
		t.Fatalf("transitive server state import was accepted: %#v", d)
	}
}

func TestHowlServerPackageCannotReachClientRoute(t *testing.T) {
	root := project(t, map[string]string{
		"go.mod":                 "module example.com/app\n\ngo 1.25\n",
		"client/pages/app.templ": goodShell,
		"client/pages/index.client.templ": `package pages

import "example.com/app/internal/meta"

templ Page() { <span>{ meta.Value() }</span> }
`,
		"internal/meta/meta.go": `//howl:server
package meta

func Value() string { return "server" }
`,
	})
	if _, ok := rules(runCheck(root, false))["client-imports-server-package"]; !ok {
		t.Fatal("a //howl:server package reached a client route")
	}
}

// ---------------------------------------------------------------------------
// Component references
// ---------------------------------------------------------------------------

const componentsPkg = `package components

templ SettingsShell(title string) {
	<div>{ title }</div>
}

templ card(label string) {
	<article>{ label }</article>
}
`

func componentProject(t *testing.T, page string) string {
	t.Helper()
	return project(t, map[string]string{
		"go.mod":                            "module example.com/app\n\ngo 1.25\n",
		"client/pages/app.templ":            goodShell,
		"client/components/shell.templ":     componentsPkg,
		"client/pages/settings/index.templ": page,
	})
}

// The error this framework reports worst. templ emits no //line directives, so
// the compiler names a generated file at a line that exists in no source —
// after two generators have run. Here it is the line that was typed.
func TestUnknownComponentIsFoundBeforeTheCompiler(t *testing.T) {
	root := componentProject(t, `package settings

import "example.com/app/client/components"

templ Page() {
	@components.SettingsShel("Settings")
}
`)
	d, ok := rules(runCheck(root, false))["unknown-component"]
	if !ok {
		t.Fatal("an undefined component reference was accepted")
	}
	if d.Line != 6 {
		t.Errorf("line = %d, want 6 — the point of this rule is the line", d.Line)
	}
	// A typo and a wrong package look identical to the compiler.
	if !strings.Contains(d.Fix, "SettingsShell") {
		t.Errorf("no suggestion: %q", d.Fix)
	}
}

func TestComponentArityIsChecked(t *testing.T) {
	root := componentProject(t, `package settings

import "example.com/app/client/components"

templ Page() {
	@components.SettingsShell("Settings", "extra")
}
`)
	if _, ok := rules(runCheck(root, false))["component-arity"]; !ok {
		t.Fatal("a call with the wrong number of arguments was accepted")
	}
}

// Go's export rule is the case of the first letter, and templ inherits it —
// which is not obvious when both files are in the same client/ tree.
func TestUnexportedComponentAcrossPackages(t *testing.T) {
	root := componentProject(t, `package settings

import "example.com/app/client/components"

templ Page() {
	@components.card("nope")
}
`)
	if _, ok := rules(runCheck(root, false))["unexported-component"]; !ok {
		t.Fatal("a lowercase component was accepted from another package")
	}
}

// An unexported component used inside its own package is the normal case — a
// layout's nav link, a row helper. A rule that fires here is a rule that gets
// switched off.
func TestUnexportedComponentInItsOwnPackageIsFine(t *testing.T) {
	root := componentProject(t, `package settings

templ Page() {
	@row("a")
	@row("b")
}

templ row(label string) {
	<li>{ label }</li>
}
`)
	if result := runCheck(root, false); !result.OK {
		t.Fatalf("reported %#v", result.Diagnostics)
	}
}

// Arguments contain commas: struct literals, JSON blobs, func literals. A rule
// that counts them naively reports working code.
func TestArgumentCountingSurvivesCommas(t *testing.T) {
	root := project(t, map[string]string{
		"go.mod":                 "module example.com/app\n\ngo 1.25\n",
		"client/pages/app.templ": goodShell,
		"client/ui/ui.templ": `package ui

templ Island(name, props string) {
	<div data-island={ name }>{ children... }</div>
}

templ Rows(items []Row, fn func(Row) string) {
	<tbody></tbody>
}
`,
		"client/pages/index.templ": `package pages

import "example.com/app/client/ui"

templ Page() {
	@ui.Island("counter", ` + "`" + `{"start":10,"label":"a, b"}` + "`" + `)
	@ui.Rows([]ui.Row{{Name: "a", Value: 1}}, func(r ui.Row) string { return r.Name })
}
`,
	})
	if result := runCheck(root, false); !result.OK {
		t.Fatalf("commas inside arguments were counted as arguments: %#v", result.Diagnostics)
	}
}

// A local variable holding a component reads exactly like a qualified call.
// It cannot be resolved from here, so it must not be reported.
func TestMethodCallOnAValueIsNotAComponentReference(t *testing.T) {
	root := componentProject(t, `package settings

templ Page() {
	{{ r := current(ctx) }}
	@r.Component()
}
`)
	if result := runCheck(root, false); !result.OK {
		t.Fatalf("a method call was read as a package reference: %#v", result.Diagnostics)
	}
}

// ---------------------------------------------------------------------------
// Cost in the wrong place
// ---------------------------------------------------------------------------

// The lifecycle code that matters lives in .templ files, beside markup that is
// not Go. These rules only reach it because the templ blocks are blanked and
// the rest is parsed — at the original line numbers.
func TestPerformanceRulesReachTemplFiles(t *testing.T) {
	root := project(t, map[string]string{
		"go.mod":                 "module example.com/app\n\ngo 1.25\n",
		"client/pages/app.templ": goodShell,
		"client/pages/dash/index.client.templ": `package dash

import (
	"net/http"
	"regexp"
)

templ Page() {
	<ul data-list>
		for _, r := range rows() {
			<li>{ r }</li>
		}
	</ul>
}

func Mount() {}

func Unmount() {}

func repaint(names []string) {
	html := ""
	for _, name := range names {
		resp, _ := http.Get("/api/row")
		defer resp.Body.Close()
		html += regexp.MustCompile("[*]").ReplaceAllString(name, "")
	}
	_ = html
}
`,
	})
	got := rules(runCheck(root, false))
	for _, rule := range []string{"request-in-loop", "defer-in-loop", "concat-in-loop", "regexp-recompiled"} {
		d, ok := got[rule]
		if !ok {
			t.Errorf("%s did not fire", rule)
			continue
		}
		if d.Level != "warning" {
			t.Errorf("%s is %s; a judgement call that fails a build is a rule people delete", rule, d.Level)
		}
		// The templ block above is 8 lines long. A line number that ignored it
		// would point into the markup.
		if d.Line < 20 {
			t.Errorf("%s reported line %d; the templ block was not accounted for", rule, d.Line)
		}
	}
}

// An accumulator declared inside the loop is one short string per iteration,
// not a quadratic append. Reporting it would make the rule noise.
func TestConcatDeclaredInsideTheLoopIsFine(t *testing.T) {
	root := project(t, map[string]string{
		"go.mod":                 "module example.com/app\n\ngo 1.25\n",
		"client/pages/app.templ": goodShell,
		"client/pages/index.templ": `package pages

templ Page() {
	<div></div>
}

func describe(items []string) []string {
	var out []string
	for _, item := range items {
		extra := ""
		if item != "" {
			extra += " +set"
		}
		out = append(out, item+extra)
	}
	return out
}
`,
	})
	if result := runCheck(root, false); !result.OK || result.Warnings != 0 {
		t.Fatalf("reported %#v", result.Diagnostics)
	}
}

// A table-driven test is a loop of deliberate single queries, and a test helper
// is not a hot path.
func TestPerformanceRulesSkipTests(t *testing.T) {
	root := project(t, map[string]string{
		"go.mod":                 "module example.com/app\n\ngo 1.25\n",
		"client/pages/app.templ": goodShell,
		// Not named _test.go on purpose: a conformance suite is ordinary code
		// that happens to take a *testing.T.
		"server/store/conformance.go": `package store

import (
	"context"
	"testing"

	"github.com/mirairoad/howl-go/db"
)

func Run(t *testing.T, ctx context.Context, s *db.Service[User, *User]) {
	for _, name := range []string{"a", "b"} {
		if _, err := s.Find(ctx, db.Query{Where: db.Eq("name", name)}); err != nil {
			t.Fatal(err)
		}
	}
}
`,
	})
	if _, ok := rules(runCheck(root, false))["query-in-loop"]; ok {
		t.Fatal("a table-driven test was reported as an N+1")
	}
}

// A pattern built from a value cannot be hoisted, so telling someone to hoist
// it is advice they cannot take.
func TestDynamicRegexpOutsideALoopIsFine(t *testing.T) {
	root := project(t, map[string]string{
		"go.mod":                 "module example.com/app\n\ngo 1.25\n",
		"client/pages/app.templ": goodShell,
		"client/pages/index.templ": `package pages

import "regexp"

templ Page() {
	<div></div>
}

func find(name, body string) bool {
	return regexp.MustCompile("^" + regexp.QuoteMeta(name) + "$").MatchString(body)
}
`,
	})
	if result := runCheck(root, false); result.Warnings != 0 {
		t.Fatalf("reported %#v", result.Diagnostics)
	}
}

// The N+1 the filter grammar exists to avoid.
func TestQueryInLoopIsReported(t *testing.T) {
	root := project(t, map[string]string{
		"go.mod":                 "module example.com/app\n\ngo 1.25\n",
		"client/pages/app.templ": goodShell,
		"server/apis/users.api.go": `package apis

import (
	"context"

	"github.com/mirairoad/howl-go/db"
)

func load(ctx context.Context, s *db.Service[User, *User], ids []string) []User {
	var out []User
	for _, id := range ids {
		u, err := s.Get(ctx, id)
		if err == nil {
			out = append(out, u)
		}
	}
	return out
}
`,
	})
	if _, ok := rules(runCheck(root, false))["query-in-loop"]; !ok {
		t.Fatal("an N+1 was accepted")
	}
}
