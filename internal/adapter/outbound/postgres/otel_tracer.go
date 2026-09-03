package postgres

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// Tracer matches pgcommon's internal port.Tracer shape structurally (that
// interface lives in an internal/ package and cannot be referenced directly
// across module boundaries) — assigning a *otelTracer to pgcommon.Config.Tracer
// satisfies it by structural typing.
type Tracer interface {
	StartSpan(ctx context.Context, name string) (context.Context, func())
}

// otelTracer adapts the process-global TracerProvider that
// gincommon.InitTracingFromEnv / ObservabilityMiddlewares install onto
// pgcommon.Config.Tracer (StartSpan). Every db.query span therefore
// exports through the same OTLP pipeline as HTTP spans — matching
// iam-user-profile's identical wiring.
type otelTracer struct {
	tracer trace.Tracer
}

// NewOTelTracer returns a pgcommon.Config.Tracer backed by gincommon's
// global TracerProvider. serviceName becomes the OTel instrumentation
// scope (typically gincommon.Config.ServiceName).
func NewOTelTracer(serviceName string) Tracer {
	if serviceName == "" {
		serviceName = "iam-delegation"
	}
	return &otelTracer{tracer: otel.Tracer(serviceName)}
}

func (o *otelTracer) StartSpan(ctx context.Context, name string) (spanCtx context.Context, end func()) {
	spanCtx, span := o.tracer.Start(ctx, name)
	return spanCtx, func() { span.End() }
}
