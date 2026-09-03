package service

// Metrics is the optional recorder DelegationService / CascadeService
// increment after successful (or failed-dependency) outcomes. Satisfied
// by *metrics.Metrics. Nil means skip — unit tests that don't wire it
// keep working. Mirrors iam-user-profile incrementing after success,
// but injected so core never imports the outbound metrics adapter.
type Metrics interface {
	RecordCreated(scope string)
	RecordEnded(reason string)
	RecordIdempotencyHit()
	RecordUPAvailabilityFailure(path string)
}
