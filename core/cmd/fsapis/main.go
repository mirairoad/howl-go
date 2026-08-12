// Command fsapis turns a directory of endpoint files into a registerable route
// table and a typed client, at build time.
//
//	server/apis/health.api.go            ->  /healthz          (Path override)
//	server/apis/summary.api.go           ->  /api/summary
//	server/apis/settings.put.api.go      ->  PUT /api/settings
//	server/apis/events/id.dyn.api.go     ->  /api/events/{id}
//
// Every `*.api.go` file declaring `var Name = api.Define(api.Spec[Q, B, R]{…})`
// becomes a route. The location on disk is the URL and the method, so nothing
// is written twice and a moved file is a moved endpoint.
//
// Go has no dynamic import, which is why this is a build step rather than a
// runtime crawl like howl's TypeScript original. The compensation is worth
// having: a missing handler is a compile error instead of a 404 found in
// production.
//
// File-name modifiers, dot-separated, the same convention the page tree uses:
//
//	summary.api.go            GET, /api/summary
//	settings.put.api.go       PUT
//	logs.post.api.go          POST
//	id.dyn.api.go             the segment is a {parameter}
//
// Two outputs:
//
//	-out     the route table, for api.Register
//	-client  a typed client over core/api's transport (optional)
package main

import (
	"bytes"
	"flag"
	"fmt"

	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type endpoint struct {
	Var     string // the exported var holding the Route
	Name    string // Spec.Name, for the client method
	Method  string
	Path    string
	Query   string // type expression as written in the source
	Body    string
	Result  string
	Params  []string // {placeholders} in Path, in order
	File    string
	Dir     string // relative to the apis root; "" at the root
	Pkg     string // package clause of the file
	Import  string // import path of Dir
	Alias   string // import alias in generated files
	Imports map[string]string
}

var (
	pkgRe    = regexp.MustCompile(`(?m)^package\s+([A-Za-z_]\w*)`)
	// Greedy up to the last "]" before the literal's "{": a type argument can
	// itself contain brackets — []telemetry.Event is the common case, and a
	// lazy match stops inside it and captures a truncated type.
	defineRe = regexp.MustCompile(`(?m)^var\s+([A-Z]\w*)\s*=\s*api\.Define\(\s*api\.Spec\[(.+)\]\s*\{`)
	nameRe   = regexp.MustCompile(`Name:\s*"([^"]*)"`)
	methodRe = regexp.MustCompile(`Method:\s*(?:"([A-Za-z]+)"|http\.Method([A-Za-z]+))`)
	pathRe   = regexp.MustCompile(`Path:\s*"([^"]*)"`)
	importRe = regexp.MustCompile(`(?m)^\s*(?:([A-Za-z_]\w*)\s+)?"([^"]+)"\s*$`)
)

var methodMods = map[string]string{
	"get": "GET", "post": "POST", "put": "PUT", "patch": "PATCH", "delete": "DELETE", "head": "HEAD",
}

func main() {
	var dir, module, out, pkg, clientOut, clientPkg, prefix, apiPkg string
	flag.StringVar(&dir, "dir", "server/apis", "root of the endpoint tree")
	flag.StringVar(&module, "module", "", "import path of -dir (required)")
	flag.StringVar(&out, "out", "", "route table to write (default <dir>/apis_gen.go)")
	flag.StringVar(&pkg, "pkg", "", "package name for the route table (default: the root package, or the last path element)")
	flag.StringVar(&clientOut, "client", "", "typed client to write, e.g. client/api/api_gen.go")
	flag.StringVar(&clientPkg, "client-pkg", "api", "package name for the generated client")
	flag.StringVar(&prefix, "prefix", "/api", "URL prefix for derived paths")
	flag.StringVar(&apiPkg, "api", "github.com/mirairoad/howl-go/core/api", "import path of the api package")
	flag.Parse()

	if module == "" {
		log.Fatal("fsapis: -module is required (the import path of -dir)")
	}
	if out == "" {
		out = filepath.Join(dir, "apis_gen.go")
	}

	endpoints, rootPkg, err := crawl(dir, module, prefix)
	if err != nil {
		log.Fatal(err)
	}
	if pkg == "" {
		pkg = rootPkg
	}

	table, err := renderTable(endpoints, pkg, dir, apiPkg)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(out, table, 0o644); err != nil {
		log.Fatal(err)
	}
	for _, e := range endpoints {
		fmt.Printf("  %-6s %-28s %s\n", e.Method, e.Path, filepath.Join(dir, e.File))
	}
	fmt.Printf("%d endpoints -> %s\n", len(endpoints), out)

	if clientOut != "" {
		client, err := renderClient(endpoints, clientPkg, apiPkg)
		if err != nil {
			log.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(clientOut), 0o755); err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(clientOut, client, 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("client -> %s\n", clientOut)
	}
}

func crawl(root, module, prefix string) ([]endpoint, string, error) {
	var endpoints []endpoint
	rootPkg := ""

	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".api.go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}

		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return err
		}
		if rel == "." {
			rel = ""
		}
		filePkg := ""
		if m := pkgRe.FindSubmatch(src); m != nil {
			filePkg = string(m[1])
		}
		if rel == "" && filePkg != "" {
			rootPkg = filePkg
		}

		matches := defineRe.FindAllSubmatch(src, -1)
		if len(matches) == 0 {
			return fmt.Errorf("%s: no `var Name = api.Define(api.Spec[Q, B, R]{…})` found", p)
		}
		if len(matches) > 1 {
			// One endpoint per file is the convention that makes the file name
			// meaningful. Two in one file would need two paths from one name.
			return fmt.Errorf("%s: %d api.Define calls; one endpoint per file", p, len(matches))
		}

		relFile, _ := filepath.Rel(root, p)
		stem, mods, err := parseBase(strings.TrimSuffix(name, ".api.go"))
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		e := endpoint{
			Var:     string(matches[0][1]),
			Method:  "GET",
			File:    relFile,
			Dir:     rel,
			Pkg:     filePkg,
			Import:  module,
			Imports: fileImports(src),
		}
		if rel != "" {
			e.Import = module + "/" + filepath.ToSlash(rel)
		}
		for mod := range mods {
			if m, ok := methodMods[mod]; ok {
				e.Method = m
			}
		}
		if m := methodRe.FindSubmatch(src); m != nil {
			if v := string(m[1]) + string(m[2]); v != "" {
				e.Method = strings.ToUpper(v)
			}
		}
		e.Query, e.Body, e.Result = typeArgs(string(matches[0][2]))
		if m := nameRe.FindSubmatch(src); m != nil {
			e.Name = string(m[1])
		}
		if e.Name == "" {
			e.Name = stem
		}
		e.Path = derivePath(prefix, rel, stem, mods)
		if m := pathRe.FindSubmatch(src); m != nil {
			e.Path = string(m[1]) // an explicit Path wins, which is what it is for
		}
		e.Params = pathParams(e.Path)
		endpoints = append(endpoints, e)
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if rootPkg == "" {
		rootPkg = filepath.Base(root)
	}

	sort.Slice(endpoints, func(i, j int) bool {
		if endpoints[i].Path != endpoints[j].Path {
			return endpoints[i].Path < endpoints[j].Path
		}
		return endpoints[i].Method < endpoints[j].Method
	})

	aliases := map[string]string{}
	for i := range endpoints {
		if endpoints[i].Dir == "" {
			continue
		}
		a, ok := aliases[endpoints[i].Import]
		if !ok {
			a = fmt.Sprintf("e%d", len(aliases))
			aliases[endpoints[i].Import] = a
		}
		endpoints[i].Alias = a
	}
	return endpoints, rootPkg, nil
}

// knownMods keeps a typo from silently becoming part of a URL: `sumary.gett`
// would otherwise publish /api/sumary.gett rather than failing the build.
var knownMods = map[string]bool{"dyn": true, "get": true, "post": true, "put": true, "patch": true, "delete": true, "head": true}

func parseBase(base string) (string, map[string]bool, error) {
	parts := strings.Split(base, ".")
	mods := map[string]bool{}
	for _, m := range parts[1:] {
		if !knownMods[m] {
			return "", nil, fmt.Errorf("unknown modifier %q (known: dyn, get, post, put, patch, delete, head)", m)
		}
		mods[m] = true
	}
	return parts[0], mods, nil
}

func derivePath(prefix, relDir, stem string, mods map[string]bool) string {
	var parts []string
	if relDir != "" {
		for _, seg := range strings.Split(filepath.ToSlash(relDir), "/") {
			s, m, err := parseBase(seg)
			if err == nil && m["dyn"] {
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
	}
	return path.Join(prefix, strings.Join(parts, "/"))
}

func pathParams(p string) []string {
	var out []string
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out = append(out, seg[1:len(seg)-1])
		}
	}
	return out
}

// typeArgs splits "PingQuery, api.None, PingResponse" while respecting the
// brackets of a generic type argument.
func typeArgs(list string) (query, body, result string) {
	var parts []string
	depth, start := 0, 0
	for i, r := range list {
		switch r {
		case '[', '(':
			depth++
		case ']', ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(list[start:i]))
				start = i + 1
			}
		}
	}
	parts = append(parts, strings.TrimSpace(list[start:]))
	for len(parts) < 3 {
		parts = append(parts, "api.None")
	}
	return parts[0], parts[1], parts[2]
}

// fileImports maps a package qualifier to its import path, so a type argument
// written as `telemetry.Event` can be re-qualified in the generated client.
func fileImports(src []byte) map[string]string {
	out := map[string]string{}
	block := src
	if start := bytes.Index(src, []byte("\nimport (")); start >= 0 {
		if end := bytes.Index(src[start:], []byte("\n)")); end >= 0 {
			block = src[start : start+end]
		}
	}
	for _, m := range importRe.FindAllSubmatch(block, -1) {
		alias, importPath := string(m[1]), string(m[2])
		if alias == "" {
			alias = path.Base(importPath)
		}
		out[alias] = importPath
	}
	return out
}
