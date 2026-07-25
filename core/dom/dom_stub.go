//go:build !(js && wasm)

// The server-side half of package dom: same API, no-ops. A page's Mount is
// compiled into the server binary as dead code and never called, so these exist
// only to keep the types resolvable.
package dom

type Element struct{}

func SetRoot(any)   {}
func Root() Element { return Element{} }
func Log(...any)    {}
func Warn(...any)   {}

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
