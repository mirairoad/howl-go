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
	if _, err := scaffold(root, "page", "/blog/{article_id}", "", "", true, nil); err != nil {
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
	if _, err := scaffold(root, "endpoint", "/api/settings/purge", "Purge", "POST", false, []string{"admin"}); err != nil {
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
	if _, err := scaffold(root, "page", "/reports", "", "", false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := scaffold(root, "page", "/reports", "", "", false, nil); err == nil {
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
