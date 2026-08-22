package main

import (
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The scaffold's whole justification is that it writes something that
// compiles, so the test parses what it wrote rather than matching strings.
func TestScaffoldCollection(t *testing.T) {
	root := t.TempDir()
	out, err := scaffold(root, request{Kind: "collection", Name: "users",
		Fields: []string{"email:string", "seats:int64"}})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	file := filepath.Join(root, "server", "store", "users.go")
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading the scaffold: %v", err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), file, body, parser.AllErrors); err != nil {
		t.Fatalf("the scaffold does not parse: %v\n%s", err, body)
	}

	formatted, err := format.Source(body)
	if err != nil || string(formatted) != string(body) {
		t.Errorf("the scaffold is not gofmt-clean; it would land as the one file in the tree gofmt wants to rewrite")
	}

	source := string(body)
	for _, want := range []string{
		"type User struct {",
		"\tdb.Doc\n",
		"Email string `json:\"email\"`",
		"Seats int64  `json:\"seats\"`", // gofmt aligns the tags
		"func (u *User) Defaults()",     // pointer, or the defaults are lost
		"func (u *User) Validate() error",
		"var Users *pg.Service[User, *User]",
		`Collection: "users"`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("scaffold is missing %q\n%s", want, source)
		}
	}
	if !strings.Contains(out, "server/store/users.go") {
		t.Errorf("report = %q", out)
	}
}

// The type name comes off the table name, and the file is meant to be edited —
// so this only has to be readable, not linguistically right.
func TestScaffoldCollectionNames(t *testing.T) {
	for table, want := range map[string]string{
		"users":      "type User struct {",
		"properties": "type Property struct {",
		"audit":      "type Audit struct {",
	} {
		root := t.TempDir()
		if _, err := scaffold(root, request{Kind: "collection", Name: table}); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		body, err := os.ReadFile(filepath.Join(root, "server", "store", table+".go"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), want) {
			t.Errorf("%s did not produce %q", table, want)
		}
	}
}

func TestScaffoldCollectionRefusesAURL(t *testing.T) {
	root := t.TempDir()
	if _, err := scaffold(root, request{Kind: "collection", Name: "/api/users/{id}"}); err == nil {
		t.Error("a URL was accepted as a collection name")
	}
	if _, err := scaffold(root, request{Kind: "collection"}); err == nil {
		t.Error("an empty collection name was accepted")
	}
}

func TestScaffoldRejectsAnUnknownKind(t *testing.T) {
	if _, err := scaffold(t.TempDir(), request{Kind: "widget", Path: "/x"}); err == nil {
		t.Error("an unknown kind was accepted")
	}
}

// The store scaffold exists because this shape is the one thing in the
// framework that cannot be derived from the API list: which half compiles for
// wasm, which half is package-level, and why publish is gated.
func TestScaffoldStore(t *testing.T) {
	root := t.TempDir()
	out, err := scaffold(root, request{Kind: "store", Name: "todos", Fields: []string{"text:string", "done:bool"}})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	for file, wants := range map[string][]string{
		"client/store/todos.go": {
			"type Todo struct {",
			"Text string `json:\"text\"`",
			"func WithTodos(ctx context.Context, items []Todo) context.Context",
			"func TodosFrom(ctx context.Context) []Todo",
			"type TodoSnapshot struct {",
			"func (s *TodoStore) Apply(op TodoOp)",
		},
		"client/store/todos_client.go": {
			"var todoClient = NewTodoStore()",
			"Todos = signal.WithEq([]Todo(nil), sameTodos)",
			"TodoCount = signal.DeriveEq(",
			// The gate is the whole point: without it the server's store
			// writes a package-level variable from every request.
			"if s == todoClient {",
		},
	} {
		path := filepath.Join(root, filepath.FromSlash(file))
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the scaffold: %v", err)
		}
		if _, err := parser.ParseFile(token.NewFileSet(), path, body, parser.AllErrors); err != nil {
			t.Fatalf("%s does not parse: %v\n%s", file, err, body)
		}
		formatted, err := format.Source(body)
		if err != nil || string(formatted) != string(body) {
			t.Errorf("%s is not gofmt-clean", file)
		}
		for _, want := range wants {
			if !strings.Contains(string(body), want) {
				t.Errorf("%s is missing %q\n%s", file, want, body)
			}
		}
	}
	if !strings.Contains(out, "Read through the signal") {
		t.Errorf("the report does not state the rule that makes the effect work:\n%s", out)
	}
}

// A .client page whose body is only markup is the scaffold that taught nothing:
// the entire difference between .client and a plain page is the lifecycle below
// the markup.
func TestScaffoldClientPageIsReactive(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/myapp\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := scaffold(root, request{Kind: "store", Name: "todos"}); err != nil {
		t.Fatal(err)
	}
	if _, err := scaffold(root, request{Kind: "page", Path: "/todos", Client: true, Store: "todos"}); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(root, "client/pages/todos/index.client.templ"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(body)
	for _, want := range []string{
		"package todos",                       // a directory is a package, and Todos is a type name
		`"example.com/myapp/client/store"`,    // the real module path, or it compiles nowhere
		"func Mount()",                        // a plain func — templ has no lifecycle
		"func Unmount()",                      // ...and its pair, or every visit leaks
		"release = append(release, signal.Effect(repaint))", // the stop func is kept, not discarded
		"dom.Off(release...)",                              // ...and released
		"release, dom.Root().Query(\"[data-add]\").On(",     // On's handle is kept too
		"store.Todos.Get()",                   // read through the signal, not the store
		"it.Text",                             // the field the store actually declares
	} {
		if !strings.Contains(source, want) {
			t.Errorf("the client page is missing %q\n%s", want, source)
		}
	}

	// The page it writes has to survive the rules it will be checked against.
	if result := runCheck(root, false); result.Errors > 0 {
		t.Errorf("the scaffold does not pass howl check: %#v", result.Diagnostics)
	}
}

// Without a store there is nothing real to wire, so the page owns its signal —
// and still demonstrates the whole cycle, release included.
func TestScaffoldClientPageWithoutAStore(t *testing.T) {
	root := t.TempDir()
	if _, err := scaffold(root, request{Kind: "page", Path: "/counter", Client: true}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(root, "client/pages/counter/index.client.templ"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"var count = signal.Of(0)", "func Mount()", "func Unmount()",
		"release = append(release, signal.Effect(repaint))",
		"dom.Off(release...)",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("missing %q\n%s", want, body)
		}
	}
}
