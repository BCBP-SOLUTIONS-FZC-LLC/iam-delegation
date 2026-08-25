package postgres

import (
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/domain"
)

// Logger is the structured logging interface LoggerAdapter wraps. Matches
// gincommon's own port.Logger shape (Debug/Info/Warn/Error(msg, fields)) so
// the same Zap-backed logger built once in each binary's main() can be
// passed straight through, with no further adapter needed on the caller's
// side.
type Logger interface {
	Debug(msg string, fields map[string]interface{})
	Info(msg string, fields map[string]interface{})
	Warn(msg string, fields map[string]interface{})
	Error(msg string, fields map[string]interface{})
}

// LoggerAdapter implements platform-pgcommon's pkg/domain.Logger (Debug/Info/
// Warn/Error(msg, ...domain.Field)) on top of a Logger. Config.Logger and
// migrate.Runner.Logger are typed against this public domain.Logger, so
// pgcommon's slow-query logging and migration log output can be routed into
// this service's own Zap-backed sink instead of going nowhere. Mirrors
// iam-user-profile's/iam-org-membership's identical LoggerAdapter.
type LoggerAdapter struct {
	log Logger
}

var _ domain.Logger = LoggerAdapter{}

// NewLoggerAdapter wraps log as a pgcommon domain.Logger.
func NewLoggerAdapter(log Logger) LoggerAdapter {
	return LoggerAdapter{log: log}
}

// Debug implements domain.Logger.
func (a LoggerAdapter) Debug(msg string, fields ...domain.Field) { a.log.Debug(msg, fieldMap(fields)) }

// Info implements domain.Logger.
func (a LoggerAdapter) Info(msg string, fields ...domain.Field) { a.log.Info(msg, fieldMap(fields)) }

// Warn implements domain.Logger.
func (a LoggerAdapter) Warn(msg string, fields ...domain.Field) { a.log.Warn(msg, fieldMap(fields)) }

// Error implements domain.Logger.
func (a LoggerAdapter) Error(msg string, fields ...domain.Field) { a.log.Error(msg, fieldMap(fields)) }

func fieldMap(fields []domain.Field) map[string]interface{} {
	m := make(map[string]interface{}, len(fields))
	for _, f := range fields {
		m[f.Key] = f.Value
	}
	return m
}
