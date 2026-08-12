# The dev server

```bash
go run github.com/mirairoad/howl-go/core/cmd/howl dev
```

Watch the tree, regenerate the route table, run templ, rebuild, restart the binary, reload the browser. Replaces `go run .` and the four commands you were running before it.

```
10:15:45.538 INFO  howl dev    url=http://localhost:9000 watching=. pages=example.com/myapp/client/pages
10:15:46.437 INFO  listening   url=http://127.0.0.1:49766 routes=8
10:15:46.457 INFO  rebuilt     why="first build" build=890.62ms total=918.54ms
10:16:27.088 INFO  css         file=client/public/app.css
10:16:30.649 INFO  rebuilt     why=client/pages/index.templ build=535.5ms total=564.18ms
```

## What happens on a change

| you change | what runs | browser |
|---|---|---|
| a `.css` file under `client/public/` | nothing | stylesheet swapped in place |
| anything else under `client/public/` | nothing | reload |
| a `.templ` under `client/pages/` | fsroutes → templ → build → restart | reload |
| any other `.templ` | templ → build → restart | reload |
| a `.go` file | build → restart | reload |
| a `.md` file | your `-pre` command, then all of the above | reload |

Measured on the example app: **~540 ms** for a templ edit, **~390 ms** for a Go edit. The CSS path is instant, because nothing is rebuilt at all.

**There is no HMR and this does not pretend otherwise.** Go cannot hot-swap a linked binary — a new version of your code means a new process. What the dev server removes is the manual part, plus the two seconds you spend switching to the browser and pressing reload.

## The port stays up

The browser talks to the dev server, never to your application directly. Your app is started on a private port and proxied.

That buys three things worth having:

- The address in your address bar never stops answering. No connection-refused to reload past.
- A request that lands mid-rebuild **waits** for the new binary instead of failing. A page that takes 800 ms once beats an error page you have to dismiss.
- The live-reload connection survives the restart it is reporting, which is what makes the reload reliable rather than a race.

## A failed build keeps the last good binary serving

The running process is not killed when a build fails. The page you are looking at keeps working, and the compiler error arrives in the browser as an overlay:

```
BUILD FAILED

build: # example.com/myapp
./main.go:166:22: syntax error: unexpected name is at end of statement

fix it and save — this closes itself. esc to dismiss.
```

Fix it, save, and the overlay disappears as the page reloads. Reloading the browser onto a broken build would replace a working page with a blank one, and the error is already on screen.

## Your application needs no dev-mode code

The dev server configures the app through three environment variables, which `core/app` reads in `app.New`:

| variable | effect |
|---|---|
| `HOWL_ADDR` | the port the app listens on — the dev server picks a free one |
| `HOWL_PUBLIC_DIR` | serve `/static/` from this directory instead of the embedded copy |
| `HOWL_DEV` | publish the live-reload endpoint to the browser |

`HOWL_PUBLIC_DIR` is the one that makes CSS instant: without it your stylesheet is inside the binary via `//go:embed`, so changing it would mean a rebuild. It also switches the static handler into re-read-every-request mode, so nothing is cached from a previous version of the file.

None of them are set unless the dev server sets them, and `Config.PublicDir` / `Config.Addr` do the same thing in code if you would rather be explicit.

## The client half — one message, two jobs

`app.js` loads the dev client only when the shell publishes a `live` endpoint:

```js
if (CONFIG.live) import(CONFIG.live + ".js")
```

The dev client is served by the dev server at **`/_howl/alive.js`**, not embedded in the framework, so a production build ships one dead `if` rather than a kilobyte of code it will never run. `router.ClientConfig(ctx)` fills the field in — no change to your shell.

The stream at **`/_howl/alive`** carries a **monotonic revision**, and the same message both greets a new connection and reloads an existing one:

```
event: alive
data: 1786501533683
```

The client remembers the first number it sees and reloads on anything higher. This is the pattern howl (TypeScript) uses on its own `/_howl/alive` socket, for a reason worth stating plainly:

> **A "reload now" event cannot survive a disconnection.** An event fired while the browser was not listening is simply gone. A laptop that slept through a rebuild, a tab whose connection dropped, or a page left open across a restart of the dev server itself would all sit there stale until you noticed and reloaded by hand.

Comparing a number on every greeting makes that self-correcting: `EventSource` reconnects on its own, the server greets it with the current revision, and if anything happened while it was away the page reloads. The revision is seeded from the clock so a restarted dev server always issues a higher number than the one it replaced, and bumped by at least one so two rebuilds inside the same millisecond are not read as "nothing changed".

Verified: with a page open, stop the dev server, edit a file, start it again — the page reloads itself as soon as the connection comes back.

The other three events are ordinary notifications, since they only make sense while connected:

| event | data | client |
|---|---|---|
| `alive` | revision | reload if higher than the one remembered |
| `css` | — | swap `<link>` elements in place |
| `build-error` | compiler output | show the overlay |
| `build-ok` | — | clear the overlay |

`build-error`, not `error`: `EventSource` dispatches its own connection failures under the name `error`, so the overlay would have appeared every time the stream hiccuped.

## The routes widget

A draggable panel listing every route the application serves, read from the
generated table — click one to navigate, `⌃R` to toggle, position and open state
remembered across reloads. A `.dyn` route gets a box for its parameter, since it
has no single URL to link to; `.client` routes are tagged `wasm`.

It is part of the dev client served from `/_howl/alive.js`, so it exists only
when `howl dev` is in front. A production build has neither the panel nor the
`/_howl/routes.json` endpoint it reads.

## Flags

```
-dir      module root (default ".")
-addr     address to serve on (default ":9000")
-pages    page tree, relative to -dir (default "client/pages")
-static   static files, served from disk in dev (default "client/public")
-module   import path of -pages (default: derived from go.mod)
-pkg      package to build (default ".")
-pre      shell command to run before generating routes
-poll     filesystem poll interval (default 300ms)
-args     arguments passed to the built binary
```

`-module` is derived by finding the nearest `go.mod` above `-dir` and joining the path back down, so an app inside a larger module works without the flag.

`-pre` is for a generation step of your own. The documentation site uses it for its Markdown:

```bash
howl dev -dir www -addr :9001 -pre "go run github.com/mirairoad/howl-go/core/cmd/mddocs"
```

## Why polling

Changes are detected by walking the tree every 300 ms and comparing modification time and size, not by `fsnotify`.

Walking a few hundred files costs well under a millisecond, and it keeps the module's dependency count where it is. It also sidesteps the thing that makes filesystem events fiddly: most editors save by writing a temporary file and renaming it over the original, which arrives as *delete then create* rather than *write*, and needs special handling per platform. Raise `-poll` if the walk ever shows up in a profile.

Generated files — `*_templ.go`, `fsroutes_gen.go`, `docs_gen.go`, `*.wasm` — are excluded from the watch. Without that, generating the route table changes a file, which triggers a rebuild, which generates the route table.

A save that touches several files produces one rebuild: the watcher keeps re-scanning until the tree stops moving before it starts.
