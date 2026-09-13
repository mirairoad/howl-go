// Package otel is OpenTelemetry for a howl-go application: traces of every
// request, page render, endpoint call, guard and document operation, and the
// metrics that fall out of them, exported over OTLP.
//
//	shutdown, err := otel.Setup(ctx, otel.WithServiceName("pack"))
//	defer shutdown(ctx)
//
//	a := app.New(app.Config{
//		Use: []mw.Middleware{
//			otel.HTTP(),                                 // first: everything below is inside the request span
//			mw.RequestID,
//			mw.LogWith(mw.LogOptions{Callers: true, Skip: mw.SkipNoise}),
//			mw.Named("guard.session", guards.Session),   // a guard, as itself in the trace
//		},
//	})
//	api.Register(mux, api.Config{OnError: otel.RecordError}, apis.FsApiRoutes()...)
//
// Setup reads the standard environment and nothing else needs configuring:
//
//	OTEL_EXPORTER_OTLP_ENDPOINT="http://10.19.96.7:4318"
//	OTEL_EXPORTER_OTLP_PROTOCOL="http/protobuf"
//	OTEL_RESOURCE_ATTRIBUTES="service.instance.id=prod,deployment.environment=prod"
//	OTEL_SERVICE_NAME="pack"
//
// OTEL_TRACES_SAMPLER, OTEL_METRIC_EXPORT_INTERVAL, OTEL_EXPORTER_OTLP_HEADERS
// and the rest are honoured by the SDK. OTEL_TRACES_EXPORTER=none or
// OTEL_METRICS_EXPORTER=none turns one signal off. The module speaks
// http/protobuf only — the collector's default port 4318 — and refuses any
// other OTEL_EXPORTER_OTLP_PROTOCOL at Setup rather than silently exporting
// nothing; the grpc exporter and its dependencies are what this module exists
// not to carry.
//
// # What is traced
//
// The framework opens spans through core/observe, which knows nothing about
// this package; Setup installs the tracer that turns them into OpenTelemetry
// spans. What arrives:
//
//	GET /dashboard/{id}                 the request; otelhttp's, named by the route once the route is known
//	├─ guard.session                    a guard wrapped in mw.Named, to the point it handed over
//	├─ howl.render /dashboard/{id}      the page and its layouts; howl.mode=document|fragment|raw, howl.bytes
//	│  └─ db.get users                  a document read the page made; db.collection.name, db.operation.name
//	└─ api Metrics                      an endpoint; howl.endpoint, and howl.api.stage on failure
//	   └─ db.find orders
//
// Events on the request span mark cache.hit / cache.miss (mw.Cache, and a
// document read's cache) and ratelimit.refused. A guard that redirected
// carries howl.redirect and the status it answered with.
//
// # Metrics
//
//	http.server.request.duration      otelhttp; http.route once known
//	howl.render.duration              by howl.route, howl.mode
//	howl.api.duration                 by howl.endpoint
//	howl.api.errors                   by howl.endpoint, howl.api.stage
//	db.client.operation.duration      by db.system.name, db.collection.name, db.operation.name
//	howl.cache                        by howl.kind (http|db), result (hit|miss)
//	howl.ratelimit.refused
//
// All durations are seconds, as the conventions ask.
//
// # Your own spans and logs
//
// Start and Current are core/observe's, re-exported so an application imports
// one package. Tracer and Meter hand back the SDK's, for custom instruments.
// SlogHandler puts trace_id and span_id on every log record made with a
// request context, which is what joins a log line to its trace in the backend;
// exporting logs over OTLP is left to the application.
package otel
