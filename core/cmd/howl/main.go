// Command howl is the development tool.
//
//	go run github.com/mirairoad/howl-go/core/cmd/howl dev
//
// `dev` watches the tree, regenerates the route table, runs templ, rebuilds,
// restarts the binary, and reloads the browser. It also proxies a stable port
// in front of the restarting server, so the address in your address bar never
// stops answering and in-flight requests wait for the new binary instead of
// failing.
//
// Go cannot hot-swap a linked binary, so there is no HMR here and this file
// does not pretend otherwise: the loop is rebuild and restart, measured and
// printed on every pass. What it removes is the manual part.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "dev":
		if err := dev(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "howl dev:", err)
			os.Exit(1)
		}
	case "check":
		if err := check(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "howl check:", err)
			os.Exit(1)
		}
	case "mcp":
		if err := mcpCommand(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "howl mcp:", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "howl: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`howl — the howl-go development tool

usage:
  howl dev [flags]     watch, rebuild, restart, reload
  howl check [flags]   the conventions, enforced: -json for structured output
  howl mcp [flags]     serve the conventions and checks as MCP tools (stdio)

run "howl <command> -h" for the flags.
`)
}
