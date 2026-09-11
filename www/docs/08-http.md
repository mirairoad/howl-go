# The HTTP layer

Everything between the socket and a component: middleware, redirects, caching, static files, status codes, errors, sub-applications, request state.

There is no framework handler type and no context wrapper. A middleware is `func(http.Handler) http.Handler` and a handler is `http.Handler` — so anything written for chi, gorilla or the standard library drops in unchanged, and anything you write here works outside howl-go.

## Middleware

Listed outermost first: the first entry sees the request first and the response last.

```go
a := app.New(app.Config{
	Routes: pages.FsClientRoutes(),
	Shell:  pages.App,
	Use: []mw.Middleware{
		mw.RequestID,
		mw.Logger(nil),
		mw.Recover(nil),
		mw.Compress{}.Handler,
	},
})
```

The chain wraps everything — pages, static files, and any handler the application adds to the mux. `a.Use(...)` appends to it for a chain built conditionally, and `a.Wrap(h)` applies it to a handler of your own.

| middleware | what it does |
|---|---|
| `mw.RequestID` | one id per request, on the context and in `X-Request-Id`. An inbound id is reused, but only if it is short and printable — it ends up in log lines and headers |
| `mw.Logger(nil)` | one `log/slog` line per request: method, path, status, bytes, duration, id. Level follows the status, so filtering to warnings shows exactly the failures. SPA fragments are tagged `partial=true` |
| `mw.Recover(nil)` | a panic becomes a 500 instead of a dropped connection. If the status line is already out it re-panics, because a truncated body is the honest signal |
| `mw.Compress{}` | gzip for dynamic responses |
| `mw.CORS{…}` | preflights and cross-origin headers |
| `mw.CSRF{…}` | origin check plus double-submit token |
| `mw.CSP{…}` | Content-Security-Policy, with a per-request nonce |
| `mw.Coalesce{}` | identical concurrent requests share one render |
| `mw.Cache{…}` | keeps a GET's 200 response for a TTL — the store behind endpoint and page caching |
| `mw.SecureHeaders{}` | `nosniff`, `Referrer-Policy`, `X-Frame-Options`, and HSTS when asked |
| `mw.Proxy{…}` | forwards some paths to another server |
| `mw.Only(prefix, …)`, `mw.Except(prefix, …)` | middleware for one part of the site |

howl (TypeScript) users will recognise most of these: `SecureHeaders` is `defaultHeaders`, `Proxy` is `createProxy`, and `Only`/`Except` are what the per-path `app.use('/admin', …)` did. `trailingSlashes('never')` is a setting rather than a middleware — see below.

### Trailing slashes

Every page answers on both `/about` and `/about/` by default. `app.Config{RedirectTrailingSlash: true}` makes one of them the address: the slashed spelling gets a `301` to the other, query kept, and the root is left alone.

It is a setting on the page routes and not a middleware for a reason that shows up as "too many redirects": `ServeMux` itself redirects `/files` to `/files/` whenever something is mounted as a subtree (`mux.Handle("/files/", …)`), and a middleware stripping the slash sends that request straight back. The page routes are the ones the app registered, with no subtree under them, so only they redirect.

### Only and Except

```go
Use: []mw.Middleware{
	mw.RequestID,
	mw.Except("/api", mw.CSRF{Secure: true}.Handler), // forms need it; a bearer-token API does not
	mw.Only("/admin", requireAdmin),
}
```

A prefix is matched a whole segment at a time: `/admin` covers `/admin` and `/admin/users`, never `/administrator`. A guard written with `strings.HasPrefix` protects a page it was never meant to — or, used to exempt a path, exempts one. `mw.Under(path, prefix)` is the same test, for middleware of your own.

### SecureHeaders

The zero value sends `X-Content-Type-Options: nosniff`, `Referrer-Policy: strict-origin-when-cross-origin` and `X-Frame-Options: DENY`, before the handler runs, so a handler that needs something else sets its own. `"-"` turns one off; `HSTS` adds `Strict-Transport-Security` — set it only once the site is https for good, because a browser that has seen it refuses plain http for that long.

Two headers howl (TS) sent are left out on purpose. `X-XSS-Protection: 1; mode=block` drove a filter every browser has since removed, and `mode=block` could be used to delete chosen scripts from a page; CSP replaced it. And `Cache-Control: no-store` on every response would switch off the year-long immutable assets below — each response here already says how it may be cached.

### Proxy

```go
mw.Proxy{
	Target:      "http://127.0.0.1:8000",
	Paths:       []string{"/api/v1/admin/{table}/restore"},
	StripPrefix: false,
}.Handler
```

It is `httputil.ReverseProxy`, so streaming, WebSocket upgrades and hop-by-hop headers are already right. `Paths` match by segment, with `{name}` matching any one — howl (TS) matched with `startsWith`, so a pattern with `:table` in it could never match a request. Requests that match nothing pass on to the next handler.

What it decides for you: `X-Forwarded-For`, `-Host` and `-Proto` are written from what this server saw, never passed through from the client; a target's `Set-Cookie` loses its `Domain` (a cookie scoped to a domain the browser never talked to is refused outright — `KeepCookieDomain` to keep it); the target's own host is sent (`KeepHost` to forward the incoming one); and a target that is down or slower than `Timeout` to answer (30 s, headers only, so a download is not cut off) is a `502` or `504` whose body does not name its address.

### Compress

Nothing is decided until the response has either produced `MinSize` bytes (1024 by default) or finished, so a 200-byte JSON reply is not wrapped in a gzip frame that makes it *bigger*. Binary types are left alone; they are already compressed. A handler that flushes early — SSE — forces the decision at the first flush, since a stream has no end to wait for.

Static files are not compressed here. They go through the static handler, which compresses each file once and keeps the result.

### Coalesce

A thundering-herd guard, not a cache. Ten clients asking for the same page in the same instant produce one render and ten identical responses; nothing is retained afterwards, so a response is never stale.

It refuses to share four things, because sharing them would be a bug:

- anything but `GET` and `HEAD`;
- requests carrying `Cookie` or `Authorization` — those responses are probably specific to one user;
- responses that set a cookie, since replaying one `Set-Cookie` to every waiter hands them all the same session or CSRF token;
- responses past `MaxBody` (8 MiB), where the waiters simply run the handler themselves.

The key includes `X-Partial` by default: a fragment and a document share a URL but are not the same response.

### CSRF

Two independent checks, because each alone has a hole. The origin must be this site — which covers a plain cross-domain form post, but passes a request that carries neither `Origin` nor `Referer`. And the token in the cookie must equal the token in the request: an attacker's page can make the browser *send* your cookie, but cannot *read* it to echo the value back.

```templ
<input type="hidden" name="csrf_token" value={ mw.CSRFToken(ctx) }/>
```

Multipart posts must send the token in the header — parsing a multipart body in middleware would mean buffering an upload before the handler has decided it wants one.

### CSP

Write the policy as a string; a struct per directive would be a second, worse syntax for something the spec already defines. The literal `{nonce}` is replaced per request:

```go
mw.CSP{Policy: "default-src 'self'; script-src 'self' 'nonce-{nonce}'"}.Handler
```

```templ
<script nonce={ mw.Nonce(ctx) } src="/static/app.js" type="module"></script>
```

Start with `ReportOnly: true`. A policy that blocks your own stylesheet looks exactly like a broken deploy.

## Logging

```go
console.Setup(console.Options{})   // installs as slog.Default
```

```
09:45:31.924 INFO  listening   url=http://localhost:4399 routes=5
09:45:33.939 INFO  http        GET / 200 7.1kB 1.01ms id=869c8d0d
09:45:33.988 INFO  http        POST /api/logs 202 0B 6.76ms id=b13b1e4b ip=10.1.4.9 ua=otel-collector/0.96
09:45:34.220 WARN  http        GET /nope 404 1.6kB 45µs id=24ab09a8
```

Level is tinted, and so is the status — a wall of green with one red row is a signal, a wall of identical grey is not. `method`, `path`, `status`, `bytes` and `took` are rendered positionally in that order whatever order you pass them, so a request line reads as a request line; everything else stays `key=value`.

howl (TypeScript) does this by patching `globalThis.console`. Go has no console to patch and does not need one: `log/slog` is already the seam every library logs through, so installing a handler reaches your code, the framework's code and your dependencies' code at once — and nothing has to import `core/console` to benefit.

**When to colour is decided by who is reading:**

| stdout is | output |
|---|---|
| a terminal | tinted, aligned, human columns |
| a pipe, a file, systemd, Docker | JSON, one object per line |

So the same binary is pleasant to run and parseable to ship, with nothing to configure. `NO_COLOR` is honoured and wins outright; `FORCE_COLOR` turns it on where detection cannot tell (a CI viewer that renders escapes but is not a terminal). `Options{JSON: …}` and `Options{Color: …}` override both.

### The startup line

`a.Listen` logs it through the same logger rather than printing to stdout, so it obeys whatever the process decided about format. It is the one line you always want: a server that is up and a server that is up *on the port you meant* look identical otherwise. At debug level it is preceded by one line per route.

### One switch

```go
app.Config{Logger: true}
```

howl (TS)'s `logger: true`: `mw.RequestID` and `mw.LogWith` with `Callers` and `SkipNoise`, through `Config.Log`, placed outside `Use` so the line times everything the chain does. Leave it off when `Use` already lists a logger, or each request is logged twice. A `mw.RequestID` in both places keeps the first id, so the log and the response agree.

### Who is hitting your API

```go
mw.LogWith(mw.LogOptions{Callers: true, Skip: mw.SkipNoise})
```

`Callers` adds `ip` and `ua` — **but only for requests that did not come from a page on this host**. Your own SPA's fetches carry a same-origin `Referer` or `Sec-Fetch-Site`, so they stay quiet; a telemetry exporter, a scraper, a partner's backend or curl do not, and get identified. Logging your own IP on every navigation is noise that hides the thing you wanted to see.

Three signals decide, cheapest first: `Sec-Fetch-Site`, then `Origin`, then `Referer`. A request with none of them is not a browser on your site. `Sec-Fetch-Site: cross-site` is believed over a forged `Referer`, since the browser sets it and script cannot.

`TrustProxy: true` reads the client address from `X-Forwarded-For` — only set it behind a proxy that *overwrites* that header, because anyone can send it. The same rule is exported as `mw.ClientIP(r, trustProxy)`, for an audit column like `last_ip`; a forwarded value that does not parse as an IP address is ignored, since a header is free text.

`Skip: mw.SkipNoise` drops `/static/`, `/healthz`, `/favicon.ico` and friends. They are the majority of requests and the least informative line in any log.

## Static files

`/static/` is served by `app.Static`, which adds the three things `http.FileServer` leaves to you: an ETag, a `Cache-Control`, and a compressed copy.

Compression happens **once per file** and is kept. That is the difference that matters here: a 6.1 MB wasm binary gzipped per request burns a core per download; gzipped once it costs 1.71 MB of memory and nothing per request.

| config | effect |
|---|---|
| default | `public, no-cache` — the client revalidates, an unchanged file answers `304` with no body |
| `StaticMaxAge: time.Hour` | `public, max-age=3600` |
| `StaticImmutable: app.Hashed` | for files you hashed yourself at build time |
| `Dev: true` | notice when a file changes on disk, for a directory instead of an `embed.FS` |

The gzipped representation carries its own ETag (`"abc…-gz"`). Two representations sharing a validator is how a cache ends up serving compressed bytes to a client that asked for plain.

### Content-hashed URLs

```templ
<link rel="stylesheet" href={ router.Asset(ctx, "app.css") }/>
<script src={ router.Asset(ctx, "app.js") } type="module"></script>
```

```
/static/app.b3089031.css     Cache-Control: public, max-age=31536000, immutable
/static/app.css              Cache-Control: public, no-cache
```

Under its own name a file has to be revalidated: the browser cannot know it is unchanged without asking, so every page load spends a conditional request per asset to be told `304` and handed nothing. Under a name containing its content hash the question cannot arise — change the file and the URL changes, so the response can promise a year.

The hash is not a build step. It is the ETag, which the handler already computes, so this works the same for an `embed.FS` in production and a watched directory in development, with nothing to generate and no renamed files on disk. The wasm renderer's URLs are published to the client the same way, through `howl-client`.

A hashed URL from an older deploy still serves the current bytes — but with `no-cache`, never `immutable`, because the name no longer describes what is in it.

Everything is compressed **once**, in the background at `Listen`, so no request pays for it — not even the first. `Dev` compares modification time and size before rebuilding, so a watched directory stays fresh without redoing the work: on a 6.94 MB wasm binary that distinction is **530 ms per request against 2.4 ms**, and the original version paid it on the `304`s too, where nothing is transferred at all.

The application's own files are layered over the framework's, so `app.js` is served without ever being copied into your project — and a file of the same name in your `static/` wins.

### Files at the root

`/static/` is for your assets. Browsers, crawlers and other tools ask for a few names at the root and will not look anywhere else — `/favicon.ico`, `/robots.txt`, `/.well-known/security.txt`:

```go
Use: []mw.Middleware{app.StaticFiles(rootFS)}
```

howl (TS)'s `staticFiles()`. Same handler as `/static/` — ETag, `Cache-Control`, compressed once — for `GET` and `HEAD` of a file the FS has; everything else passes on. The FS is listed once, so a page request costs a map lookup rather than an open. A file shadows a page at the same path, so give it a directory of its own rather than pointing it at `Public`.

## Redirects

A plain `http.Redirect` works for every request, including the client runtime's fragment requests: `fetch()` follows it, and `app.js` shows the page it landed on **under that page's URL**, without caching it under the one that redirected. Before, the target was swapped in silently under the old address, and a reload then disagreed with the screen.

When the redirect should be a fresh document — a sign-in or sign-out, a guard sending someone to a page with a different shell, anything on another origin — use `app.Redirect`:

```go
if viewer.ID == "" {
	app.Redirect(w, r, "/sign-in", http.StatusSeeOther)
	return
}
```

A document request gets the redirect as usual. A fragment request gets `204` with `X-Howl-Location`, and the client loads that URL as a whole page, so chrome outside `#outlet` — the name in the header — is rendered again. `app.IsPartial(r)` tells the two apart for middleware of your own. A followed redirect to another origin, a non-HTML response or a `.raw` route is a document load either way.

## Endpoints: headers, cookies, caching

An endpoint returns a value and the framework writes it, so a handler has no writer to break. What it can add is headers and cookies:

```go
Handler: func(r *api.Request[api.None, SignIn]) (Session, error) {
	token, err := sessions.Start(r.Context(), r.Body.Email)
	if err != nil {
		return Session{}, err
	}
	r.SetCookie(&http.Cookie{Name: "session", Value: token, HttpOnly: true, SameSite: http.SameSiteLaxMode, Path: "/"})
	return Session{Email: r.Body.Email}, nil
},
```

`r.Header()` is the response's header map and `r.SetCookie` adds to it — howl (TS)'s `ctx.headers` and `ctx.cookies.set`. Both go out with error responses too, because clearing a session cookie on a `401` is a real case. `Content-Type` stays the framework's.

A query field tagged `path:"id"` is the typed form of `r.Param("id")`, like howl (TS)'s `params` schema: `/orders/abc` answers `400` naming `id` before the handler runs, and a tag that names no placeholder in the path panics at `Register` instead of staying zero forever.

### Caching an endpoint

```go
var Suggest = api.Define(api.Spec[SuggestQuery, api.None, []Place]{
	Name:        "Suggest",
	Description: "Address suggestions from the maps provider, which bills per call.",
	Cache:       api.Cache{TTL: 5 * time.Second},
	Handler:     …,
})
```

howl (TS)'s `caching: { ttl }`, with its key: path, query, caller. `api.Config.Identity(r)` names the caller — return the user id, or `""` for anonymous visitors, who then share. Without it, a request carrying `Cookie` or `Authorization` is keyed by those headers themselves: never wrong, but every browser holding any cookie gets its own entry.

| rule | why |
|---|---|
| `Authorize` runs first, on hits too | an entry filled by an admin is not a way past the roles |
| GET only; a cached POST panics at `Register` | a stored POST answers the second submission with the first one's result |
| 200 only | a cached 500 is an outage that outlives its cause |
| never a response that sets a cookie | a stored `Set-Cookie` hands every later caller the same session |
| request `Cache-Control` is ignored | the cache usually protects something from exactly that traffic |

Responses say `X-Howl-Cache: hit` or `miss`, and a hit carries `Age`. There is no purge: a TTL is the whole contract, so cache what may be that stale. A read that must show the write that just happened belongs uncached, or in `db`'s document cache, which the write itself invalidates.

`api.Config.Cache` is the store — `nil` is one in-process LRU for the table. It is a `cache.Store` (`core/cache`), the same interface as `db.Cache.Adapter` and `app.Config.Cache`, so one Redis adapter serves all three, and `cache.Try(redis, cache.NewLRU(1000), 150*time.Millisecond)` falls back to memory when Redis is slow or gone.

### Caching a page

```go
//howl:cache 30s
templ Pricing() { … }
```

The rendered response is reused — but only between visitors who send **no cookie and no `Authorization`**, and never from a response that sets a cookie. A rendered page is somebody's: it may greet the signed-in user or carry their CSRF token, and a visitor with no cookie is the one visitor it provably is not.

That has a consequence worth knowing before reaching for it: an application whose middleware hands every new visitor a cookie — `mw.CSRF` does — never stores a page, because every cookie-less response sets one. That is the rule working. Cache such a page's data instead, in `Config.Data` or the document store. A fragment and a whole document are separate entries, and the build id is part of the key, so a shared store never serves a document naming the previous deploy's assets.

## Status codes

A page that discovers mid-render that the thing it was asked for does not exist has to be able to say so:

```templ
templ Article() {
	{{ a, ok := store.ArticleByID(router.Param(ctx, "article_id")) }}
	if ok {
		<h1>{ a.Title }</h1>
	} else {
		{{ router.NotFound(ctx) }}
		<h1>404</h1>
		<p>No such article.</p>
	}
}
```

`router.SetStatus(ctx, code)` sets any status; `router.NotFound(ctx)` is the 404 shorthand. Without this, `/blog/nope` answers "no such article" markup with HTTP 200 — a soft 404, invisible to crawlers and uptime checks alike.

It lives in `router`, not `app`, for a layering reason worth knowing: the generated route table sits inside the page tree, so anything a page imports has to stay a leaf. A page importing the server runtime to set a status would drag `net/http` into the wasm build.

This works because **the page is rendered into a buffer before anything is sent**. Buffering buys three things a streaming render cannot have: a status a component can still change, an error page instead of a truncated document, and a `Content-Length`. The cost is holding one document in memory per in-flight request; the buffers are pooled.

## Errors

```go
app.Config{
	OnError: func(w http.ResponseWriter, r *http.Request, err error) {
		w.WriteHeader(http.StatusInternalServerError)
		pages.Error(err).Render(r.Context(), w)
	},
}
```

`OnError` owns the whole response, because nothing has been written when it runs. A component that fails halfway leaves no half-page on the wire. The default is a plain 500 and a log line.

## Sub-applications

```go
a.Mount("/admin", adminMux)
a.Mount("/metrics", promhttp.Handler())
```

The prefix is stripped, so the mounted handler sees `/users`, not `/admin/users`. There is no sub-app type to learn — anything that is an `http.Handler` mounts.

Unknown paths fall through to the 404 page for **every** method, not just `GET`: a method-specific catch-all conflicts with any method-less pattern registered above it, and `ServeMux` panics at registration rather than at the request.

## Request state

Go already has the bag — `context.Context`. What it lacks is the typing, and generics supply it. The key *is* the type:

```go
type User struct{ ID string }

// middleware
ctx := state.With(r.Context(), User{ID: "u_1"})

// any component
u := state.Get[User](ctx)              // zero value if absent
u, ok := state.From[User](ctx)         // when absent matters
```

One value per type. Two things with the same underlying type that mean different things want different named types — which is what you would want anyway.

`Config.Data` remains the hook for values every render needs; `state` is how anything else travels.

## Server-sent events

```go
mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
	s, err := app.SSE(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for {
		select {
		case <-s.Done():
			return
		case t := <-ticker.C:
			if s.Send("tick", t.String()) != nil {
				return // client went away
			}
		}
	}
})
```

Multi-line payloads are split across `data:` lines, as the wire format requires — a raw newline would end the event early. The browser reconnects on its own, so a failed `Send` is a `return`, not an error to report.
