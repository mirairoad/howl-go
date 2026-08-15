package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// howl mcp — the conventions as tools an agent can call
//
// llms.txt tells an agent the rules. This lets it ask. The difference matters
// because an agent cannot decide to consult a convention file it has to
// remember exists, whereas a tool shows up in its tool list.
//
// Stateless on purpose: every tool is a pure function of the working tree, so
// there is no session to keep, nothing to resume, and the dev server restarting
// underneath changes nothing. That also makes it correct over stdio and over a
// stateless HTTP transport without two code paths.
//
// JSON-RPC 2.0, newline-delimited on stdin/stdout, hand-rolled. The surface is
// initialize / tools/list / tools/call, and writing it out costs less than the
// first dependency in a module that has one.
// ---------------------------------------------------------------------------

// defaultProtocol is what we answer with when a client does not name a version.
// When it does, we echo it back: this server's tools are plain request/response
// and work across every revision that has tools/call, so refusing a client over
// a version string would be the only thing that could break.
const defaultProtocol = "2025-06-18"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func mcpCommand(args []string) error {
	fset := flag.NewFlagSet("mcp", flag.ExitOnError)
	dir := fset.String("dir", ".", "project root the tools answer about")
	fset.Parse(args)

	root, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	return serveMCP(os.Stdin, os.Stdout, root)
}

func serveMCP(in io.Reader, out io.Writer, root string) error {
	reader := bufio.NewReaderSize(in, 1<<20)
	encoder := json.NewEncoder(out)

	for {
		line, err := reader.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) == 0 {
			if err != nil {
				return nil
			}
			continue
		}
		var req rpcRequest
		if jsonErr := json.Unmarshal(line, &req); jsonErr != nil {
			// A malformed line is the client's problem, but dying here would
			// take the whole session with it.
			encoder.Encode(rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}}) //nolint:errcheck
			if err != nil {
				return nil
			}
			continue
		}

		result, rpcErr := dispatch(root, req)
		// A notification has no id and takes no answer — replying to one is a
		// protocol error some clients treat as fatal.
		if len(req.ID) > 0 {
			encoder.Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rpcErr}) //nolint:errcheck
		}
		if err != nil {
			return nil
		}
	}
}

func dispatch(root string, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &params) //nolint:errcheck
		version := params.ProtocolVersion
		if version == "" {
			version = defaultProtocol
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "howl-go", "version": "dev"},
			"instructions": "Tools for working in a howl-go project. Call howl_conventions before writing " +
				"routing, page or endpoint code — the file-naming rules cannot be guessed, because Go rejects " +
				"the conventions every JS framework uses. Call howl_check after editing.",
		}, nil

	case "ping":
		return map[string]any{}, nil

	case "notifications/initialized", "notifications/cancelled":
		return nil, nil

	case "tools/list":
		return map[string]any{"tools": tools()}, nil

	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid params"}
		}
		text, err := callTool(root, params.Name, params.Arguments)
		if err != nil {
			// A failing tool is a result, not a transport error: the model has
			// to see the message to act on it.
			return map[string]any{
				"content": []any{map[string]any{"type": "text", "text": err.Error()}},
				"isError": true,
			}, nil
		}
		return map[string]any{
			"content": []any{map[string]any{"type": "text", "text": text}},
		}, nil
	}
	return nil, &rpcError{Code: -32601, Message: "unknown method " + req.Method}
}

func object(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func tools() []tool {
	dirProp := map[string]any{"dir": str("project root; defaults to the directory the server was started in")}
	return []tool{
		{
			Name:  "howl_conventions",
			Title: "howl-go conventions",
			Description: "The framework's conventions in one file (llms.txt): routing and file-name modifiers, " +
				"page anatomy, the shell contract, the package layering rule, the endpoint layer, and an explicit " +
				"list of things not to invent. Read this before writing howl-go code — Go rejects _layout.templ " +
				"and [id].templ, so the answers here look different from every JS framework on purpose.",
			InputSchema: object(map[string]any{
				"section": str("optional heading to return on its own, e.g. \"Routing conventions\""),
			}),
		},
		{
			Name:  "howl_check",
			Title: "Check a howl-go project",
			Description: "Run the framework's conventions against the project and return structured diagnostics: " +
				"pages importing core/app, `templ Mount()`, a shell missing #outlet or the page-head markers, " +
				"endpoints reading the raw query instead of their declared one, roles declared with no Authorize " +
				"wired, hand-edited generated files. Optionally also runs go generate/build/vet.",
			InputSchema: object(map[string]any{
				"dir":   dirProp["dir"],
				"build": map[string]any{"type": "boolean", "description": "also run go generate, go build and go vet (slower)"},
			}),
		},
		{
			Name:  "howl_routes",
			Title: "Page routes",
			Description: "The page route table this project actually serves, read from the generated fsroutes_gen.go: " +
				"pattern, label, whether the browser renders it (wasm) and whether it has lifecycle hooks. " +
				"Use it instead of guessing what a URL maps to.",
			InputSchema: object(dirProp),
		},
		{
			Name:  "howl_endpoints",
			Title: "API endpoints",
			Description: "The endpoint table this project actually serves, read from the generated apis_gen.go plus " +
				"each *.api.go: method, path, the declaring file, roles, and the Go query/body/response types.",
			InputSchema: object(dirProp),
		},
		{
			Name:  "howl_scaffold",
			Title: "Scaffold a page or an endpoint",
			Description: "Create a page or an endpoint file with the correct name, location and shape. The file name " +
				"carries the behaviour in this framework, so a scaffold is the difference between a route that exists " +
				"and a build error. Refuses to overwrite.",
			InputSchema: object(map[string]any{
				"dir":  dirProp["dir"],
				"kind": map[string]any{"type": "string", "enum": []string{"page", "endpoint"}, "description": "what to create"},
				"path": str("the URL this should serve, e.g. /reports or /blog/{id} or /api/reports"),
				"name": str("display name; the page label or the endpoint's Name"),
				"method": map[string]any{"type": "string", "enum": []string{"GET", "POST", "PUT", "PATCH", "DELETE"},
					"description": "endpoint only; defaults to GET"},
				"client": map[string]any{"type": "boolean", "description": "page only: also render it in the browser (.client)"},
				"roles":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "endpoint only: role strings your Authorize will interpret"},
			}, "kind", "path"),
		},
	}
}

func callTool(root, name string, raw json.RawMessage) (string, error) {
	var args struct {
		Dir     string   `json:"dir"`
		Section string   `json:"section"`
		Build   bool     `json:"build"`
		Kind    string   `json:"kind"`
		Path    string   `json:"path"`
		Name    string   `json:"name"`
		Method  string   `json:"method"`
		Client  bool     `json:"client"`
		Roles   []string `json:"roles"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	dir := root
	if args.Dir != "" {
		abs, err := filepath.Abs(args.Dir)
		if err != nil {
			return "", err
		}
		dir = abs
	}

	switch name {
	case "howl_conventions":
		return conventions(args.Section), nil
	case "howl_check":
		return asJSON(runCheck(dir, args.Build))
	case "howl_routes":
		return asJSON(readPageRoutes(dir))
	case "howl_endpoints":
		return asJSON(readEndpoints(dir))
	case "howl_scaffold":
		return scaffold(dir, args.Kind, args.Path, args.Name, args.Method, args.Client, args.Roles)
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

func asJSON(v any) (string, error) {
	out, err := json.MarshalIndent(v, "", "  ")
	return string(out), err
}

// conventions serves llms.txt, embedded so the tool answers the same way from a
// downloaded module as from a checkout. Serving the file rather than a second
// copy of the rules is the point: there is one place to update.
func conventions(section string) string {
	text := string(llmsTxt)
	if section == "" {
		return text
	}
	lines := strings.Split(text, "\n")
	var out []string
	capturing := false
	for _, line := range lines {
		if strings.HasPrefix(line, "#") {
			heading := strings.TrimSpace(strings.TrimLeft(line, "# "))
			if capturing {
				break
			}
			capturing = strings.Contains(strings.ToLower(heading), strings.ToLower(section))
		}
		if capturing {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return "no section matching " + section + " — call this tool with no arguments for the whole file"
	}
	return strings.Join(out, "\n")
}

// ---------------------------------------------------------------------------
// Reading what the project actually serves
// ---------------------------------------------------------------------------

type pageRoute struct {
	Pattern string `json:"pattern"`
	Label   string `json:"label"`
	Client  bool   `json:"client"`
	Raw     bool   `json:"raw"`
	Mount   bool   `json:"mount"`
}

var routeLineRe = regexp.MustCompile(`\{Pattern: "([^"]*)", Label: "([^"]*)"[^}]*?Mount: (\w+[\w.]*)[^}]*?Client: (true|false), Raw: (true|false)\}`)

func readPageRoutes(root string) any {
	var routes []pageRoute
	for _, f := range sourceFiles(root) {
		if filepath.Base(f.Rel) != "fsroutes_gen.go" {
			continue
		}
		for _, m := range routeLineRe.FindAllStringSubmatch(string(f.Body), -1) {
			routes = append(routes, pageRoute{
				Pattern: m[1], Label: m[2],
				Mount:  m[3] != "nil",
				Client: m[4] == "true",
				Raw:    m[5] == "true",
			})
		}
	}
	if routes == nil {
		return map[string]any{
			"routes": []pageRoute{},
			"note":   "no fsroutes_gen.go found — run the generator (make, or howl dev) first",
		}
	}
	return map[string]any{"routes": routes}
}

type endpointInfo struct {
	Method   string   `json:"method"`
	Path     string   `json:"path"`
	Name     string   `json:"name,omitempty"`
	File     string   `json:"file,omitempty"`
	Roles    []string `json:"roles,omitempty"`
	Query    string   `json:"query,omitempty"`
	Body     string   `json:"body,omitempty"`
	Response string   `json:"response,omitempty"`
}

var (
	atRe        = regexp.MustCompile(`api\.At\("([A-Z]+)",\s*"([^"]*)",\s*([\w.]+)\)`)
	specRe      = regexp.MustCompile(`(?m)^var\s+([A-Z]\w*)\s*=\s*api\.Define\(\s*api\.Spec\[(.+)\]\s*\{`)
	specNameRe  = regexp.MustCompile(`Name:\s*"([^"]*)"`)
	roleValueRe = regexp.MustCompile(`"([^"]*)"`)
)

func readEndpoints(root string) any {
	byVar := map[string]endpointInfo{}
	for _, f := range sourceFiles(root) {
		if !strings.HasSuffix(f.Rel, ".api.go") {
			continue
		}
		m := specRe.FindSubmatch(f.Body)
		if m == nil {
			continue
		}
		info := endpointInfo{File: f.Rel}
		parts := strings.Split(string(m[2]), ",")
		for i, p := range parts {
			p = strings.TrimSpace(p)
			switch i {
			case 0:
				info.Query = p
			case 1:
				info.Body = p
			case 2:
				info.Response = p
			}
		}
		if n := specNameRe.FindSubmatch(f.Body); n != nil {
			info.Name = string(n[1])
		}
		if r := rolesRe.Find(f.Body); r != nil {
			for _, role := range roleValueRe.FindAllStringSubmatch(string(r), -1) {
				info.Roles = append(info.Roles, role[1])
			}
		}
		byVar[string(m[1])] = info
	}

	var endpoints []endpointInfo
	for _, f := range sourceFiles(root) {
		if filepath.Base(f.Rel) != "apis_gen.go" {
			continue
		}
		for _, m := range atRe.FindAllStringSubmatch(string(f.Body), -1) {
			name := m[3]
			if i := strings.LastIndex(name, "."); i >= 0 {
				name = name[i+1:]
			}
			info := byVar[name]
			info.Method, info.Path = m[1], m[2]
			endpoints = append(endpoints, info)
		}
	}
	if endpoints == nil {
		return map[string]any{
			"endpoints": []endpointInfo{},
			"note":      "no apis_gen.go found — this project has no endpoint tree, or the generator has not run",
		}
	}
	return map[string]any{"endpoints": endpoints}
}

//go:embed llms.txt
var llmsTxt []byte
