package port

// Logger is the structured logging port this service funnels through:
// HTTP middleware (via platform-gincommon), inbound adapters, outbound
// adapters, the SQS consumer, and both binaries. It matches
// platform-gincommon's own port.Logger shape exactly
// (Debug/Info/Warn/Error(msg, fields)), so the single Zap-backed logger
// built once via platform-gincommon's logger.NewLogger (cmd/server/main.go,
// cmd/reconciler/main.go) satisfies it directly — no adapter needed at
// the call site that builds it.
//
// The one exception is platform-pgcommon's Field-based domain.Logger;
// postgres.NewLoggerAdapter wraps this port so slow-query and migration
// lines still land on the same Zap sink.
// Matching iam-user-profile / iam-org-membership.
type Logger interface {
	Debug(msg string, fields map[string]interface{})
	Info(msg string, fields map[string]interface{})
	Warn(msg string, fields map[string]interface{})
	Error(msg string, fields map[string]interface{})
}
