package main

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

// Logger is the map[string]interface{}-based logging interface
// platform-gincommon, platform-events, and this repo's own outbound-adapter/
// jobs Logger interfaces all share (identical method shape everywhere).
// Satisfied directly by the Zap-backed logger platform-gincommon/pkg/logger.
// NewLogger returns — no adapter needed, unlike the hand-rolled *slog.Logger
// this replaced (mirrors iam-user-profile's/iam-org-membership's pattern).
// platform-pgcommon's own Field-based domain.Logger is the one exception —
// see internal/adapter/outbound/postgres.NewLoggerAdapter.
type Logger interface {
	Debug(msg string, fields map[string]interface{})
	Info(msg string, fields map[string]interface{})
	Warn(msg string, fields map[string]interface{})
	Error(msg string, fields map[string]interface{})
}

// otelSpanTracer adapts an OTel trace.Tracer to the structural
// StartSpan(ctx, name) (context.Context, func()) shape pgcommon.Config.Tracer
// expects.
type otelSpanTracer struct{ tracer trace.Tracer }

func (t otelSpanTracer) StartSpan(ctx context.Context, name string) (spanCtx context.Context, end func()) {
	spanCtx, span := t.tracer.Start(ctx, name)
	return spanCtx, func() { span.End() }
}
