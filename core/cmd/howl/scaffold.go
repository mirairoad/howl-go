package main

import (
	"fmt"
	"os"
	"path/filepath"
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
func scaffold(root, kind, urlPath, name, method string, client bool, roles []string) (string, error) {
	if urlPath == "" {
		return "", fmt.Errorf("scaffold: path is required, e.g. /reports or /api/reports")
	}
	switch kind {
	case "page":
		return scaffoldPage(root, urlPath, name, client)
	case "endpoint":
		return scaffoldEndpoint(root, urlPath, name, method, roles)
	}
	return "", fmt.Errorf("scaffold: kind must be \"page\" or \"endpoint\", got %q", kind)
}

func scaffoldPage(root, urlPath, label string, client bool) (string, error) {
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
	pkg := goIdent(filepath.Base(dir))
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

	if err := write(file, body); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %s\n\nroute:   %s\npackage: %s\n\nRe-run the generators (make, or howl dev picks it up on save).",
		rel(root, file), urlPath, pkg), nil
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
