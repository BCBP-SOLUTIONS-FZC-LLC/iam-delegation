package main

import (
	"log/slog"
	"os"

	pgcdomain "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/domain"
)

// slogDomainLogger adapts *slog.Logger to platform-pgcommon's Field-based
// domain.Logger. Duplicated from cmd/server/observability.go — the two
// binaries are separate `main` packages and cannot import each other.
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
// shape this repo's outbound-adapter/jobs packages expect.
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

func newReconcilerLogger(env string) *slog.Logger {
	level := slog.LevelInfo
	if env == "development" {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
