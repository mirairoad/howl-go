package otel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/mirairoad/howl-go/core/observe"
	sdk "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Option adjusts Setup. The environment wins over every option that overlaps
// with it: options are what the binary knows about itself, the environment is
// what the deployment knows.
type Option func(*config)

type config struct {
	service   string
	attrs     []attribute.KeyValue
	spans     sdktrace.SpanExporter
	reader    sdkmetric.Reader
	noMetrics bool
}

// WithServiceName is the service.name to use when OTEL_SERVICE_NAME is not
// set — the binary's own idea of what it is.
func WithServiceName(name string) Option { return func(c *config) { c.service = name } }

// WithAttributes adds resource attributes below the environment's.
func WithAttributes(attrs ...attribute.KeyValue) Option {
	return func(c *config) { c.attrs = append(c.attrs, attrs...) }
}

// WithSpanExporter replaces the OTLP span exporter — an in-memory one in a
// test, a stdout one to see what would be sent.
func WithSpanExporter(e sdktrace.SpanExporter) Option { return func(c *config) { c.spans = e } }

// WithMetricReader replaces the OTLP metric reader, likewise.
func WithMetricReader(r sdkmetric.Reader) Option { return func(c *config) { c.reader = r } }

// WithoutMetrics exports traces only.
func WithoutMetrics() Option { return func(c *config) { c.noMetrics = true } }

// Setup builds the tracer and meter providers from the standard OTEL_*
// environment, installs them as the SDK's globals and as core/observe's
// tracer, and returns the function that flushes and stops them. Call it once,
// first thing in main; call shutdown last.
func Setup(ctx context.Context, opts ...Option) (shutdown func(context.Context) error, err error) {
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}
	if err := checkProtocol(); err != nil {
		return nil, err
	}

	// Options first, environment merged on top: resource.Merge lets the
	// second argument win, so OTEL_SERVICE_NAME beats WithServiceName and a
	// deployment can rename a binary without rebuilding it.
	mine := []attribute.KeyValue{}
	if cfg.service != "" {
		mine = append(mine, attribute.String("service.name", cfg.service))
	}
	mine = append(mine, cfg.attrs...)
	env, err := resource.New(ctx, resource.WithFromEnv(), resource.WithTelemetrySDK())
	if err != nil {
		return nil, fmt.Errorf("otel: resource: %w", err)
	}
	res, err := resource.Merge(resource.NewSchemaless(mine...), env)
	if err != nil {
		return nil, fmt.Errorf("otel: resource: %w", err)
	}

	var closers []func(context.Context) error

	spans := cfg.spans
	if spans == nil && !off("OTEL_TRACES_EXPORTER") {
		spans, err = otlptracehttp.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("otel: trace exporter: %w", err)
		}
	}
	topts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		// A span may arrive with an id already assigned to it; see Navigations.
		sdktrace.WithIDGenerator(clientIDs{next: randomIDs{}}),
	}
	if spans != nil {
		topts = append(topts, sdktrace.WithBatcher(spans))
	}
	// No WithSampler: the SDK reads OTEL_TRACES_SAMPLER and its ARG itself,
	// and defaults to parent-based always-on.
	tp := sdktrace.NewTracerProvider(topts...)
	closers = append(closers, tp.Shutdown)

	var meter metric.Meter
	mopts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	reader := cfg.reader
	if reader == nil && !cfg.noMetrics && !off("OTEL_METRICS_EXPORTER") {
		exp, err := otlpmetrichttp.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("otel: metric exporter: %w", err)
		}
		reader = sdkmetric.NewPeriodicReader(exp) // interval from OTEL_METRIC_EXPORT_INTERVAL
	}
	if reader != nil {
		mopts = append(mopts, sdkmetric.WithReader(reader))
	}
	mp := sdkmetric.NewMeterProvider(mopts...)
	closers = append(closers, mp.Shutdown)
	meter = mp.Meter(instrumentation)

	sdk.SetTracerProvider(tp)
	sdk.SetMeterProvider(mp)
	sdk.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	t, err := newTracer(tp.Tracer(instrumentation), meter)
	if err != nil {
		return nil, err
	}
	observe.SetDefault(t)

	return func(ctx context.Context) error {
		var errs []error
		for i := len(closers) - 1; i >= 0; i-- {
			errs = append(errs, closers[i](ctx))
		}
		observe.SetDefault(nil)
		return errors.Join(errs...)
	}, nil
}

// checkProtocol refuses a protocol this module does not speak. The
// alternative — exporting nothing while the process believes it is — is the
// failure mode an observability library must not have.
func checkProtocol() error {
	for _, name := range []string{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "OTEL_EXPORTER_OTLP_PROTOCOL"} {
		switch p := strings.TrimSpace(os.Getenv(name)); p {
		case "", "http/protobuf":
		default:
			return fmt.Errorf("otel: %s=%q — this module exports http/protobuf only (the collector's :4318); "+
				"for grpc use go.opentelemetry.io/contrib/exporters/autoexport in your own main", name, p)
		}
	}
	return nil
}

func off(name string) bool { return strings.EqualFold(strings.TrimSpace(os.Getenv(name)), "none") }

// Start opens a span — core/observe's Start, re-exported so an application
// imports one package for the framework's spans and its own.
func Start(ctx context.Context, name string) (context.Context, observe.Span) {
	return observe.Start(ctx, name)
}

// Current is the span on ctx, or a no-op.
func Current(ctx context.Context) observe.Span { return observe.Current(ctx) }

// Tracer is the SDK's tracer for instrumenting with the OpenTelemetry API
// directly, when observe.Span's three methods are not enough.
func Tracer(name string) trace.Tracer { return sdk.Tracer(name) }

// Meter is the SDK's meter, for custom instruments.
func Meter(name string) metric.Meter { return sdk.Meter(name) }

// TraceID is the current trace's id as a hex string, or "" outside a trace —
// for putting on an error page, or in a support ticket.
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}
