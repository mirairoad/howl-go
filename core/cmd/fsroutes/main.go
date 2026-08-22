// Command fsroutes turns a directory tree into a Go route table at build time.
//
//	client/pages/index.templ                   ->  /
//	client/pages/about/index.templ             ->  /about
//	client/pages/dashboard/metrics/index.templ ->  /dashboard/metrics
//	client/pages/blog/article_id.dyn.templ     ->  /blog/{article_id}
//
// Every .templ file is a route except two reserved basenames:
//
//	layout.templ         wraps its directory and everything below it
//	app.templ            the document shell, root only
//	*.component.templ    colocated markup, never a route
//
// These borrow howl's convention but WITHOUT the leading underscore: the Go
// toolchain ignores every file and directory whose name begins with "_", so
// `_layout.templ` would compile to `_layout_templ.go` and then be invisible to
// the compiler. Reserved basenames are the portable form.
//
// Behaviour is encoded in dot-separated modifiers on the file name:
//
//	index.templ                 a plain server-rendered route
//	index.client.templ          also renderable in the browser by the wasm build
//	article_id.dyn.templ        /{article_id} — a path parameter
//	article_id.dyn.client.templ both, in either order
//	print.bare.templ            skips the layout chain, keeps the document shell
//	embed.raw.templ             skips both: the page's markup is the whole reply
//
// Modifiers are suffixes, not prefixes, because Go ignores every file whose
// name begins with "_": `_index.templ` compiles to `_index_templ.go`, and the
// package then reports "no Go files". Brackets are out for the same class of
// reason — Go rejects "[" and "{" in file names ("invalid input file name")
// and in import paths ("malformed import path: invalid char"), so neither
// `[article_id].templ` nor `[article_id]/` can exist in a Go module. Dots are
// legal in both, which is why they carry the convention here.
//
// A `//howl:route` directive overrides the derived pattern outright when the
// filesystem cannot express what you need.
//
// Two reserved names may sit alongside the page component in the same file:
//
//	templ Head()    merged into the document <head> — title, meta, link
//	func Mount()    runs in the browser after this page's first paint
//	func Unmount()  runs just before this page's markup is replaced
//
// Mount takes no arguments and touches the DOM through client/dom, so the file
// needs no build tag and still compiles for the server.
//
// A route's component is whichever zero-argument `templ Name()` (other than the
// reserved Head) its file declares first, so files sharing a directory (and therefore a Go package)
// just give their components different names. Per-route metadata rides on
// directive comments rather than consts, for the same reason: two `const Title`
// in one package would not compile.
//
//	templ Article() { … }
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// directive returns the single capture of a //howl: comment, or "".
func directive(re *regexp.Regexp, src []byte) string {
	if m := re.FindSubmatch(src); m != nil {
		return string(m[1])
	}
	return ""
}

type route struct {
	Pattern   string
	Label     string
	Head      bool
	Mount     bool
	Unmount   bool
	Client    bool
	Data      string // //howl:data — this route's own client-data endpoint
	Bare      bool   // no layout chain
	Raw       bool   // no layout chain, no document shell
	File      string // relative to pagesDir, for logging
	Dir       string // directory relative to pagesDir; "" for root
	Component string // the templ func declared in that file
	Alias     string // import alias; "" when it lives in the root package
	Import    string
	Layouts   []string
}

var (
	pkgRe     = regexp.MustCompile(`(?m)^package\s+([A-Za-z_]\w*)`)
	compRe    = regexp.MustCompile(`(?m)^templ\s+([A-Z]\w*)\s*\(\s*\)\s*\{`)
	headRe    = regexp.MustCompile(`(?m)^templ\s+Head\s*\(\s*\)\s*\{`)
	mountRe   = regexp.MustCompile(`(?m)^func\s+Mount\s*\(\s*\)\s*\{`)
	unmountRe = regexp.MustCompile(`(?m)^func\s+Unmount\s*\(\s*\)\s*\{`)
	// Directives attach to the file, not to a Go symbol, so several routes can
	// share one package without colliding.
	navRe   = regexp.MustCompile(`(?m)^//howl:nav\s*$`)
	routeRe = regexp.MustCompile(`(?m)^//howl:route\s+(\S+)\s*$`)
	// A `.client` route's own data endpoint. Without it every browser-rendered
	// route shares one payload, which is only workable while they all want the
	// same thing.
	dataRe = regexp.MustCompile(`(?m)^//howl:data\s+(\S+)\s*$`)
)

const (
	layoutFile = "layout.templ"
	appFile    = "app.templ"
)

func main() {
	var pagesDir, modulePath, out, routerPkg string
	flag.StringVar(&pagesDir, "dir", "client/pages", "root of the page tree")
	flag.StringVar(&modulePath, "module", "github.com/mirairoad/howl-go/examples/toy_app/client/pages", "import path of -dir")
	flag.StringVar(&out, "out", "client/pages/fsroutes_gen.go", "file to write")
	flag.StringVar(&routerPkg, "router", "github.com/mirairoad/howl-go/core/router", "import path of the router package")
	flag.Parse()
	if modulePath == "" {
		log.Fatal("fsroutes: -module is required (the import path of -dir)")
	}

	routes, rootPkg, err := crawl(pagesDir, modulePath)
	if err != nil {
		log.Fatal(err)
	}
	src, err := render(routes, rootPkg, pagesDir, routerPkg)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(out, src, 0o644); err != nil {
		log.Fatal(err)
	}
	for _, r := range routes {
		extra := ""
		if r.Head {
			extra += " +head"
		}
		if r.Mount {
			extra += " +mount"
		}
		fmt.Printf("  %-24s %s%s\n", r.Pattern, filepath.Join(pagesDir, r.File), extra)
	}
	fmt.Printf("%d routes -> %s\n", len(routes), out)
}

func crawl(pagesDir, modulePath string) ([]route, string, error) {
	var routes []route
	rootPkg := ""

	err := filepath.WalkDir(pagesDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			// The Go tool ignores these anyway; never let them become routes.
			if path != pagesDir && (strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(name) != ".templ" || name == layoutFile {
			return nil
		}
		// Components are colocated markup, not destinations. Naming one
		// foo.component.templ keeps it beside the page that uses it without
		// accidentally publishing /foo.
		if strings.HasSuffix(name, ".component.templ") {
			return nil
		}

		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(pagesDir, filepath.Dir(path))
		if err != nil {
			return err
		}
		if rel == "." {
			rel = ""
		}
		if rel == "" {
			if m := pkgRe.FindSubmatch(src); m != nil {
				rootPkg = string(m[1])
			}
		}
		if name == appFile {
			return nil // the shell, not a route
		}

		// Head is a reserved component name, so it never becomes the page.
		comp := ""
		for _, m := range compRe.FindAllSubmatch(src, -1) {
			if string(m[1]) != "Head" {
				comp = string(m[1])
				break
			}
		}
		if comp == "" {
			return fmt.Errorf("%s: no zero-argument `templ Name()` found (Head is reserved)", path)
		}

		relFile, _ := filepath.Rel(pagesDir, path)
		base := strings.TrimSuffix(name, ".templ")
		stem, mods, err := parseBase(base)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		pat, err := pattern(rel, stem, mods)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if m := routeRe.FindSubmatch(src); m != nil {
			pat = string(m[1]) // explicit override
		}
		r := route{
			Pattern:   pat,
			Label:     label(rel, stem),
			Head:      headRe.Match(src),
			Mount:     mountRe.Match(src),
			Unmount:   unmountRe.Match(src),
			Client:    mods["client"],
			Data:      directive(dataRe, src),
			Bare:      mods["bare"] || mods["raw"],
			Raw:       mods["raw"],
			File:      relFile,
			Dir:       rel,
			Component: comp,
			Import:    modulePath,
		}
		if rel != "" {
			r.Import = modulePath + "/" + filepath.ToSlash(rel)
		}
		routes = append(routes, r)
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if rootPkg == "" {
		return nil, "", fmt.Errorf("no package clause found at the root of %s", pagesDir)
	}

	// Static patterns before dynamic ones, so /blog/latest is registered ahead
	// of /blog/{article_id} and wins the lookup.
	sort.Slice(routes, func(i, j int) bool {
		di := strings.Contains(routes[i].Pattern, "{")
		dj := strings.Contains(routes[j].Pattern, "{")
		if di != dj {
			return dj
		}
		return routes[i].Pattern < routes[j].Pattern
	})

	// One alias per imported directory: several routes can share a package, and
	// two different directories can both be `package metrics`.
	aliases := map[string]string{}
	for i := range routes {
		if routes[i].Dir == "" {
			continue
		}
		a, ok := aliases[routes[i].Import]
		if !ok {
			a = fmt.Sprintf("p%d", len(aliases))
			aliases[routes[i].Import] = a
		}
		routes[i].Alias = a
	}

	// Layout chain: walk from the page's directory up to the root, collecting a
	// layout.templ at each level. Mirrors howl's discoverEngineChain; app.templ
	// is the document, applied separately, so it is not a stop condition.
	for i, r := range routes {
		if r.Bare {
			continue // .bare / .raw: the chain is dropped at generation time
		}
		var chain []string
		for dir := r.Dir; ; {
			if _, err := os.Stat(filepath.Join(pagesDir, dir, layoutFile)); err == nil {
				qual := "Layout"
				if dir != "" {
					a, ok := aliases[modulePath+"/"+filepath.ToSlash(dir)]
					if !ok {
						return nil, "", fmt.Errorf("%s has a %s but no route, so nothing imports it", filepath.Join(pagesDir, dir), layoutFile)
					}
					qual = a + ".Layout"
				}
				chain = append([]string{qual}, chain...)
			}
			if dir == "" {
				break
			}
			if dir = filepath.Dir(dir); dir == "." {
				dir = ""
			}
		}
		routes[i].Layouts = chain
	}

	return routes, rootPkg, nil
}

// knownMods are the behaviours a file name may declare. Anything else is a
// typo, and a silently ignored typo here means a route that quietly loses a
// capability — so it is an error.
//
//	dyn     the segment is a {parameter}
//	client  the wasm build renders this route in the browser
//	bare    no layout chain (howl's skipInheritedLayouts)
//	raw     no layout chain and no document shell (howl's skipAppWrapper)
var knownMods = map[string]bool{"dyn": true, "client": true, "bare": true, "raw": true}

var modList = "dyn, client, bare, raw"

// parseBase splits "article_id.dyn.client" into the stem and its modifiers.
func parseBase(base string) (string, map[string]bool, error) {
	parts := strings.Split(base, ".")
	mods := map[string]bool{}
	for _, m := range parts[1:] {
		if !knownMods[m] {
			return "", nil, fmt.Errorf("unknown modifier %q (known: %s)", m, modList)
		}
		mods[m] = true
	}
	return parts[0], mods, nil
}

// pattern builds the URL from a directory and the file's stem. The "dyn"
// modifier turns a segment into a parameter; "index" contributes no segment of
// its own. Directory names carry modifiers too, so posts/id.dyn/ works.
func pattern(relDir, stem string, mods map[string]bool) (string, error) {
	var parts []string
	if relDir != "" {
		for _, seg := range strings.Split(filepath.ToSlash(relDir), "/") {
			s, m, err := parseBase(seg)
			if err != nil {
				return "", err
			}
			if m["dyn"] {
				s = "{" + s + "}"
			}
			parts = append(parts, s)
		}
	}
	if stem != "index" {
		if mods["dyn"] {
			stem = "{" + stem + "}"
		}
		parts = append(parts, stem)
	} else if mods["dyn"] {
		return "", fmt.Errorf("index cannot be dynamic; name the file after the parameter instead")
	}
	if len(parts) == 0 {
		return "/", nil
	}
	return "/" + strings.Join(parts, "/"), nil
}

// label is nav/tab text only — the document title comes from `templ Head()`.
func label(relDir, stem string) string {
	name := stem
	if stem == "index" {
		if relDir == "" {
			return "Home"
		}
		name, _, _ = parseBase(filepath.Base(relDir))
	}
	if name == "" {
		return "Page"
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

func render(routes []route, rootPkg, pagesDir, routerPkg string) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by tools/fsroutes. DO NOT EDIT.\n")
	fmt.Fprintf(&b, "// Source of truth is the directory tree under %s.\n\n", pagesDir)
	fmt.Fprintf(&b, "package %s\n\n", rootPkg)

	imports := map[string]string{}
	for _, r := range routes {
		if r.Alias != "" {
			imports[r.Alias] = r.Import
		}
	}
	aliasList := make([]string, 0, len(imports))
	for a := range imports {
		aliasList = append(aliasList, a)
	}
	sort.Strings(aliasList)

	fmt.Fprintf(&b, "import (\n\t%q\n", routerPkg)
	for _, a := range aliasList {
		fmt.Fprintf(&b, "\t%s %q\n", a, imports[a])
	}
	fmt.Fprintf(&b, ")\n\n")

	fmt.Fprintf(&b, "// FsClientRoutes is the route table derived from the page tree.\n")
	fmt.Fprintf(&b, "func FsClientRoutes() []router.Route {\n\treturn []router.Route{\n")
	for _, r := range routes {
		page := r.Component
		if r.Alias != "" {
			page = r.Alias + "." + r.Component
		}
		layouts := "nil"
		if len(r.Layouts) > 0 {
			layouts = "[]router.Wrapper{" + strings.Join(r.Layouts, ", ") + "}"
		}
		qual := func(name string, present bool) string {
			if !present {
				return "nil"
			}
			if r.Alias != "" {
				return r.Alias + "." + name
			}
			return name
		}
		data := ""
		if r.Data != "" {
			data = fmt.Sprintf(", Data: %q", r.Data)
		}
		fmt.Fprintf(&b, "\t\t{Pattern: %q, Label: %q, Page: %s, Head: %s, Mount: %s, Unmount: %s, Layouts: %s, Client: %t, Raw: %t%s},\n",
			r.Pattern, r.Label, page, qual("Head", r.Head), qual("Mount", r.Mount),
			qual("Unmount", r.Unmount), layouts, r.Client, r.Raw, data)
	}
	fmt.Fprintf(&b, "\t}\n}\n")

	return format.Source(b.Bytes())
}
