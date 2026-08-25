package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// howl check — the conventions, enforced instead of described
//
// llms.txt says a page may not import core/app and that `templ Mount()` does
// not exist. Until now nothing checked: you found out from a Go error two steps
// removed from the rule you broke, or you did not find out at all.
//
// Every rule here is one this framework already relies on. The generators
// enforce the ones they can see (an unknown file-name modifier is a hard
// error); these are the ones that only show up as a confusing failure later, or
// never.
// ---------------------------------------------------------------------------

// Diagnostic is one broken rule. The shape is deliberately flat: it is printed
// for a human, handed to an editor, and returned over MCP without translation.
type Diagnostic struct {
	File    string `json:"file"`
	Line    int    `json:"line,omitempty"`
	Rule    string `json:"rule"`
	Level   string `json:"level"` // error | warning
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

type checkResult struct {
	OK          bool         `json:"ok"`
	Errors      int          `json:"errors"`
	Warnings    int          `json:"warnings"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

func check(args []string) error {
	fset := flag.NewFlagSet("check", flag.ExitOnError)
	dir := fset.String("dir", ".", "module root")
	asJSON := fset.Bool("json", false, "emit diagnostics as JSON")
	build := fset.Bool("build", true, "also run the generators and go build")
	fset.Parse(args)

	root, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	result := runCheck(root, *build)

	if *asJSON {
		out, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(out))
	} else {
		for _, d := range result.Diagnostics {
			where := d.File
			if d.Line > 0 {
				where = fmt.Sprintf("%s:%d", d.File, d.Line)
			}
			fmt.Printf("%s: %s [%s] %s\n", where, d.Level, d.Rule, d.Message)
			if d.Fix != "" {
				fmt.Printf("    fix: %s\n", d.Fix)
			}
		}
		if result.OK {
			fmt.Println("ok — no problems found")
		} else {
			fmt.Printf("%d error(s), %d warning(s)\n", result.Errors, result.Warnings)
		}
	}
	if result.Errors > 0 {
		os.Exit(1)
	}
	return nil
}

func runCheck(root string, build bool) checkResult {
	var out []Diagnostic
	files := sourceFiles(root)

	out = append(out, lintPages(root, files)...)
	out = append(out, lintClientRenderSafety(root, files)...)
	out = append(out, lintReactivity(root, files)...)
	out = append(out, lintComponents(root, files)...)
	out = append(out, lintPerformance(root, files)...)
	out = append(out, lintShell(root, files)...)
	out = append(out, lintEndpoints(root, files)...)
	out = append(out, lintCollections(root, files)...)
	out = append(out, lintGenerated(root, files)...)
	if build {
		out = append(out, lintBuild(root)...)
	}

	// Sorted, because the rules run in whatever order suits them and a reader
	// works through a file top to bottom.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})

	result := checkResult{Diagnostics: out}
	for _, d := range out {
		if d.Level == "error" {
			result.Errors++
		} else {
			result.Warnings++
		}
	}
	result.OK = result.Errors == 0
	if out == nil {
		result.Diagnostics = []Diagnostic{}
	}
	return result
}

// Client-renderable templates run in two different Go binaries. A package
// global therefore means two different values even when both builds compile:
// build.Tag(), time.Now(), environment-backed config and session singletons all
// render correctly on the server and quietly change after local navigation.
//
// Start from every .client route and every layout that can wrap one, then walk
// module-local imports. That includes shared UI components; checking only the
// route file misses exactly the attractive Layout -> ui.Footer -> build.Tag
// shape that caused this rule to exist.
func lintClientRenderSafety(root string, files []sourceFile) []Diagnostic {
	module := modulePath(root)
	if module == "" {
		return nil
	}
	byDir := map[string][]sourceFile{}
	clientDirs := map[string]bool{}
	var clientFiles []string
	for _, f := range files {
		dir := path.Dir(f.Rel)
		if dir == "." {
			dir = ""
		}
		byDir[dir] = append(byDir[dir], f)
		if strings.HasSuffix(f.Rel, ".client.templ") {
			clientDirs[dir] = true
			clientFiles = append(clientFiles, f.Rel)
		}
	}
	// A layout is part of the render surface when a client route is below its
	// directory. Its whole package is linked into the wasm route table.
	for _, f := range files {
		if filepath.Base(f.Rel) != "layout.templ" {
			continue
		}
		dir := path.Dir(f.Rel)
		if dir == "." {
			dir = ""
		}
		prefix := strings.TrimSuffix(dir, "/") + "/"
		for _, rel := range clientFiles {
			if dir == "" || strings.HasPrefix(rel, prefix) {
				clientDirs[dir] = true
				break
			}
		}
	}

	queue := make([]string, 0, len(clientDirs))
	for dir := range clientDirs {
		queue = append(queue, dir)
	}
	surface := map[string]bool{}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if surface[dir] {
			continue
		}
		surface[dir] = true
		for _, f := range byDir[dir] {
			for _, imp := range sourceImports(f) {
				if imp == module {
					queue = append(queue, "")
				} else if strings.HasPrefix(imp, module+"/") {
					queue = append(queue, strings.TrimPrefix(imp, module+"/"))
				}
			}
		}
	}

	var out []Diagnostic
	seenImport := map[string]bool{}
	for dir := range surface {
		serverMarked := false
		for _, f := range byDir[dir] {
			if bytes.Contains(f.Body, []byte("//howl:server")) {
				serverMarked = true
			}
		}
		if serverMarked {
			// The importing edge below reports the useful location; a marker on
			// an entry package has no such edge, so report the marker itself.
			if clientDirs[dir] {
				for _, f := range byDir[dir] {
					if line, ok := findLine(f.Body, serverDirectiveRe); ok {
						out = append(out, Diagnostic{File: f.Rel, Line: line, Rule: "client-imports-server-package", Level: "error", Message: "a client-renderable package is marked //howl:server", Fix: "move runtime state behind Config.Bootstrap or a route-data endpoint and hydrate it into ctx"})
						break
					}
				}
			}
		}
		for _, f := range byDir[dir] {
			if strings.HasSuffix(f.Rel, ".templ") {
				if line, ok := findLine(f.Body, runtimeStateCallRe); ok {
					out = append(out, Diagnostic{File: f.Rel, Line: line, Rule: "client-runtime-global", Level: "error", Message: "client-renderable markup reads process runtime state; the server and wasm binaries can produce different HTML", Fix: "compute the value on the server, return it from Config.Bootstrap or route data, and read the hydrated value from ctx"})
				}
			}
			for _, imp := range sourceImports(f) {
				key := f.Rel + "\x00" + imp
				if seenImport[key] {
					continue
				}
				seenImport[key] = true
				level, message := "", ""
				if imp == "os" || imp == "os/exec" || strings.Contains(imp, "/server/") || strings.HasSuffix(imp, "/server") || markedServerImport(module, imp, byDir) {
					level, message = "error", "a client-renderable package imports server/runtime state"
				} else if suspiciousClientImportRe.MatchString(imp) {
					level, message = "error", "a client-renderable package imports build/config/auth state; values from it describe the wasm build instead of the running server"
				}
				if level != "" {
					line := importLine(f.Body, imp)
					out = append(out, Diagnostic{File: f.Rel, Line: line, Rule: "client-imports-server-package", Level: level, Message: message + ": " + imp, Fix: "expose compile-time constants separately; deliver runtime values through Config.Bootstrap or route data"})
				}
			}
		}
	}
	return out
}

func sourceImports(f sourceFile) []string {
	if strings.HasSuffix(f.Rel, ".go") {
		parsed, err := parser.ParseFile(token.NewFileSet(), f.Rel, f.Body, parser.ImportsOnly)
		if err != nil {
			return nil
		}
		out := make([]string, 0, len(parsed.Imports))
		for _, imp := range parsed.Imports {
			out = append(out, strings.Trim(imp.Path.Value, `"`))
		}
		return out
	}
	var out []string
	for _, m := range importLineRe.FindAllSubmatch(f.Body, -1) {
		out = append(out, string(m[2]))
	}
	return out
}

func markedServerImport(module, imp string, byDir map[string][]sourceFile) bool {
	if imp != module && !strings.HasPrefix(imp, module+"/") {
		return false
	}
	dir := strings.TrimPrefix(strings.TrimPrefix(imp, module), "/")
	for _, f := range byDir[dir] {
		if bytes.Contains(f.Body, []byte("//howl:server")) {
			return true
		}
	}
	return false
}

func importLine(body []byte, imp string) int {
	quoted := `"` + imp + `"`
	for i, line := range strings.Split(string(body), "\n") {
		if strings.Contains(line, quoted) {
			return i + 1
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// Rules
// ---------------------------------------------------------------------------

var (
	importAppRe        = regexp.MustCompile(`"[^"]*howl-go/core/app"`)
	importDBRe         = regexp.MustCompile(`"[^"]*howl-go/db(/[\w/]+)?"`)
	templMountRe       = regexp.MustCompile(`(?m)^templ\s+(Mount|Unmount)\s*\(`)
	rawQueryRe         = regexp.MustCompile(`r\.HTTP\.(URL\.Query\(\)|FormValue|PostFormValue)`)
	rolesRe            = regexp.MustCompile(`Roles:\s*\[\]string\{[^}]*"`)
	authorizeRe        = regexp.MustCompile(`Authorize:\s*`)
	structFieldRe      = regexp.MustCompile(`(?m)^\s+([A-Z]\w*)\s+[\[\]\*\w\.]+(\s+` + "`" + `[^` + "`" + `]*` + "`" + `)?\s*$`)
	generatedRe        = regexp.MustCompile(`^// Code generated`)
	runtimeStateCallRe = regexp.MustCompile(`\b(os\.(Getenv|LookupEnv|Environ)|time\.Now|runtime\.Version)\s*\(`)
	// A route package is often legitimately named settings/config or auth.
	// The heuristic is about application-internal runtime state, not the last
	// segment of every import path. Other server-only packages can opt into the
	// exact //howl:server marker checked above.
	suspiciousClientImportRe = regexp.MustCompile(`/internal/(build|config|auth|session)$`)
	serverDirectiveRe        = regexp.MustCompile(`//howl:server`)
)

// A page may not import core/app: the generated table lives inside the page
// tree, so anything a page imports has to stay a leaf. Breaking it produces
// either an import cycle or net/http linked into the wasm build.
func lintPages(root string, files []sourceFile) []Diagnostic {
	var out []Diagnostic
	for _, f := range files {
		inPages := strings.Contains(f.Rel, "client/pages/") || strings.HasPrefix(f.Rel, "client/pages")
		if !inPages {
			continue
		}
		if strings.HasSuffix(f.Rel, ".go") || strings.HasSuffix(f.Rel, ".templ") {
			if line, ok := findLine(f.Body, importAppRe); ok {
				out = append(out, Diagnostic{
					File: f.Rel, Line: line, Rule: "page-imports-app", Level: "error",
					Message: "a page imports core/app; the generated route table lives here, so page imports must stay leaves",
					Fix:     "use core/router (router.NotFound, router.ClientConfig, router.Param) — or move the code out of client/pages",
				})
			}
			// Same rule, different package: db drags database/sql into a build
			// that may target wasm, and a page reaching storage directly is the
			// import that makes the page tree stop being leaves.
			if line, ok := findLine(f.Body, importDBRe); ok {
				out = append(out, Diagnostic{
					File: f.Rel, Line: line, Rule: "page-imports-db", Level: "error",
					Message: "a page imports db; page imports must stay leaves, and db links database/sql into the wasm build",
					Fix:     "load the documents in an endpoint or Config.Data and hand them to the page through ctx",
				})
			}
		}
		if strings.HasSuffix(f.Rel, ".templ") {
			if line, ok := findLine(f.Body, templMountRe); ok {
				out = append(out, Diagnostic{
					File: f.Rel, Line: line, Rule: "templ-mount", Level: "error",
					Message: "`templ Mount()` cannot exist — a templ block compiles to a function that writes HTML",
					Fix:     "declare it as a plain Go func in the same file: func Mount() { … }",
				})
			}
		}
	}
	return out
}

// The shell has four parts the client runtime depends on. Each missing one
// fails silently and differently: no swap target, no teardown, stacking head
// tags, no dev reload.
func lintShell(root string, files []sourceFile) []Diagnostic {
	var out []Diagnostic
	for _, f := range files {
		if filepath.Base(f.Rel) != "app.templ" {
			continue
		}
		// Prose in an HTML comment is not markup. Without stripping it, a shell
		// that merely *mentions* howl-client in a comment passes the check —
		// which is exactly how examples/hello slipped through while still
		// publishing the older config.
		//
		// The page-head markers are the exception: they are HTML comments on
		// purpose, so that rule reads the original body.
		body := stripHTMLComments(string(f.Body))
		missing := func(needle, rule, message, fix string) {
			if !strings.Contains(body, needle) {
				out = append(out, Diagnostic{File: f.Rel, Rule: rule, Level: "error", Message: message, Fix: fix})
			}
		}
		missing(`id="outlet"`, "shell-outlet",
			"the document shell has no #outlet; client navigation has nothing to swap",
			`add <main id="outlet" data-route={ router.Current(ctx) }>{ children... }</main>`)
		missing("data-route", "shell-data-route",
			"the outlet has no data-route; the outgoing page's Unmount cannot be found",
			`add data-route={ router.Current(ctx) } to the #outlet element`)
		if !strings.Contains(string(f.Body), "page-head") {
			out = append(out, Diagnostic{File: f.Rel, Rule: "shell-page-head", Level: "error",
				Message: "the shell has no <!--page-head--> markers; page head tags will stack up across navigations",
				Fix:     "wrap @templ.Raw(head) in <!--page-head--> and <!--/page-head-->"})
		}
		if !strings.Contains(body, "howl-client") {
			level, message := "warning", "the shell publishes howl-wasm-routes; howl-client also carries the client data endpoint and the dev reload endpoint"
			if !strings.Contains(body, "howl-wasm-routes") {
				level, message = "error", "the shell publishes no client config; the client runtime cannot know which routes need wasm"
			}
			out = append(out, Diagnostic{
				File: f.Rel, Rule: "shell-client-config", Level: level, Message: message,
				Fix: `@templ.JSONScript("howl-client", router.ClientConfig(ctx))`,
			})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Reactivity and lifecycle
//
// These are the rules the browser half of the framework relies on, and every
// one of them fails silently: a leaked effect still works, it just also fires
// against a DOM that was thrown away, and it does so once more per visit. The
// server ones are worse — a signal written from a request handler is a data
// race that shows up as one user seeing another's data, under load, later.
// ---------------------------------------------------------------------------

var (
	funcMountRe   = regexp.MustCompile(`(?m)^func\s+Mount\s*\(\s*\)`)
	funcUnmountRe = regexp.MustCompile(`(?m)^func\s+Unmount\s*\(\s*\)`)
	registersRe   = regexp.MustCompile(`signal\.(Effect|Watch|WatchAny)\(|\.On\("`)
	bareEffectRe  = regexp.MustCompile(`(?m)^\s*signal\.(Effect|Watch|WatchAny)\(`)
	// A statement that is only a .On( call: the release func it returns went
	// nowhere, so the js.Func behind it can never be released.
	bareListenerRe = regexp.MustCompile(`(?m)^\s*[\w.()\[\]"' -]*\.On\("`)
	importSignalRe = regexp.MustCompile(`"[^"]*howl-go/core/signal"`)
	importJSRe     = regexp.MustCompile(`"syscall/js"`)
	notPortableRe  = regexp.MustCompile(`"(database/sql|os|os/exec|net/http/httptest)"`)
)

func lintReactivity(root string, files []sourceFile) []Diagnostic {
	var out []Diagnostic
	for _, f := range files {
		// Generated files restate the source they came from — a docs page
		// carries every code sample in this file's own documentation, and
		// linting those is how a rule reports itself.
		if generatedRe.Match(f.Body) {
			continue
		}
		inPages := strings.HasPrefix(f.Rel, "client/pages") || strings.Contains(f.Rel, "/client/pages/")
		inStore := strings.Contains(f.Rel, "client/store/")
		isServer := strings.HasPrefix(f.Rel, "server/") || strings.Contains(f.Rel, "/server/") ||
			strings.HasSuffix(f.Rel, ".api.go")

		if inPages && funcMountRe.Match(f.Body) {
			// Mount that registers nothing needs no Unmount — the metrics page
			// in the toy app logs and fetches, and pairing it would be
			// ceremony. Mount that subscribes is the leak: one more live
			// listener or effect per visit, all of them firing at a DOM that
			// no longer exists.
			if registersRe.Match(f.Body) && !funcUnmountRe.Match(f.Body) {
				line, _ := findLine(f.Body, funcMountRe)
				out = append(out, Diagnostic{
					File: f.Rel, Line: line, Rule: "mount-without-unmount", Level: "error",
					Message: "Mount registers an effect, a watcher or a DOM listener and there is no Unmount; every visit to this route adds another one",
					Fix:     "keep the stop funcs in package-level vars and call them in func Unmount()",
				})
			}
			if line, ok := findLine(f.Body, bareEffectRe); ok {
				out = append(out, Diagnostic{
					File: f.Rel, Line: line, Rule: "effect-not-released", Level: "error",
					Message: "the stop func from signal.Effect/Watch is discarded, so this subscription can never be released",
					Fix:     "stopEffect = signal.Effect(repaint), then call stopEffect() in Unmount",
				})
			}
			if line, ok := findLine(f.Body, bareListenerRe); ok {
				out = append(out, Diagnostic{
					File: f.Rel, Line: line, Rule: "listener-not-released", Level: "warning",
					Message: "the release func from dom.On is discarded; a js.Func is held alive from the JS side until it is released, so this leaks one Go closure per visit",
					Fix:     "release = append(release, el.On(\"click\", fn)), then dom.Off(release...) in Unmount",
				})
			}
			if line, ok := findLine(f.Body, importJSRe); ok {
				out = append(out, Diagnostic{
					File: f.Rel, Line: line, Rule: "page-imports-syscall-js", Level: "error",
					Message: "a page imports syscall/js; the page package is compiled for the server too, where that does not build",
					Fix:     "use core/dom, which is real under GOOS=js and no-ops everywhere else",
				})
			}
		}

		// The server must never write a signal. Signals are package-level, so
		// two concurrent requests would be writing one variable — and the
		// browser's renderer would then read whatever the last request left.
		if isServer && !inStore {
			if line, ok := findLine(f.Body, importSignalRe); ok {
				out = append(out, Diagnostic{
					File: f.Rel, Line: line, Rule: "server-imports-signal", Level: "error",
					Message: "server code imports core/signal; signals are package-level, so a request handler writing one races every other request",
					Fix:     "keep request state in ctx (core/state, Config.Data) and mirror into signals only on the browser's store instance",
				})
			}
		}

		// A store package is imported by pages, and pages compile for wasm.
		// Anything here that only exists on a server takes the whole browser
		// build down with it — at link time, in a message about a package the
		// author never mentioned.
		if inStore {
			if line, ok := findLine(f.Body, notPortableRe); ok {
				out = append(out, Diagnostic{
					File: f.Rel, Line: line, Rule: "store-not-portable", Level: "warning",
					Message: "the store imports a server-only package; this package is compiled into the wasm build through the pages that import it",
					Fix:     "keep the store to domain types and pure logic — do the I/O in an endpoint and hand the result over as a Snapshot",
				})
			}
			if line, ok := findLine(f.Body, importDBRe); ok {
				out = append(out, Diagnostic{
					File: f.Rel, Line: line, Rule: "store-not-portable", Level: "warning",
					Message: "the store imports db; database/sql cannot go into the wasm build the pages produce",
					Fix:     "load documents in an endpoint and hand the store a Snapshot",
				})
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Component references
//
// `undefined: components.SettingsShell` is the error this framework produces
// most, and it is the one it reports worst. templ emits no //line directives,
// so the compiler names ui_templ.go:412 — a generated file, at a line that
// exists in no source, after fsroutes and templ generate have both run. The
// author sees a failure three steps from the `@components.SettingsShell()`
// they typed.
//
// The reference itself is resolvable without a compiler: a .templ file
// declares its imports, and every package in the module declares its exported
// names. Doing it here reports the mistake at the line that made it, before
// anything is generated.
// ---------------------------------------------------------------------------

// pkgInfo is what one directory declares, from the point of view of somebody
// calling into it.
type pkgInfo struct {
	Path    string         // import path
	Name    string         // the package clause, which is not always the directory
	Symbols map[string]int // exported name -> parameter count, or -1 when it cannot be counted
}

var (
	// Unexported too: a component used only inside its own package is the
	// normal case for a layout's nav link, and reporting it would be a rule
	// that fires on the framework's own documentation site.
	templDeclRe  = regexp.MustCompile(`(?m)^templ\s+([A-Za-z_]\w*)\s*\(([^)]*)\)`)
	goFuncDeclRe = regexp.MustCompile(`(?m)^func\s+([A-Za-z_]\w*)\s*(?:\[[^\]]*\])?\(([^)]*)\)`)
	pkgClauseRe  = regexp.MustCompile(`(?m)^package\s+(\w+)`)
	// A component call: @Name( or @alias.Name(. The leading boundary keeps an
	// email address in body text from reading as a call.
	callRe = regexp.MustCompile(`(^|[\s(){}>,;])@([A-Za-z_]\w*)(?:\.([A-Za-z_]\w*))?\s*\(`)
)

// indexPackages reads what every directory in the module declares. It is a
// source scan, not a type check: it has to work on .templ files, which are not
// Go, and before templ generate has turned them into files that are.
func indexPackages(root string, files []sourceFile) map[string]*pkgInfo {
	module := modulePath(root)
	byDir := map[string]*pkgInfo{}

	for _, f := range files {
		dir := path.Dir(f.Rel)
		if dir == "." {
			dir = ""
		}
		info := byDir[dir]
		if info == nil {
			importPath := module
			if dir != "" {
				importPath = module + "/" + dir
			}
			info = &pkgInfo{Path: importPath, Symbols: map[string]int{}}
			byDir[dir] = info
		}
		if info.Name == "" {
			if m := pkgClauseRe.FindSubmatch(f.Body); m != nil {
				info.Name = string(m[1])
			}
		}

		// Generated files are indexed too. They declare the same names as the
		// source they came from, and a project mid-pipeline may have only one
		// of the two on disk — missing a declaration would report a call that
		// is perfectly correct.
		if strings.HasSuffix(f.Rel, ".go") {
			indexGoDecls(f, info.Symbols)
			continue
		}
		for _, m := range templDeclRe.FindAllSubmatch(f.Body, -1) {
			info.Symbols[string(m[1])] = countParams(string(m[2]))
		}
		for _, m := range goFuncDeclRe.FindAllSubmatch(f.Body, -1) {
			info.Symbols[string(m[1])] = countParams(string(m[2]))
		}
	}

	byPath := make(map[string]*pkgInfo, len(byDir))
	for _, info := range byDir {
		byPath[info.Path] = info
	}
	return byPath
}

// indexGoDecls uses the parser rather than a regex, because a .go file is
// real Go: grouped var blocks, generics and methods all have to come out
// right, and a missed declaration here is a false report on working code.
func indexGoDecls(f sourceFile, out map[string]int) {
	file, err := parser.ParseFile(token.NewFileSet(), f.Rel, f.Body, parser.SkipObjectResolution)
	if err != nil {
		return // mid-edit, or generated but not yet written: index nothing
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil {
				continue // a method is not callable as @Name()
			}
			out[d.Name.Name] = fieldCount(d.Type.Params)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch sp := spec.(type) {
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						// A var may hold a func; its arity is not worth
						// chasing through the type.
						out[n.Name] = -1
					}
				case *ast.TypeSpec:
					out[sp.Name.Name] = -1
				}
			}
		}
	}
}

func fieldCount(list *ast.FieldList) int {
	if list == nil {
		return 0
	}
	n := 0
	for _, f := range list.List {
		if _, variadic := f.Type.(*ast.Ellipsis); variadic {
			return -1
		}
		if len(f.Names) == 0 {
			n++
			continue
		}
		n += len(f.Names)
	}
	return n
}

// countParams counts declared parameters. `a, b string, c int` is three: the
// names are comma-separated whether or not they share a type.
func countParams(sig string) int {
	sig = strings.TrimSpace(sig)
	if sig == "" {
		return 0
	}
	if strings.Contains(sig, "...") {
		return -1
	}
	return len(splitTopLevel(sig))
}

func lintComponents(root string, files []sourceFile) []Diagnostic {
	index := indexPackages(root, files)
	var out []Diagnostic

	for _, f := range files {
		if !strings.HasSuffix(f.Rel, ".templ") || generatedRe.Match(f.Body) {
			continue
		}
		body := string(f.Body)
		imports := templImports(body, index)
		dir := path.Dir(f.Rel)
		if dir == "." {
			dir = ""
		}
		module := modulePath(root)
		self := module
		if dir != "" {
			self = module + "/" + dir
		}

		for _, m := range callRe.FindAllStringSubmatchIndex(body, -1) {
			// Submatch indices: 1 is the leading boundary, 2 is the name or
			// the alias, 3 is the name when the call is qualified.
			name := body[m[4]:m[5]]
			target, qualified := index[self], ""
			if m[6] >= 0 { // @alias.Name(
				qualified = name
				alias := name
				name = body[m[6]:m[7]]
				importPath, ok := imports[alias]
				if !ok {
					// Not an import: a local variable holding a component,
					// which is legal and cannot be resolved from here.
					continue
				}
				target, ok = index[importPath]
				if !ok {
					continue // outside this module; the compiler owns it
				}
				qualified += "." + name
			} else {
				qualified = name
			}
			if target == nil {
				continue
			}
			line := strings.Count(body[:m[4]], "\n") + 1

			if _, ok := target.Symbols[name]; !ok {
				out = append(out, Diagnostic{
					File: f.Rel, Line: line, Rule: "unknown-component", Level: "error",
					Message: fmt.Sprintf("%s is not declared in %s", qualified, target.Path),
					Fix:     nearest(name, target.Symbols, target.Path, target.Path != self),
				})
				continue
			}
			// Declared, but not from here. Go's export rule is the case of the
			// first letter, and templ inherits it.
			if target.Path != self && !token.IsExported(name) {
				out = append(out, Diagnostic{
					File: f.Rel, Line: line, Rule: "unexported-component", Level: "error",
					Message: fmt.Sprintf("%s is declared in %s but is not exported", qualified, target.Path),
					Fix:     "capitalise it at the declaration, or move the caller into that package",
				})
				continue
			}

			want := target.Symbols[name]
			if want < 0 {
				continue // variadic, or a value whose arity is not knowable here
			}
			got, ok := countArgs(body[m[1]-1:])
			if !ok || got == want {
				continue
			}
			out = append(out, Diagnostic{
				File: f.Rel, Line: line, Rule: "component-arity", Level: "error",
				Message: fmt.Sprintf("%s takes %s, called with %d", qualified, plural(want, "argument"), got),
				Fix:     "pages take no arguments — everything reaches them through ctx; shared components declare theirs",
			})
		}
	}
	return out
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// nearest suggests the declared name closest to the one that was written. A
// typo and a wrong package look identical in the compiler's message; here the
// two can be told apart.
func nearest(name string, symbols map[string]int, importPath string, exportedOnly bool) string {
	best, bestScore := "", 0
	for candidate := range symbols {
		if exportedOnly && !token.IsExported(candidate) {
			continue
		}
		score := commonPrefix(strings.ToLower(candidate), strings.ToLower(name))
		if score > bestScore && score >= 3 {
			best, bestScore = candidate, score
		}
	}
	if best != "" {
		return fmt.Sprintf("did you mean %s.%s?", path.Base(importPath), best)
	}
	names := visible(symbols, exportedOnly)
	if len(names) == 0 {
		return fmt.Sprintf("%s declares no components — check the import path", importPath)
	}
	return fmt.Sprintf("%s declares: %s", importPath, strings.Join(names, ", "))
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func visible(symbols map[string]int, exportedOnly bool) []string {
	out := make([]string, 0, len(symbols))
	for name := range symbols {
		if exportedOnly && !token.IsExported(name) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	if len(out) > 12 {
		out = append(out[:12], "…")
	}
	return out
}

// templImports reads the import block of a .templ file. templ's import syntax
// is Go's, but the file around it is not Go, so this cannot go through the
// parser.
func templImports(body string, index map[string]*pkgInfo) map[string]string {
	out := map[string]string{}
	add := func(alias, importPath string) {
		if alias == "" {
			// The default alias is the package clause, not the directory:
			// client/api may well be `package apiclient`, and guessing from
			// the path would leave the call unresolved.
			alias = path.Base(importPath)
			if info, ok := index[importPath]; ok && info.Name != "" {
				alias = info.Name
			}
		}
		if alias == "_" || alias == "." {
			return
		}
		out[alias] = importPath
	}
	for _, m := range importLineRe.FindAllStringSubmatch(body, -1) {
		add(strings.TrimSpace(m[1]), m[2])
	}
	return out
}

var importLineRe = regexp.MustCompile(`(?m)^\s*(?:import\s+)?([A-Za-z_]\w*\s+)?"([^"]+)"`)

// countArgs counts the top-level arguments of a call whose opening paren is at
// s[0]. It tracks nesting and every kind of Go string literal, because an
// argument may perfectly well contain a comma — a struct literal, a JSON blob,
// a func literal. When the call does not close, it reports that it could not
// tell rather than guessing.
func countArgs(s string) (int, bool) {
	inner, ok := balanced(s)
	if !ok {
		return 0, false
	}
	if strings.TrimSpace(inner) == "" {
		return 0, true
	}
	return len(splitTopLevel(inner)), true
}

// balanced returns the text between s[0] and its matching close paren.
func balanced(s string) (string, bool) {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 {
				return s[1:i], true
			}
		case '"', '\'', '`':
			j, ok := skipString(s, i)
			if !ok {
				return "", false
			}
			i = j
		}
	}
	return "", false
}

func skipString(s string, i int) (int, bool) {
	quote := s[i]
	for j := i + 1; j < len(s); j++ {
		if s[j] == '\\' && quote != '`' {
			j++
			continue
		}
		if s[j] == quote {
			return j, true
		}
		if s[j] == '\n' && quote != '`' {
			return 0, false // an unterminated literal: give up rather than guess
		}
	}
	return 0, false
}

// splitTopLevel splits on commas that are not inside brackets or a string.
func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case '"', '\'', '`':
			if j, ok := skipString(s, i); ok {
				i = j
			}
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// ---------------------------------------------------------------------------
// Cost in the wrong place
//
// None of these are wrong answers. They are the right answer computed once per
// row, once per request, or once per keystroke, which is why nothing ever fails
// and the page is simply slower than it reads. The compiler has no opinion
// about any of them, so they survive review by looking exactly like working
// code — because they are working code.
//
// Everything here is a warning. A judgement call that fails a build is a rule
// people delete rather than argue with.
// ---------------------------------------------------------------------------

func lintPerformance(root string, files []sourceFile) []Diagnostic {
	var out []Diagnostic
	for _, f := range files {
		if generatedRe.Match(f.Body) || strings.HasSuffix(f.Rel, "_test.go") {
			continue
		}
		body := f.Body
		if strings.HasSuffix(f.Rel, ".templ") {
			// The lifecycle code that matters most — Mount, repaint, the
			// handlers they bind — lives in .templ files, next to markup that
			// is not Go. Blanking the templ blocks leaves the Go, at its
			// original line numbers.
			body = goPortion(f.Body)
		} else if !strings.HasSuffix(f.Rel, ".go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, f.Rel, body, parser.SkipObjectResolution)
		if err != nil {
			continue // mid-edit; the compiler will say so more precisely
		}
		usesDB := importDBRe.Match(f.Body)

		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}
			// main and init run once. Everything else may run per request,
			// per render or per row.
			once := fn.Recv == nil && (fn.Name.Name == "main" || fn.Name.Name == "init")
			// A test helper is not a hot path, and a table-driven test is a
			// loop of deliberate single queries. Reported, it would only teach
			// people that these rules are noise.
			if takesTestingT(fn) {
				return false
			}
			at := func(pos token.Pos) int { return fset.Position(pos).Line }

			ast.Inspect(fn.Body, func(n ast.Node) bool {
				// A pattern built from a value cannot be hoisted — it is not
				// known until the call. Only a constant one is the mistake
				// this rule is about; the dynamic case is caught below, and
				// only when it is inside a loop.
				if call, ok := n.(*ast.CallExpr); ok && !once && isRegexpCompile(call) && constantPattern(call) {
					out = append(out, Diagnostic{
						File: f.Rel, Line: at(call.Pos()), Rule: "regexp-recompiled", Level: "warning",
						Message: "this regexp is compiled every time " + fn.Name.Name + " runs; compiling is orders of magnitude dearer than matching",
						Fix:     "hoist it to a package-level var: var thingRe = regexp.MustCompile(`…`)",
					})
				}
				body, isLoop := loopBody(n)
				if !isLoop {
					return true
				}
				out = append(out, loopDiagnostics(f.Rel, body, at, usesDB)...)
				return true
			})
			return false // the outer Inspect already reached every function
		})
	}
	return out
}

// loopDiagnostics reports the things that are only a problem because they are
// inside a loop.
func loopDiagnostics(rel string, body *ast.BlockStmt, at func(token.Pos) int, usesDB bool) []Diagnostic {
	var out []Diagnostic
	declared := declaredIn(body)

	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.DeferStmt:
			out = append(out, Diagnostic{
				File: rel, Line: at(node.Pos()), Rule: "defer-in-loop", Level: "warning",
				Message: "a defer runs when the function returns, not when the iteration ends; these accumulate for the whole loop",
				Fix:     "close it at the end of the iteration, or move the body into its own function",
			})

		case *ast.AssignStmt:
			// A string built by repeated += reallocates and copies everything
			// written so far, once per iteration. An accumulator declared
			// inside the loop is not that — it is one short string per row.
			if node.Tok != token.ADD_ASSIGN || len(node.Lhs) != 1 || !looksLikeString(node.Rhs) {
				return true
			}
			target, ok := node.Lhs[0].(*ast.Ident)
			if !ok || declared[target.Name] {
				return true
			}
			out = append(out, Diagnostic{
				File: rel, Line: at(node.Pos()), Rule: "concat-in-loop", Level: "warning",
				Message: "building a string with += in a loop copies everything written so far on every iteration",
				Fix:     "use a strings.Builder, or render into the io.Writer you were given",
			})

		case *ast.CallExpr:
			switch {
			case isRegexpCompile(node) && !constantPattern(node):
				out = append(out, Diagnostic{
					File: rel, Line: at(node.Pos()), Rule: "regexp-recompiled", Level: "warning",
					Message: "a regexp built and compiled on every iteration; compiling costs orders of magnitude more than matching",
					Fix:     "compile it once outside the loop, cache it by the value it varies on, or match without a regexp",
				})
			case isHTTPRequest(node):
				out = append(out, Diagnostic{
					File: rel, Line: at(node.Pos()), Rule: "request-in-loop", Level: "warning",
					Message: "one HTTP round trip per iteration; in the browser that is one network latency per item, serially",
					Fix:     "ask for the whole set in one request, or run them concurrently and collect the results",
				})
			case usesDB && isDocumentRead(node):
				out = append(out, Diagnostic{
					File: rel, Line: at(node.Pos()), Rule: "query-in-loop", Level: "warning",
					Message: "one query per iteration — the N+1 the filter grammar exists to avoid",
					Fix:     `Find(ctx, db.Query{Where: db.In("id", ids)}) once, then index the result by id`,
				})
			}
		}
		return true
	})
	return out
}

// takesTestingT reports a func that receives *testing.T or *testing.B — a test
// or a helper for one, wherever it happens to live.
func takesTestingT(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil {
		return false
	}
	for _, param := range fn.Type.Params.List {
		star, ok := param.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		if sel, ok := star.X.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "testing" {
				return true
			}
		}
	}
	return false
}

func loopBody(n ast.Node) (*ast.BlockStmt, bool) {
	switch loop := n.(type) {
	case *ast.ForStmt:
		return loop.Body, true
	case *ast.RangeStmt:
		return loop.Body, true
	}
	return nil, false
}

// declaredIn reports the names declared inside this block, so an accumulator
// that lives for one iteration can be told from one that lives for the loop.
func declaredIn(body *ast.BlockStmt) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			if node.Tok == token.DEFINE {
				for _, lhs := range node.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						out[id.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			for _, name := range node.Names {
				out[name.Name] = true
			}
		}
		return true
	})
	return out
}

// looksLikeString is a heuristic, because this runs without type information.
// It only has to be right about the shape that matters: text being appended.
func looksLikeString(rhs []ast.Expr) bool {
	found := false
	for _, e := range rhs {
		ast.Inspect(e, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.BasicLit:
				if node.Kind == token.STRING {
					found = true
				}
			case *ast.CallExpr:
				if name, ok := calleeName(node); ok {
					switch name {
					case "fmt.Sprintf", "fmt.Sprint", "string", "strconv.Itoa", "strconv.Quote":
						found = true
					}
				}
			}
			return !found
		})
	}
	return found
}

func isRegexpCompile(call *ast.CallExpr) bool {
	name, ok := calleeName(call)
	if !ok {
		return false
	}
	switch name {
	case "regexp.MustCompile", "regexp.Compile", "regexp.MustCompilePOSIX", "regexp.CompilePOSIX":
		return true
	}
	return false
}

// constantPattern reports whether every part of the pattern is a literal, so
// the call could be hoisted to a package-level var as it stands.
func constantPattern(call *ast.CallExpr) bool {
	if len(call.Args) == 0 {
		return false
	}
	var literal func(ast.Expr) bool
	literal = func(e ast.Expr) bool {
		switch node := e.(type) {
		case *ast.BasicLit:
			return node.Kind == token.STRING
		case *ast.BinaryExpr:
			return node.Op == token.ADD && literal(node.X) && literal(node.Y)
		}
		return false
	}
	return literal(call.Args[0])
}

func isHTTPRequest(call *ast.CallExpr) bool {
	name, ok := calleeName(call)
	if !ok {
		return false
	}
	switch name {
	case "http.Get", "http.Post", "http.Head", "http.PostForm":
		return true
	}
	// c.Do(req) on anything: an http.Client is the only thing in ordinary use
	// with that name and one argument.
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Do" && len(call.Args) == 1
}

// isDocumentRead spots a db.Service call by its shape: a method with a name
// from the small, fixed set the service exposes, taking ctx first. It is only
// consulted for files that import db at all.
func isDocumentRead(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || len(call.Args) == 0 {
		return false
	}
	switch sel.Sel.Name {
	case "Get", "Find", "Create", "Patch", "PatchFields", "Delete", "Count", "PatchWhere":
	default:
		return false
	}
	first, ok := call.Args[0].(*ast.Ident)
	return ok && (first.Name == "ctx" || first.Name == "c")
}

// calleeName renders pkg.Fn or Fn for a call, and reports false for anything
// deeper — a chained call is not something these rules match on.
func calleeName(call *ast.CallExpr) (string, bool) {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name, true
	case *ast.SelectorExpr:
		if x, ok := fn.X.(*ast.Ident); ok {
			return x.Name + "." + fn.Sel.Name, true
		}
	}
	return "", false
}

// goPortion blanks out every templ, css and script block, leaving the Go
// declarations around them — the imports, Mount, Unmount, repaint, the helper
// funcs — at their original line numbers, so a diagnostic still points at the
// line the author is looking at.
func goPortion(body []byte) []byte {
	src := string(body)
	out := []byte(src)
	for _, m := range blockStartRe.FindAllStringSubmatchIndex(src, -1) {
		open := strings.IndexByte(src[m[0]:], '{')
		if open < 0 {
			continue
		}
		open += m[0]
		inner, ok := balanced(src[open:])
		if !ok {
			continue
		}
		blank(out, m[0], open+len(inner)+2)
	}
	return out
}

var blockStartRe = regexp.MustCompile(`(?m)^(templ|css|script)\s+\w+\s*\(`)

// blank replaces a span with spaces, keeping every newline, so byte offsets
// and line numbers both survive.
func blank(b []byte, from, to int) {
	if to > len(b) {
		to = len(b)
	}
	for i := from; i < to; i++ {
		if b[i] != '\n' {
			b[i] = ' '
		}
	}
}

// A db collection's Defaults must be on the pointer receiver. On a value
// receiver it still satisfies the interface — a value method is in the
// pointer's method set — so the service calls it, it mutates a copy, and the
// defaults silently never reach storage. Nothing fails; the field is just
// empty forever.
//
// Validate is fine either way: it only reads.
//
// This is the one rule that parses instead of matching. A regex cannot tell a
// declaration from a string literal, and the first thing it flagged was the
// deliberately-broken fixture inside this package's own tests. go/parser needs
// no build and no type information, so the constraint that keeps the rest of
// this file regex-based does not apply.
func lintCollections(root string, files []sourceFile) []Diagnostic {
	var out []Diagnostic
	for _, f := range files {
		if !strings.HasSuffix(f.Rel, ".go") || !bytes.Contains(f.Body, []byte("db.Doc")) {
			continue
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, f.Rel, f.Body, 0)
		if err != nil {
			continue // not our problem; the compiler will say so first
		}

		documents := map[string]bool{}
		for _, decl := range parsed.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, field := range st.Fields.List {
					if len(field.Names) > 0 {
						continue // named, so not embedded
					}
					if sel, ok := field.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Doc" {
						if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "db" {
							documents[ts.Name.Name] = true
						}
					}
				}
			}
		}
		if len(documents) == 0 {
			continue
		}

		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "Defaults" || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			recv := fn.Recv.List[0]
			ident, byValue := recv.Type.(*ast.Ident)
			if !byValue || !documents[ident.Name] {
				continue
			}
			name := "d"
			if len(recv.Names) > 0 {
				name = recv.Names[0].Name
			}
			out = append(out, Diagnostic{
				File: f.Rel, Line: fset.Position(fn.Pos()).Line,
				Rule: "collection-value-receiver", Level: "error",
				Message: "Defaults is on a value receiver, so it mutates a copy — the service calls it and the defaults are lost",
				Fix:     "func (" + name + " *" + ident.Name + ") Defaults()",
			})
		}
	}
	return out
}

// stripHTMLComments blanks out <!-- … --> while keeping the line count, so a
// diagnostic's line number still points at the right place.
func stripHTMLComments(body string) string {
	var b strings.Builder
	for {
		open := strings.Index(body, "<!--")
		if open < 0 {
			b.WriteString(body)
			return b.String()
		}
		closeAt := strings.Index(body[open:], "-->")
		if closeAt < 0 {
			b.WriteString(body[:open])
			return b.String()
		}
		b.WriteString(body[:open])
		b.WriteString(strings.Repeat("\n", strings.Count(body[open:open+closeAt], "\n")))
		body = body[open+closeAt+3:]
	}
}

// The endpoint rules. These are the ones the type system cannot state: an
// endpoint declaring a typed query and then reading the raw one is legal Go and
// completely defeats the layer.
func lintEndpoints(root string, files []sourceFile) []Diagnostic {
	var out []Diagnostic
	declaresRoles, wiresAuthorize := "", false

	for _, f := range files {
		if authorizeRe.Match(f.Body) && !strings.HasSuffix(f.Rel, ".api.go") {
			wiresAuthorize = true
		}
		if !strings.HasSuffix(f.Rel, ".api.go") {
			continue
		}
		if line, ok := findLine(f.Body, rawQueryRe); ok {
			out = append(out, Diagnostic{
				File: f.Rel, Line: line, Rule: "typed-query", Level: "warning",
				Message: "the handler reads the raw request query; the endpoint already declares a typed one",
				Fix:     "read r.Query.Field — and add the field to the query type if it is missing",
			})
		}
		if rolesRe.Match(f.Body) && declaresRoles == "" {
			declaresRoles = f.Rel
		}
		if n := len(defineRe.FindAll(f.Body, -1)); n > 1 {
			out = append(out, Diagnostic{
				File: f.Rel, Rule: "one-endpoint-per-file", Level: "error",
				Message: fmt.Sprintf("%d api.Define calls in one file; the file name is the URL, so it can only describe one", n),
				Fix:     "split the extra endpoints into their own *.api.go files",
			})
		}
		out = append(out, lintJSONTags(f)...)
	}

	if declaresRoles != "" && !wiresAuthorize {
		out = append(out, Diagnostic{
			File: declaresRoles, Rule: "roles-unwired", Level: "error",
			Message: "an endpoint declares Roles but nothing in this module sets api.Config.Authorize; every caller would be let through",
			Fix:     "pass api.Config{Authorize: …} to api.Register — roles are the application's to interpret",
		})
	}
	return out
}

// A response struct without json tags ships Go field names on the wire, which
// makes the API shape an accident of refactoring.
func lintJSONTags(f sourceFile) []Diagnostic {
	var out []Diagnostic
	lines := strings.Split(string(f.Body), "\n")
	inStruct := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "type ") && strings.HasSuffix(trimmed, "struct {") {
			inStruct = true
			continue
		}
		if inStruct && trimmed == "}" {
			inStruct = false
			continue
		}
		if !inStruct || trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		// A field carrying any tag has been thought about — `query:"limit"` on
		// an input struct is not a missing json tag, and flagging it taught the
		// rule to cry wolf on every endpoint that takes a filter.
		if m := structFieldRe.FindStringSubmatch(line); m != nil && !strings.Contains(line, "`") {
			out = append(out, Diagnostic{
				File: f.Rel, Line: i + 1, Rule: "json-tag", Level: "warning",
				Message: fmt.Sprintf("field %s has no json tag; the wire name will follow the Go name", m[1]),
				Fix:     fmt.Sprintf("add `json:%q`", strings.ToLower(m[1])),
			})
		}
	}
	return out
}

// A generated file that lost its header was edited by hand, and the next
// generator run will silently throw the edit away.
func lintGenerated(root string, files []sourceFile) []Diagnostic {
	var out []Diagnostic
	for _, f := range files {
		base := filepath.Base(f.Rel)
		isGenerated := strings.HasSuffix(base, "_templ.go") || strings.HasSuffix(base, "_gen.go")
		if !isGenerated || generatedRe.Match(f.Body) {
			continue
		}
		out = append(out, Diagnostic{
			File: f.Rel, Rule: "edited-generated", Level: "warning",
			Message: "this looks like a generated file but has no \"Code generated\" header; edits here are overwritten on the next build",
			Fix:     "edit the .templ or *.api.go source and re-run the generators",
		})
	}
	return out
}

// lintBuild runs what the dev server runs. A convention the generators enforce
// — an unknown file-name modifier, two endpoints in one file — surfaces here
// with the generator's own wording rather than being reimplemented.
func lintBuild(root string) []Diagnostic {
	var out []Diagnostic
	steps := []struct {
		rule, name string
		args       []string
	}{
		{"generate", "go", []string{"generate", "./..."}},
		{"build", "go", []string{"build", "./..."}},
		{"vet", "go", []string{"vet", "./..."}},
	}
	for _, step := range steps {
		cmd := exec.Command(step.name, step.args...)
		cmd.Dir = root
		if output, err := cmd.CombinedOutput(); err != nil {
			out = append(out, Diagnostic{
				File: ".", Rule: step.rule, Level: "error",
				Message: strings.TrimSpace(string(output)),
			})
			// A failed generate makes the build's errors noise; stop at the
			// first real failure rather than reporting its consequences.
			break
		}
	}
	return out
}

// ---------------------------------------------------------------------------

type sourceFile struct {
	Rel  string
	Body []byte
}

func sourceFiles(root string) []sourceFile {
	var out []sourceFile
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error { //nolint:errcheck
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".howl", "node_modules", "dist", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(d.Name()) {
		case ".go", ".templ":
		default:
			return nil
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		out = append(out, sourceFile{Rel: filepath.ToSlash(rel), Body: body})
		return nil
	})
	return out
}

func findLine(body []byte, re *regexp.Regexp) (int, bool) {
	loc := re.FindIndex(body)
	if loc == nil {
		return 0, false
	}
	return strings.Count(string(body[:loc[0]]), "\n") + 1, true
}

// defineRe is the same shape fsapis looks for, kept here so `check` needs no
// build step to count endpoints in a file.
var defineRe = regexp.MustCompile(`(?m)^var\s+[A-Z]\w*\s*=\s*api\.Define\(`)
