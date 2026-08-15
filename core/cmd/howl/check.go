package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	out = append(out, lintShell(root, files)...)
	out = append(out, lintEndpoints(root, files)...)
	out = append(out, lintGenerated(root, files)...)
	if build {
		out = append(out, lintBuild(root)...)
	}

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

// ---------------------------------------------------------------------------
// Rules
// ---------------------------------------------------------------------------

var (
	importAppRe   = regexp.MustCompile(`"[^"]*howl-go/core/app"`)
	templMountRe  = regexp.MustCompile(`(?m)^templ\s+(Mount|Unmount)\s*\(`)
	rawQueryRe    = regexp.MustCompile(`r\.HTTP\.(URL\.Query\(\)|FormValue|PostFormValue)`)
	rolesRe       = regexp.MustCompile(`Roles:\s*\[\]string\{[^}]*"`)
	authorizeRe   = regexp.MustCompile(`Authorize:\s*`)
	structFieldRe = regexp.MustCompile(`(?m)^\s+([A-Z]\w*)\s+[\[\]\*\w\.]+(\s+` + "`" + `[^` + "`" + `]*` + "`" + `)?\s*$`)
	generatedRe   = regexp.MustCompile(`^// Code generated`)
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
