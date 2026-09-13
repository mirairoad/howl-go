package otel

import (
	"context"

	sdk "go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// forceFlush pushes batched spans to the exporter now.
func forceFlush() error {
	if tp, ok := sdk.GetTracerProvider().(*sdktrace.TracerProvider); ok {
		return tp.ForceFlush(context.Background())
	}
	return nil
}
