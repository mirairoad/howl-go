//go:build !(js && wasm)

// The server-side half of package dom: same API, no-ops. A page's Mount is
// compiled into the server binary as dead code and never called, so these exist
// only to keep the types resolvable.
package dom

import (
	"context"
	"errors"
)

type Element struct{}

func SetRoot(any)   {}
func Root() Element { return Element{} }
func Log(...any)    {}
func Warn(...any)   {}

func Navigate(string, ...NavOption) {}
func Prefetch(string)               {}

type NavOptions struct {
	Replace    bool
	Transition string
}

type NavOption func(*NavOptions)

func Replace() NavOption          { return func(*NavOptions) {} }
func Transition(string) NavOption { return func(*NavOptions) {} }

func (Element) Valid() bool               { return false }
func (Element) Query(string) Element      { return Element{} }
func (Element) QueryAll(string) []Element { return nil }
func (Element) Text() string              { return "" }
func (Element) SetText(string)            {}
func (Element) SetHTML(string)            {}
func (Element) Value() string             { return "" }
func (Element) SetValue(string)           {}
func (Element) Hide(bool)                 {}
func (Element) Attr(string) string        { return "" }
func (Element) SetAttr(string, string)    {}
func (Element) On(string, func())         {}

// Fetch is the browser's fetch(); on the server there is no browser. The
// signature exists so a page's Mount compiles for both targets — it is never
// called off the wasm build, the same way Mount itself never runs there.
func Fetch(context.Context, string, string, []byte, map[string]string) (int, []byte, error) {
	return 0, nil, errors.New("dom.Fetch: not running in a browser")
}

func GetJSON(context.Context, string, any) error           { return errNoBrowser }
func PostJSON(context.Context, string, any, any) error     { return errNoBrowser }
func JSON(context.Context, string, string, any, any) error { return errNoBrowser }

var errNoBrowser = errors.New("dom: not running in a browser")
