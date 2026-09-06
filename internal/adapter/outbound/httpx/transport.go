// Package httpx supplies the single HTTP transport every outbound adapter
// in this service builds on, so W3C `traceparent` injection and client-span
// emission cannot be forgotten on a new call site. Without it an outbound
// call starts a fresh, parentless trace at the callee and a create → User
// Profile / membership-check chain stops being one trace.
//
// Copied from iam-realm-provisioner's identical package: both composition
// roots call gincommon.InitTracingFromEnv unconditionally, so the global
// propagator and tracer provider are already installed.
package httpx

import (
	"io"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// MaxResponseBodyBytes bounds how much of an outbound response body this
// service will ever decode. Every peer here (User Profile, Org Membership)
// returns a small JSON object, so 1 MiB is generous headroom, not a tuned
// limit — its purpose is only to cap worst-case memory use if a mesh peer
// misbehaves or is compromised, not to accommodate any expected payload
// size.
const MaxResponseBodyBytes = 1 << 20

// LimitBody wraps body in an io.LimitReader capped at MaxResponseBodyBytes,
// so json.NewDecoder(...).Decode can never be made to buffer an unbounded
// amount of memory from a single response.
func LimitBody(body io.Reader) io.Reader {
	return io.LimitReader(body, MaxResponseBodyBytes)
}

// NewTransport wraps base — nil meaning http.DefaultTransport — with the
// OpenTelemetry round-tripper, which injects the traceparent header carried
// on the request context and records a client span per call.
func NewTransport(base http.RoundTripper) http.RoundTripper {
	return otelhttp.NewTransport(base)
}

// NewClient returns an http.Client with the instrumented transport and the
// given per-request timeout.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: NewTransport(nil)}
}

// Instrument wraps an existing client's transport with NewTransport. A nil
// client becomes NewClient(timeout). The input client is not mutated.
func Instrument(c *http.Client, timeout time.Duration) *http.Client {
	if c == nil {
		return NewClient(timeout)
	}
	clone := *c
	clone.Transport = NewTransport(c.Transport)
	if clone.Timeout == 0 && timeout > 0 {
		clone.Timeout = timeout
	}
	return &clone
}
