# Observability

Traces of every request, page render, endpoint call, guard and document operation, and the metrics that fall out of them, exported over OTLP. Nothing in the application opens a span for any of it.

## Two parts

`core/observe` is the framework's vocabulary for "something is happening": a `Span` with three methods — `Set`, `Event`, `End` — and a `Tracer` that opens one. `App.render`, the endpoint pipeline, `mw.Cache`, `mw.RateLimit` and every `db.Service` operation call it. It imports nothing, and its zero state is a no-op: an application that never heard of tracing pays a nil check per operation.

`otel/` is a nested module with its own `go.mod`, like `db/pg` and `desktop`. It holds the OpenTelemetry SDK — some twenty modules — and implements the tracer. The framework's `go.mod` keeps its one dependency.

## Setup

```go
shutdown, err := otel.Setup(ctx, otel.WithServiceName("pack"))
defer shutdown(ctx)

a := app.New(app.Config{
	Use: []mw.Middleware{
		otel.HTTP(),                                // first: the request span
		mw.RequestID,
		mw.LogWith(mw.LogOptions{Callers: true, Skip: mw.SkipNoise}),
		mw.Named("guard.session", guards.Session),  // a guard, as itself in the trace
	},
})
api.Register(mux, api.Config{OnError: otel.RecordError}, apis.FsApiRoutes()...)
slog.SetDefault(slog.New(otel.SlogHandler(slog.Default().Handler())))

log.Fatal(a.Listen(otel.Routes(mux)))
```

The environment does the rest, and it is the standard one:

```
OTEL_EXPORTER_OTLP_ENDPOINT="http://10.19.96.7:4318"
OTEL_EXPORTER_OTLP_PROTOCOL="http/protobuf"
OTEL_RESOURCE_ATTRIBUTES="service.instance.id=prod,deployment.environment=prod"
OTEL_SERVICE_NAME="pack"
```

`OTEL_SERVICE_NAME` beats `WithServiceName`; a deployment renames a binary without rebuilding it. `OTEL_TRACES_SAMPLER`, `OTEL_METRIC_EXPORT_INTERVAL` and `OTEL_EXPORTER_OTLP_HEADERS` are honoured by the SDK. `OTEL_TRACES_EXPORTER=none` or `OTEL_METRICS_EXPORTER=none` turns one signal off.

The module speaks `http/protobuf` only — the collector's `:4318` — and refuses any other `OTEL_EXPORTER_OTLP_PROTOCOL` at `Setup`. Exporting nothing while the process believes it is exporting is the one failure an observability library must not have, and the gRPC exporter's dependency tree is what the module exists not to carry.

## What a trace looks like

```
GET /dashboard/{id}                    the request; named by the route once the mux has matched
├─ guard.session                       a middleware in mw.Named, to the point it handed over
├─ howl.render /dashboard/{id}         the page and its layouts; howl.mode, howl.client, howl.bytes
│  └─ db.get users                     a read the page made; collection, operation, backend
└─ api Metrics                         an endpoint; howl.api.stage on failure
   └─ db.find orders
```

The request span opens named by the method alone — the mux has not matched yet — and is renamed the moment the framework learns the route, in `App.render` or the endpoint pipeline. A static file or a 404 keeps the bare method, which is what the conventions say to do when there is no route.

A guard wrapped in `mw.Named` gets a span that ends the moment it calls the next handler, so its own cost stands apart from the page behind it. One that answers instead — a redirect to sign-in — keeps the span to the end of its reply and records the status and the `Location`: the answer to "why did the dashboard bounce this user".

Events on the request span mark `cache.hit` / `cache.miss` — `mw.Cache`, `//howl:cache`, `Spec.Cache` and a document read's cache alike — and `ratelimit.refused`. A `//howl:cache` hit has no render span at all; the absence is the information. Every traced response carries `X-Trace-Id`, so the trace behind a screenshot of a network tab can be found.

## Every method, and the handlers the framework does not route

A page route is registered `GET` — a page is a document — but nothing else here is GET-only. An endpoint is whatever its file name says: `logs.post.api.go` is a `POST`, and it traces exactly like a `GET`, with the same span, the same stages and the same metrics. `mw.RateLimit`, guards, and every `db` write — `create`, `patch`, `delete`, `patch_where`, `delete_where`, `restore` — are method-blind. `mw.Cache` sees only GET because GET is all it caches.

The exception is a handler registered on the mux **by hand**: a form post to `server/actions`, a fragment endpoint answering an `<li>`, a webhook. Neither `App.render` nor the endpoint pipeline runs for it, so nothing in the framework knows its route, and the span is a bare `POST` with no `http.route` — every hand-written write path in the application collapsing into one unnamed series.

```go
log.Fatal(a.Listen(otel.Routes(mux)))
```

`otel.Routes` reads the pattern the mux matched and names the span after it: `POST /api/todos`, `DELETE /api/todos/{id}`. The path template, not the URL — one time series for a route, not one per id.

It has to wrap the mux directly, with nothing in between. `ServeMux` sets `Request.Pattern` in place on the request it was handed, so a middleware that clones the request — `mw.RequestID`, or anything calling `WithContext` — hides the pattern from everything outside it. `a.Listen` applies `Use` around the handler you give it, which puts the request span outside and this inside, where it belongs.

## The database

Every `db.Service` call is a span, and the backend call is a span inside it:

```
db.get users              the operation — cache, decoding, the wait
└─ sql.find_one users     the backend call
```

The gap between the two is the service's own cost: validating, building keys, unmarshalling, or waiting on another caller's query. Without it a slow unmarshal and a slow database look identical.

Reads (`get`, `get_many`, `find`, `one`, `count`), writes (`create`, `patch`, `delete`, `restore`, `patch_where`, `delete_where`) and maintenance (`report`, `drop_field`, `columns`, `drop_column`) all open one. Beyond the collection, backend and operation name, each carries what only that operation knows:

| attribute | what it answers |
|---|---|
| `howl.db.rows` | how many documents came back — "8" and "8000" being the interesting difference |
| `howl.cache.hits` / `howl.cache.misses` | how many lookups hit. Counted per operation, not one event per lookup: `GetMany` of fifty ids is one span with two numbers |
| `howl.db.session` | the call is inside a transaction, so the cache was bypassed in both directions |
| `howl.db.attempts` | how many times a patch retried its optimistic lock; reaching 3 is `ErrConflict` about to happen |
| `howl.db.coalesced` | this read waited on another caller's identical query and issued none of its own — a slow span with nothing under it, explained |
| `howl.db.bulk` | `native` when the backend wrote the set in one statement, `fallback` when the service walked the rows one at a time |

The document id and the row count stay attributes, never metric labels.

## The query, recorded and redacted

`db.query.text` carries the filter and `howl.db.set` what an update wrote — the two things that answer "which documents did that touch, and what did it do to them". A `DeleteWhere` that hit more rows than anyone expected is unreadable without them.

The shape is always recorded: field names, operators, how many elements an `in` clause had. Those are the schema, not the data. What `db.Options.Trace` decides is the values.

| mode | values |
|---|---|
| `observe.Safe` — the zero value | numbers, booleans, null, and strings that are identifiers: a UUID (howl's own ids are UUIDv7), a ULID, a hex digest, digits. Everything else becomes `"?"` |
| `observe.Full` | every value, except the two rules below. For a development machine, or a service holding no personal data |
| `observe.Off` | nothing — not the values, not the field names |

Two rules hold in **every** mode, `Full` included:

1. **A sensitive field name redacts its whole subtree.** `password`, `api_key`, `email`, `phone`, `dob`, `card`, `iban`, `session` and the rest, matched by word rather than by substring — so `api_key` and `key` are sensitive while `keyword` and `monkey` are not. A word break is punctuation *or* a case change, because a JSON document is as likely to say `userEmail` as `user_email`; splitting only on punctuation made every camelCase spelling ordinary, and `phoneNumber` holding digits is a value Safe would otherwise have kept, digits being how an id looks.
2. **Anything that looks like an email address is never recorded**, wherever it appears: under an innocent field name, nested inside an `$or`, inside an array, even in key position. The test for it asserts exactly that, in both modes, at every depth — because this is the one piece of personal data that is recognisable without knowing the schema, and the one that turns a trace into a mailing list.

```
{"limit":20,"sort":{"created":-1},"where":{"$and":[{"email":{"$eq":"?"}},
 {"org_id":{"$eq":"0193a5c2-5f4e-7b3a-9c1d-2e6f8a0b4c7d"}}],"meta.deleted_at":null}}
```

Three details that make that readable. The id is kept by name in `howl.db.id`, because an id is how the document is found again — in the database and in the next trace. Sort and projection fields go in **key** position (`{"created":-1}`), since a key is schema and a string value is data; written as `["-created"]` they would render as `["?"]` and say nothing. And an `in` clause over five hundred ids renders eight of them followed by `"+492 more"` — the count being the difference between two queries with the same shape. Output is capped at 1 KiB, depth at 6.

An RFC 3339 **date-time** counts as an identifier, so a range query stays legible under Safe:

```
{"meta.created_at":{"$lt":"2026-01-02T03:04:05Z"}}
```

That is the difference between knowing a bulk delete swept by date and knowing what it deleted — which is the question being asked the moment somebody notices the row count. A moment names no one.

A bare **date** does not count: `1985-03-12` is how a date of birth is written, and `2026-01-02T03:04:05Z` is not how anybody writes one by hand. A DOB stored as a `time.Time` does marshal to the second form, so the field name is what stops it — `dob`, `birth`, `birthdate`, `birthday` and `born` are denied in every spelling, `dateOfBirth` and `date_of_birth` alike.

Endpoints do the same for what the caller sent — `howl.api.query` and `howl.api.body`, under `api.Config.Trace` — recorded *before* `Validate` runs, because a call refused for being wrong is exactly the one whose arguments you want to see.

**Nothing is rendered unless a tracer is installed.** Measured on `db.Find` against the in-memory backend: untraced is byte-identical to `Trace: observe.Off` — 16.1 µs and 50 allocations — while with a tracer attached it is 27.2 µs, of which rendering the filter is 2.3 µs and 22 allocations. Against a backend that talks to a real database over a socket, that is noise.

`One` gets its own span with the `Find` it delegates to nested inside, because a trace should say which of the two the caller wrote, and "not found" is `One`'s answer rather than `Find`'s.

## The browser's half

A navigation starts its trace in the browser, so a click and the database query behind it end up in one trace rather than two.

`app.js` mints a W3C `traceparent` per navigation — sixteen random bytes and eight, from `crypto.getRandomValues`, with no SDK to load first — and sends it on everything that navigation causes: the fragment fetch, the route's `//howl:data` endpoint, and any request the page makes from Go, since `core/dom` reads `howl.traceparent()` and the generated API client goes through it. The server's propagator joins that trace, so the request span is already a child of the navigation before the navigation has even been reported.

What the server cannot see is the rest: how long the swap took, and the navigations that never reached it at all.

```go
mux.Handle("POST /api/telemetry/navigations", otel.Navigations())

a := app.New(app.Config{Telemetry: "/api/telemetry/navigations", ...})
log.Fatal(a.Listen(otel.Routes(mux)))
```

`Config.Telemetry` is published in the `howl-client` JSON the shell embeds, and nothing reports unless it is set — the same shape the dev client's reload endpoint uses. `app.js` batches rows for five seconds and flushes on `pagehide` through `sendBeacon`, which survives the document being torn down; a browser that refuses the beacon gets a `keepalive` POST instead. Either way it is fire-and-forget, so a telemetry endpoint that is down cannot slow a page down.

`otel.Navigations` creates each span with **the browser's own span id** rather than a fresh one. That is the whole trick: the request the server recorded arrived parented to that id, so the navigation ends up containing it.

```
howl.navigate /dashboard/metrics    220 ms   howl.mode=fragment  howl.bytes=1834
└─ GET /dashboard/metrics            18 ms
   └─ howl.render /dashboard/metrics  3 ms
      └─ db.find orders               9 ms
```

A `howl.mode=wasm` row has no child at all, by construction — the server was never contacted — and that row is the only evidence the framework's fastest path ever ran.

Every navigation also dispatches `howl:navigated` on the document with `{path, mode, ms, bytes, traceId, spanId}`, whether or not telemetry is configured, so an application that ships its numbers somewhere else can listen for that instead.

## Metrics

| metric | labelled by |
|---|---|
| `http.server.request.duration` | otelhttp's; `http.route` once known |
| `howl.render.duration` | `howl.route`, `howl.mode` |
| `howl.api.duration`, `howl.api.errors` | `howl.endpoint`, and `howl.api.stage` on errors |
| `db.client.operation.duration` | `db.system.name`, `db.collection.name`, `db.operation.name` |
| `howl.db.storage.duration` | the same three — the backend's share of the operation above |
| `howl.cache` | `howl.kind` (`http` or `db`), `result` (`hit` or `miss`), `db.collection.name` |
| `howl.ratelimit.refused` | — |

Two cache outcomes have no operation to hang them on, and come from each service's own counters instead:

```go
otel.WatchCache(users, orders, invoices)
```

`howl.db.cache.bypassed` counts reads that could not consult the cache at all, because the shared version was unreadable — a broken `Versioner`, which otherwise looks exactly like a cache that is merely cold. `howl.db.cache.too_large` counts results returned but never stored, being over `Cache.MaxEntryBytes` — the unbounded `Find` that will never be cached however often it is asked for. Both are observable counters read at collection time, so they cost nothing per operation.

Durations are in seconds. The metrics are derived from the spans at `End`: a span the framework opened says what it is in `howl.kind`, and the tracer turns its duration into the histogram for that kind. One set of hooks, and a trace and a metric cannot disagree about what happened. Labels are the handful of bounded attributes above — an id or a byte count goes on the span only, because a label with unbounded values is how a metrics backend runs out of memory.

## Your own spans and logs

```go
ctx, span := otel.Start(ctx, "import.csv")
span.Set("rows", n)
defer span.End(err)
```

`otel.Start` is `observe.Start` re-exported, so an application imports one package for the framework's spans and its own. `otel.Tracer(name)` and `otel.Meter(name)` hand back the SDK's objects for anything three methods cannot say.

`otel.SlogHandler` wraps any `slog.Handler` — `console`'s included — and adds `trace_id` and `span_id` to every record logged with a request context. Two keys, and a log line in the backend joins its trace. Exporting the records themselves over OTLP is the application's decision; the module keeps them wherever they already go.

## Rules

- `otel.HTTP()` goes first in `Use`. Everything after it is inside the request span.
- The mux goes in `otel.Routes`, or every hand-written handler traces as a bare method.
- Do not open spans for what is already traced: a request, a render, an endpoint, a db operation.
- A guard is `mw.Named("guard.<name>", guard)`, not a hand-opened span.
- Nothing under `client/` may import `core/observe`. A page compiles into the wasm build too, where a span has no exporter. `howl check` reports it.
