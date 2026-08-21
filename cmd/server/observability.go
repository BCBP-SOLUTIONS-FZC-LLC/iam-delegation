package main

import (
	"context"
	"log/slog"
	"os"

	pgcdomain "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/domain"
	"go.opentelemetry.io/otel/trace"
)

// slogDomainLogger adapts *slog.Logger to platform-pgcommon's Field-based
// domain.Logger — a different shape from the map[string]interface{}-based
// Logger used everywhere else in this process (gincommon, platform-events,
// and this repo's own outbound-adapter Logger interfaces). Mirrors
// iam-tender-acl's identical adapter (cmd/tender-acl/observability.go).
type slogDomainLogger struct{ l *slog.Logger }

func (a slogDomainLogger) Debug(msg string, fields ...pgcdomain.Field) {
	a.l.Debug(msg, domainFieldsToArgs(fields)...)
}
func (a slogDomainLogger) Info(msg string, fields ...pgcdomain.Field) {
	a.l.Info(msg, domainFieldsToArgs(fields)...)
}
func (a slogDomainLogger) Warn(msg string, fields ...pgcdomain.Field) {
	a.l.Warn(msg, domainFieldsToArgs(fields)...)
}
func (a slogDomainLogger) Error(msg string, fields ...pgcdomain.Field) {
	a.l.Error(msg, domainFieldsToArgs(fields)...)
}

func domainFieldsToArgs(fields []pgcdomain.Field) []any {
	args := make([]any, 0, len(fields)*2)
	for _, f := range fields {
		args = append(args, f.Key, f.Value)
	}
	return args
}

// mapLogger adapts *slog.Logger to the map[string]interface{}-based Logger
// shape platform-gincommon, platform-events, and this repo's own
// valkey/userprofile/orgmembership/consumer/jobs packages all structurally
// expect — one shared adapter satisfies every one of them (Go interface
// satisfaction is structural).
type mapLogger struct{ l *slog.Logger }

func (a mapLogger) Debug(msg string, fields map[string]interface{}) {
	a.l.Debug(msg, mapFieldsToArgs(fields)...)
}
func (a mapLogger) Info(msg string, fields map[string]interface{}) {
	a.l.Info(msg, mapFieldsToArgs(fields)...)
}
func (a mapLogger) Warn(msg string, fields map[string]interface{}) {
	a.l.Warn(msg, mapFieldsToArgs(fields)...)
}
func (a mapLogger) Error(msg string, fields map[string]interface{}) {
	a.l.Error(msg, mapFieldsToArgs(fields)...)
}

func mapFieldsToArgs(fields map[string]interface{}) []any {
	args := make([]any, 0, len(fields)*2)
	for k, v := range fields {
		args = append(args, k, v)
	}
	return args
}

// otelSpanTracer adapts an OTel trace.Tracer to the structural
// StartSpan(ctx, name) (context.Context, func()) shape pgcommon.Config.Tracer
// expects.
type otelSpanTracer struct{ tracer trace.Tracer }

func (t otelSpanTracer) StartSpan(ctx context.Context, name string) (spanCtx context.Context, end func()) {
	spanCtx, span := t.tracer.Start(ctx, name)
	return spanCtx, func() { span.End() }
}

func newLogger(env string) *slog.Logger {
	level := slog.LevelInfo
	if env == "development" {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
