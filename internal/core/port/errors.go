package port

import "errors"

// ErrDependencyUnavailable is the sentinel outbound HTTP-client adapters
// wrap around 5xx/timeout failures (never 4xx business responses), so
// service code can distinguish "the dependency is down" (503) from "the
// dependency answered with a business rejection" (422) via errors.Is.
var ErrDependencyUnavailable = errors.New("dependency_unavailable")
