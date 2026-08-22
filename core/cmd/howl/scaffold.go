package main

import (
	"cmp"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// scaffold writes a page or an endpoint file at the right location, with the
// right name.
//
// In this framework the file name carries the behaviour — .dyn, .client, .bare,
// .raw, .post — and the directory is the URL. Getting either wrong is a route
// that silently does not exist, or a build error two steps away from the
// mistake. That is exactly the kind of thing worth generating rather than
// remembering.
// request is one scaffold, named rather than positional: the kinds share a
// caller and almost none of the fields, so an argument list would be six
// zero values wide at every call site.
type request struct {
	Kind   string
	Path   string // the URL this serves
	Name   string // page label, endpoint Name, store or collection name
	Method string
	Store  string   // page only: the store it renders, wired for real
	Client bool     // page only: also rendered in the browser
	Roles  []string // endpoint only
	Fields []string // store and collection only
}

func scaffold(root string, req request) (string, error) {
	switch req.Kind {
	case "collection":
		return scaffoldCollection(root, cmp.Or(req.Name, req.Path), req.Fields)
	case "store":
		return scaffoldStore(root, cmp.Or(req.Name, req.Path), req.Fields)
	}
	if req.Path == "" {
		return "", fmt.Errorf("scaffold: path is required, e.g. /reports or /api/reports")
	}
	switch req.Kind {
	case "page":
		return scaffoldPage(root, req.Path, req.Name, req.Store, req.Client)
	case "endpoint":
		return scaffoldEndpoint(root, req.Path, req.Name, req.Method, req.Roles)
	}
	return "", fmt.Errorf("scaffold: kind must be \"page\", \"endpoint\", \"store\" or \"collection\", got %q", req.Kind)
}

// scaffoldCollection writes a db collection: the document struct, the two
// optional hooks, and the constructor that creates the table.
//
// Unlike a page or an endpoint, the location carries no machine meaning — no
// generator reads server/store/, and a collection is an ordinary Go value. The
// scaffold earns its place on the parts that are not guessable: the envelope is
// embedded rather than wrapped, Defaults must be on the pointer receiver or it
// silently mutates a copy, and the constructor runs DDL so it takes a context
// and returns an error.
func scaffoldCollection(root, collection string, fields []string) (string, error) {
	if collection == "" {
		return "", fmt.Errorf("scaffold: a collection needs a name, e.g. users")
	}
	table := strings.ToLower(strings.Trim(collection, "/"))
	if strings.ContainsAny(table, "/{}:") {
		return "", fmt.Errorf("scaffold: %q is not a collection name; pass a table name like users", collection)
	}
	doc := goIdent(singular(table))
	plural := goIdent(table)
	if plural == doc {
		plural = doc + "s"
	}
	r := strings.ToLower(doc[:1])

	decls, first := documentFields(fields)
	imports, validate := "", "\treturn nil"
	if first != "" {
		imports = "\t\"errors\"\n"
		validate = fmt.Sprintf("\tif %s.%s == \"\" {\n\t\treturn errors.New(%q)\n\t}\n\treturn nil",
			r, first, strings.ToLower(first)+": required")
	}

	body := strings.NewReplacer(
		"$DOC", doc, "$PLURAL", plural, "$R", r, "$TABLE", table,
		"$FIELDS", decls, "$IMPORTS", imports, "$VALIDATE", validate,
	).Replace(collectionTemplate)

	file := filepath.Join(root, "server", "store", table+".go")
	if err := write(file, body); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %s\n\ncollection: %s\ndocument:   %s\n\n"+
		"Call store.Open%s(ctx, conn) at startup. Nothing generates from this directory — "+
		"a collection is an ordinary Go value, so move it wherever it belongs.",
		rel(root, file), table, doc, plural), nil
}

const collectionTemplate = `package store

import (
	"context"
$IMPORTS
	"github.com/mirairoad/howl-go/db"
	"github.com/mirairoad/howl-go/db/pg"
)

// $DOC is one document. The struct is the schema: db.Doc supplies the id,
// the version and the audit/soft-delete envelope, and the json tags are the
// stored field names.
type $DOC struct {
	db.Doc
$FIELDS}

// Defaults runs on create, before validation. The pointer receiver is not
// optional: on a value receiver it still satisfies the interface, the service
// still calls it, and every default is written to a copy and lost.
func ($R *$DOC) Defaults() {
	// if $R.Plan == "" { $R.Plan = "free" }
}

// Validate runs on create and on every patch. A plain error becomes
// db.ErrInvalid, which an endpoint maps to a 400.
func ($R *$DOC) Validate() error {
$VALIDATE
}

// $PLURAL is the collection. Adding a field to $DOC needs no migration; the
// only thing that changes DDL is the Promote list below.
var $PLURAL *pg.Service[$DOC, *$DOC]

// Open$PLURAL creates the table if it does not exist and wires the service.
// Call it once at startup, after the connection is open.
func Open$PLURAL(ctx context.Context, conn pg.Conn) error {
	s, err := pg.New[$DOC](ctx, conn, pg.Options{
		Collection: "$TABLE",
		// Unique:  []string{"email"},              // a unique generated column
		// Promote: []pg.Promote{{Path: "plan"}},   // index what you filter on
	})
	if err != nil {
		return err
	}
	$PLURAL = s
	return nil
}
`

// documentFields turns "email:string" pairs into struct fields, and reports the
// first string field so Validate is a working example rather than a stub.
func documentFields(fields []string) (string, string) {
	if len(fields) == 0 {
		fields = []string{"name:string"}
	}
	var decls strings.Builder
	first := ""
	for _, f := range fields {
		name, typ, ok := strings.Cut(f, ":")
		if name == "" {
			continue
		}
		if !ok || typ == "" {
			typ = "string"
		}
		ident := goIdent(name)
		if first == "" && typ == "string" {
			first = ident
		}
		json := strings.ToLower(strings.NewReplacer(" ", "_", "-", "_").Replace(name))
		fmt.Fprintf(&decls, "\t%s %s `json:%q`\n", ident, typ, json)
	}
	if decls.Len() == 0 {
		return "\tName string `json:\"name\"`\n", "Name"
	}
	return decls.String(), first
}

// singular is deliberately dumb — it only has to turn a table name into a
// readable type name, and the file it writes is meant to be edited.
func singular(s string) string {
	switch {
	case strings.HasSuffix(s, "ies"):
		return strings.TrimSuffix(s, "ies") + "y"
	case strings.HasSuffix(s, "sses"), strings.HasSuffix(s, "xes"):
		return strings.TrimSuffix(s, "es")
	case strings.HasSuffix(s, "s") && !strings.HasSuffix(s, "ss"):
		return strings.TrimSuffix(s, "s")
	}
	return s
}

// scaffoldStore writes the browser-side store: the domain type, the mutation
// methods, the wire format, and the signals that make a page react to them.
//
// This is the piece that is hardest to arrive at by reading the API list. The
// shape is two files because it is two rules:
//
//   - <name>.go compiles for the server AND for GOOS=js. It is the domain, and
//     it is why there is no second implementation of the mutation logic in
//     TypeScript. Nothing server-shaped may enter it.
//   - <name>_client.go holds package-level signals. The server must never write
//     them — two concurrent requests would be writing one variable — so publish
//     is gated on the browser's own instance.
func scaffoldStore(root, name string, fields []string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("scaffold: a store needs a name, e.g. todos")
	}
	base := strings.ToLower(strings.Trim(name, "/"))
	if strings.ContainsAny(base, "/{}:") {
		return "", fmt.Errorf("scaffold: %q is not a store name; pass something like todos", name)
	}
	item := goIdent(singular(base))
	plural := goIdent(base)
	if plural == item {
		plural = item + "s"
	}
	lower := strings.ToLower(item[:1]) + item[1:]

	decls, first := storeFields(fields, item)

	// $FIRSTJSON before $FIRST: strings.Replacer takes the first pattern that
	// matches at a position, so the shorter one would eat the longer one's
	// prefix and leave "JSON" behind.
	rep := strings.NewReplacer(
		"$ITEM", item, "$PLURAL", plural, "$LOWER", lower,
		"$FIELDS", decls, "$FIRSTJSON", strings.ToLower(first), "$FIRST", first,
	)

	domain := filepath.Join(root, "client", "store", base+".go")
	if err := write(domain, rep.Replace(storeDomainTemplate)); err != nil {
		return "", err
	}
	reactive := filepath.Join(root, "client", "store", base+"_client.go")
	if err := write(reactive, rep.Replace(storeClientTemplate)); err != nil {
		return "", err
	}

	return fmt.Sprintf(`wrote %s
wrote %s

store:   %sStore        (server + wasm; the domain, and the only copy of the mutation logic)
signals: %s, %sCount  (browser only; package-level, so the server must never write them)

The three wires, in the order they run:

 1. SSR    — Config.Data: ctx = store.With%s(ctx, srv.List()); the page renders from ctx.
 2. Handoff — an endpoint returns %sSnapshot; the browser calls it once from Mount.
 3. Local  — %sClient().Apply(op) mutates, publish() sets the signal, the effect repaints.

Read through the signal (%s.Get()), never through the store: that is what
registers the effect as a dependent. Scaffold the page that renders it with
kind:"page", client:true, store:%q.`,
		rel(root, domain), rel(root, reactive),
		item, plural, item, plural, item, plural, plural, base), nil
}

// storeFields turns "text:string" pairs into the item's fields. The first
// string field becomes the one Add takes, so the scaffold has a working
// mutation rather than a stub.
func storeFields(fields []string, item string) (string, string) {
	if len(fields) == 0 {
		fields = []string{"text:string"}
	}
	var decls strings.Builder
	first := ""
	for _, f := range fields {
		fname, typ, ok := strings.Cut(f, ":")
		if fname == "" {
			continue
		}
		if !ok || typ == "" {
			typ = "string"
		}
		ident := goIdent(fname)
		if first == "" && typ == "string" {
			first = ident
		}
		json := strings.ToLower(strings.NewReplacer(" ", "_", "-", "_").Replace(fname))
		fmt.Fprintf(&decls, "\t%s %s `json:%q`\n", ident, typ, json)
	}
	if first == "" {
		first = "Text"
		decls.WriteString("\tText string `json:\"text\"`\n")
	}
	return decls.String(), first
}

// The domain half. No net/http, no database/sql, no os — this file is compiled
// into the wasm binary through the pages that import it, and one server-only
// import takes the whole browser build down with it.
const storeDomainTemplate = `package store

import (
	"context"
	"sync"
)

// $ITEM is one record. The same struct is rendered by the server, serialised to
// the browser, and mutated there.
type $ITEM struct {
	ID int ` + "`json:\"id\"`" + `
$FIELDS}

// ---------------------------------------------------------------------------
// The SSR handoff. Pages take no arguments — the generated route table needs
// one uniform signature — so their data arrives through the context templ
// already threads into Render. Set it in Config.Data.
// ---------------------------------------------------------------------------

type $LOWERKey struct{}

func With$PLURAL(ctx context.Context, items []$ITEM) context.Context {
	return context.WithValue(ctx, $LOWERKey{}, items)
}

func $PLURALFrom(ctx context.Context) []$ITEM {
	items, _ := ctx.Value($LOWERKey{}).([]$ITEM)
	return items
}

// ---------------------------------------------------------------------------
// The store. One instance per process on the server, one in the browser tab.
// ---------------------------------------------------------------------------

type $ITEMStore struct {
	mu     sync.Mutex
	nextID int
	items  []$ITEM
}

func New$ITEMStore() *$ITEMStore { return &$ITEMStore{} }

// $ITEMSnapshot is the wire format: whole-state serialisation, used to hydrate
// the browser from the server and to reconcile back the other way.
type $ITEMSnapshot struct {
	NextID int     ` + "`json:\"nextId\"`" + `
	Items  []$ITEM ` + "`json:\"items\"`" + `
}

// $ITEMOp is a single mutation. The browser applies it locally for an instant
// repaint and ships the same value to the server, which applies it identically
// — one implementation of the rules, running in two places.
type $ITEMOp struct {
	Kind string ` + "`json:\"kind\"`" + `
	$FIRST string ` + "`json:\"$FIRSTJSON,omitempty\"`" + `
	ID    int    ` + "`json:\"id,omitempty\"`" + `
}

func (s *$ITEMStore) Snapshot() $ITEMSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]$ITEM, len(s.items))
	copy(out, s.items)
	return $ITEMSnapshot{NextID: s.nextID, Items: out}
}

func (s *$ITEMStore) Restore(sn $ITEMSnapshot) {
	s.mu.Lock()
	s.nextID = sn.NextID
	s.items = append(s.items[:0], sn.Items...)
	s.mu.Unlock()
	s.publish()
}

func (s *$ITEMStore) List() []$ITEM { return s.Snapshot().Items }

func (s *$ITEMStore) Add(v string) $ITEM {
	s.mu.Lock()
	s.nextID++
	item := $ITEM{ID: s.nextID, $FIRST: v}
	s.items = append(s.items, item)
	s.mu.Unlock()
	s.publish()
	return item
}

func (s *$ITEMStore) Del(id int) {
	s.mu.Lock()
	for i, item := range s.items {
		if item.ID == id {
			s.items = append(s.items[:i], s.items[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	s.publish()
}

// Apply runs one op. The server and the browser call this same method.
func (s *$ITEMStore) Apply(op $ITEMOp) {
	switch op.Kind {
	case "add":
		s.Add(op.$FIRST)
	case "del":
		s.Del(op.ID)
	}
}
`

// The reactive half. Split out because the rule is different: everything here
// is package-level and therefore browser-only.
const storeClientTemplate = `package store

import "github.com/mirairoad/howl-go/core/signal"

// The browser's store, exposed reactively.
//
// On the server a store is per-process and read through the request context —
// two requests must never see each other's state. In the browser there is one
// user and one tab, so package-level signals are the right shape: any Mount,
// Unmount or event handler can read them, and anything derived from them
// updates itself.
var $LOWERClient = New$ITEMStore()

// $PLURALClient is the browser-side store. Mutating it publishes to the
// signals below.
func $PLURALClient() *$ITEMStore { return $LOWERClient }

var (
	// $PLURAL is the reactive list. Slices are not comparable, so it carries an
	// explicit equality test — without one, a re-hydrate that changed nothing
	// would still wake every dependent.
	$PLURAL = signal.WithEq([]$ITEM(nil), same$PLURAL)

	// $ITEMCount is derived. DeriveEq means a mutation that leaves the count
	// alone does not wake anything that only reads the count.
	$ITEMCount = signal.DeriveEq(func() int { return len($PLURAL.Get()) })
)

func same$PLURAL(a, b []$ITEM) bool {
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

// publish mirrors a mutation into the signals. Only the browser's instance does
// this: the server's store is a different pointer, so concurrent requests never
// write these package-level variables.
func (s *$ITEMStore) publish() {
	if s == $LOWERClient {
		$PLURAL.Set(s.List())
	}
}
`

func scaffoldPage(root, urlPath, label, store string, client bool) (string, error) {
	segments := splitPath(urlPath)
	if len(segments) == 0 {
		return "", fmt.Errorf("scaffold: / already exists as client/pages/index.templ")
	}

	// A {param} segment becomes a file named after the parameter with .dyn,
	// because Go rejects both brackets and braces in file names and import
	// paths. Everything before it is an ordinary directory.
	var dirs []string
	stem := "index"
	modifiers := ""
	for i, seg := range segments {
		last := i == len(segments)-1
		if param, ok := parameter(seg); ok {
			if !last {
				dirs = append(dirs, param+".dyn")
				continue
			}
			stem, modifiers = param, ".dyn"
			continue
		}
		if last {
			dirs = append(dirs, seg)
			continue
		}
		dirs = append(dirs, seg)
	}
	if client {
		modifiers += ".client"
	}

	dir := filepath.Join(append([]string{root, "client", "pages"}, dirs...)...)
	file := filepath.Join(dir, stem+modifiers+".templ")
	// Lowercase: a directory becomes a Go package, and Todos is a type name.
	pkg := strings.ToLower(goIdent(filepath.Base(dir)))
	if label == "" {
		label = title(strings.Trim(segments[len(segments)-1], "{}"))
	}
	component := goIdent(label)
	if component == "" || component == "Index" {
		component = "Page"
	}

	body := fmt.Sprintf(`package %s

templ Head() {
	<title>%s</title>
}

templ %s() {
	<section>
		<h1>%s</h1>
	</section>
}
`, pkg, label, component, label)

	note := "Re-run the generators (make, or howl dev picks it up on save)."
	if client {
		body = clientPage(root, pkg, label, component, store)
		note = "Re-run the generators, then build the wasm binary — a .client route needs it:\n" +
			"  GOOS=js GOARCH=wasm go build -o client/public/views.wasm ./wasm\n\n" +
			"Mount and Unmount are a pair. Everything Mount registers, Unmount releases; " +
			"without that every visit adds a live effect firing at a DOM that was thrown away."
		if store == "" {
			note += "\n\nThe page owns its signal for now. Once a second page reads the same data, " +
				"move it into client/store (howl_scaffold kind:\"store\") — signals are package-level, " +
				"and a store is where that is a design rather than an accident."
		}
	}

	if err := write(file, body); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %s\n\nroute:   %s\npackage: %s\n\n%s",
		rel(root, file), urlPath, pkg, note), nil
}

// clientPage is the body of a .client route: the page markup, plus the
// lifecycle and the reactive loop the browser runs.
//
// A scaffold that emitted only markup here was worse than nothing. The whole
// difference between .client and a plain page is the code below it, and it is
// the part with no analogue in a JS framework: no hooks, no re-render, no
// virtual DOM — a plain func, an auto-tracked effect, and one component
// rendered by the same templ code the server ran.
func clientPage(root, pkg, label, component, store string) string {
	// Wired to a store when there is one to wire to: the signal, the hydrate
	// call and the repaint then all refer to something real, which is the
	// difference between an example and a working page.
	if store == "" {
		return fmt.Sprintf(`package %s

import (
	"strconv"

	"github.com/mirairoad/howl-go/core/dom"
	"github.com/mirairoad/howl-go/core/signal"
)

// count is the reactive state. Package-level, because there is one browser tab
// and one user — and because a signal read inside an effect is what registers
// the dependency. The server never writes it.
var count = signal.Of(0)

templ Head() {
	<title>%s</title>
}

templ %s() {
	<section>
		<h1>%s</h1>
		<button data-inc>+1</button>
		<output data-count>0</output>
	</section>
}

// release holds everything Mount registered — the effect's stop func and the
// listener's release func are the same shape, so Unmount treats them alike.
// This is the whole lifecycle contract: what Mount registers, Unmount releases.
var release []func()

// Mount runs in the browser after this page's markup is in the DOM — on the
// cold load and again after every client-side navigation here. It is a plain
// Go func, not a templ block: templ produces markup, func does something.
func Mount() {
	// On returns the func that removes the listener. Dropping it leaks the Go
	// closure behind it for the life of the tab, once per visit.
	release = append(release, dom.Root().Query("[data-inc]").On("click", func() {
		count.Set(count.Get() + 1)
	}))

	// No dependency array. The effect installs itself while it runs, so every
	// Get() inside registers the edge — the list cannot be wrong because there
	// is not one.
	release = append(release, signal.Effect(repaint))
}

// Unmount runs just before this page's markup is replaced.
func Unmount() {
	dom.Off(release...)
	release = nil
}

func repaint() {
	out := dom.Root().Query("[data-count]")
	if !out.Valid() {
		return // the page has already been swapped away
	}
	out.SetText(strconv.Itoa(count.Get()))
}
`, pkg, label, component, label)
	}

	base := strings.ToLower(strings.Trim(store, "/"))
	item := goIdent(singular(base))
	plural := goIdent(base)
	if plural == item {
		plural = item + "s"
	}
	storeImport := modulePath(root) + "/client/store"

	return strings.NewReplacer(
		"$PKG", pkg, "$LABEL", label, "$COMPONENT", component,
		"$ITEMLIST", item+"List", "$ITEM", item, "$PLURAL", plural, "$IMPORT", storeImport,
		"$FIRSTFIELD", storeFirstField(root, base, item),
	).Replace(`package $PKG

import (
	"context"
	"strings"

	"github.com/mirairoad/howl-go/core/dom"
	"github.com/mirairoad/howl-go/core/signal"

	"$IMPORT"
)

templ Head() {
	<title>$LABEL</title>
}

// The list is rendered from ctx here and from the signal in repaint below —
// deliberately the same component, so the server's first paint and every local
// update produce identical markup. Move $PLURAL into client/ui when a second
// page needs it.
templ $COMPONENT() {
	<section>
		<h1>$LABEL</h1>
		<button data-add>add</button>
		<ul data-list>
			@$ITEMLIST(store.$PLURALFrom(ctx))
		</ul>
	</section>
}

templ $ITEMLIST(items []store.$ITEM) {
	for _, it := range items {
		<li>{ it.$FIRSTFIELD }</li>
	}
}

// release holds every registration Mount made — effects, watchers and DOM
// listeners alike, since all three hand back a func().
var release []func()

// Mount hydrates the browser's store from the server, then renders from it.
// After this runs the server is out of the loop: a mutation repaints locally
// and is reported afterwards, so nobody waits on a round trip.
func Mount() {
	// On hands back the func that removes the listener. Dropping it leaks the
	// Go closure behind it for the life of the tab, once per visit.
	release = append(release, dom.Root().Query("[data-add]").On("click", func() {
		// Local first: apply, which publishes to the signal, which repaints.
		store.$PLURALClient().Apply(store.$ITEMOp{Kind: "add", $FIRSTFIELD: "new"})
	}))

	// repaint reads the signal inside itself, so the dependency is discovered
	// by running — there is no list to keep correct.
	release = append(release, signal.Effect(repaint))
	release = append(release, signal.Watch(store.$ITEMCount.Get, func(now, before int) {
		dom.Log("[$PKG] count", before, "->", now)
	}))

	// Hydrate from the server, then render from the local store. The generated
	// client is typed against the same Go types the endpoint declares, so
	// renaming a field breaks the build on both sides at once. Scaffold the
	// endpoint (kind:"endpoint"), run fsapis, then uncomment:
	//
	//	go func() {
	//		sn, err := apiclient.New("").$PLURAL(context.Background())
	//		if err != nil {
	//			dom.Warn("[$PKG] hydrate failed:", err.Error())
	//			return
	//		}
	//		store.$PLURALClient().Restore(sn) // publishes to the signal, which repaints
	//	}()
	//
	// The goroutine is not optional: blocking the JS callback deadlocks the Go
	// scheduler, because the fetch can only resolve once control returns to the
	// event loop.
}

// Unmount releases every registration Mount made. An effect that outlives its
// DOM keeps firing against nodes that were thrown away, and every visit adds
// another one.
func Unmount() {
	dom.Off(release...)
	release = nil
}

func repaint() {
	list := dom.Root().Query("[data-list]")
	if !list.Valid() {
		return // the page has already been swapped away
	}
	// Read through the signal, not the store: that is what registers this
	// effect as a dependent.
	items := store.$PLURAL.Get()

	var sb strings.Builder
	if err := $ITEMLIST(items).Render(context.Background(), &sb); err != nil {
		dom.Warn("[$PKG] render failed:", err.Error())
		return
	}
	list.SetHTML(sb.String())
}
`)
}

// storeFirstField finds the field the store's Add takes, so the generated page
// renders a field that exists. Reading the store rather than assuming a name is
// the difference between a scaffold that compiles and one that looks right.
func storeFirstField(root, base, item string) string {
	body, err := os.ReadFile(filepath.Join(root, "client", "store", base+".go"))
	if err != nil {
		return "Text"
	}
	// The type name is the only part that varies, so only that part is built
	// per call; the field pattern is compiled once.
	block := regexp.MustCompile(`(?s)type\s+` + regexp.QuoteMeta(item) + `\s+struct\s*\{(.*?)\n\}`).FindSubmatch(body)
	if block == nil {
		return "Text"
	}
	if m := stringFieldRe.FindSubmatch(block[1]); m != nil {
		return string(m[1])
	}
	return "Text"
}

var stringFieldRe = regexp.MustCompile(`(?m)^\s*([A-Z]\w*)\s+string\b`)

// modulePath reads the module line out of go.mod. A generated import has to be
// the real path — a placeholder would compile nowhere, and the one thing a
// scaffold owes its caller is a file that builds.
func modulePath(root string) string {
	body, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "example.com/myapp"
	}
	for _, line := range strings.Split(string(body), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return "example.com/myapp"
}

func scaffoldEndpoint(root, urlPath, name, method string, roles []string) (string, error) {
	if method == "" {
		method = "GET"
	}
	method = strings.ToUpper(method)

	segments := splitPath(strings.TrimPrefix(urlPath, "/api"))
	if len(segments) == 0 {
		return "", fmt.Errorf("scaffold: an endpoint needs a path under /api, e.g. /api/reports")
	}

	var dirs []string
	stem := "index"
	modifiers := ""
	for i, seg := range segments {
		last := i == len(segments)-1
		if param, ok := parameter(seg); ok {
			if last {
				stem, modifiers = param, ".dyn"
				continue
			}
			dirs = append(dirs, param+".dyn")
			continue
		}
		if last {
			stem = seg
			continue
		}
		dirs = append(dirs, seg)
	}
	if method != "GET" {
		modifiers += "." + strings.ToLower(method)
	}

	dir := filepath.Join(append([]string{root, "server", "apis"}, dirs...)...)
	file := filepath.Join(dir, stem+modifiers+".api.go")
	pkg := "apis"
	if len(dirs) > 0 {
		pkg = goIdent(filepath.Base(dir))
		pkg = strings.ToLower(pkg[:1]) + pkg[1:]
	}
	if name == "" {
		name = title(stem)
	}
	symbol := goIdent(name)
	response := symbol + "Response"

	rolesLine := ""
	if len(roles) > 0 {
		quoted := make([]string, 0, len(roles))
		for _, r := range roles {
			quoted = append(quoted, fmt.Sprintf("%q", r))
		}
		rolesLine = fmt.Sprintf("\tRoles: []string{%s},\n", strings.Join(quoted, ", "))
	}

	bodyType := "api.None"
	bodyDecl := ""
	if method == "POST" || method == "PUT" || method == "PATCH" {
		bodyType = symbol + "Input"
		bodyDecl = fmt.Sprintf(`// %s is the request body. Implement Validate to reject bad input before the
// handler runs; a plain error becomes a 400.
type %s struct {
	Name string `+"`json:\"name\"`"+`
}

func (i %s) Validate() error {
	if i.Name == "" {
		return api.Invalid("name", "is required")
	}
	return nil
}

`, bodyType, bodyType, bodyType)
	}

	source := fmt.Sprintf(`package %s

import (
	"github.com/mirairoad/howl-go/core/api"
)

%s// %s is what this endpoint returns.
type %s struct {
	OK bool `+"`json:\"ok\"`"+`
}

// %s serves %s %s.
var %s = api.Define(api.Spec[api.None, %s, %s]{
	Name: %q,
%s	Handler: func(r *api.Request[api.None, %s]) (%s, error) {
		return %s{OK: true}, nil
	},
})
`, pkg, bodyDecl, response, response, symbol, method, urlPath, symbol, bodyType, response, name, rolesLine, bodyType, response, response)

	if err := write(file, source); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %s\n\nendpoint: %s %s\npackage:  %s\n\nRe-run fsapis (make) to add it to the table and the client.",
		rel(root, file), method, urlPath, pkg), nil
}

func write(file, body string) error {
	if _, err := os.Stat(file); err == nil {
		return fmt.Errorf("scaffold: %s already exists", file)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	// Go source goes out formatted, so a scaffold never lands as the one file
	// in the tree that gofmt wants to rewrite. templ source is left alone —
	// go/format does not parse it.
	if filepath.Ext(file) == ".go" {
		if formatted, err := format.Source([]byte(body)); err == nil {
			body = string(formatted)
		}
	}
	return os.WriteFile(file, []byte(body), 0o644)
}

func splitPath(p string) []string {
	var out []string
	for _, seg := range strings.Split(strings.Trim(p, "/"), "/") {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

func parameter(seg string) (string, bool) {
	if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
		return seg[1 : len(seg)-1], true
	}
	if strings.HasPrefix(seg, ":") {
		return seg[1:], true
	}
	return "", false
}

func title(s string) string {
	s = strings.NewReplacer("-", " ", "_", " ").Replace(s)
	if s == "" {
		return "Page"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// goIdent turns "metric series" or "get-me" into MetricSeries / GetMe.
func goIdent(s string) string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9')
	})
	var b strings.Builder
	for _, f := range fields {
		b.WriteString(strings.ToUpper(f[:1]) + f[1:])
	}
	if b.Len() == 0 {
		return "Page"
	}
	return b.String()
}

func rel(root, file string) string {
	if r, err := filepath.Rel(root, file); err == nil {
		return filepath.ToSlash(r)
	}
	return file
}
