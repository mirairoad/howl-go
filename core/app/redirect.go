package app

import "net/http"

// HeaderLocation carries a redirect on a fragment response. The client runtime
// reads it and loads the URL as a whole document — see Redirect.
const HeaderLocation = "X-Howl-Location"

// IsPartial reports whether r is the client runtime asking for a fragment: an
// in-page navigation or a hover prefetch, answered without the document shell
// — howl (TS)'s ctx.isPartial.
//
// Middleware that redirects is where it matters. A fragment request that gets
// a plain 302 is followed by fetch() on its own; the client runtime notices and
// shows the page it landed on under that page's URL. Redirect goes one step
// further, for when the landing page should be a fresh document.
func IsPartial(r *http.Request) bool { return r.Header.Get("X-Partial") == "1" }

// Redirect sends the browser to url and makes it load there as a whole
// document, whether the request was a document load or a fragment. Use it
// where the redirect changes more than #outlet — a sign-in, a sign-out, a
// guard that sends someone to a page with a different shell — or where url is
// on another origin, which fetch() cannot follow into a fragment at all.
//
//	if viewer.ID == "" {
//	    app.Redirect(w, r, "/sign-in", http.StatusSeeOther)
//	    return
//	}
//
// A document request gets http.Redirect with code. A fragment request gets 204
// and X-Howl-Location: a status the browser would follow itself would hand the
// client runtime the target's markup as a fragment, and the chrome outside
// #outlet — the signed-in name in the header, say — would stay as it was.
//
// A plain http.Redirect is still correct wherever a swap is enough: the client
// runtime follows it and shows the target under its own URL.
func Redirect(w http.ResponseWriter, r *http.Request, url string, code int) {
	if IsPartial(r) {
		w.Header().Set(HeaderLocation, url)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, url, code)
}
