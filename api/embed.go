// Package api embeds this service's machine-readable event contract so the
// running binary can serve it (GET /asyncapi, GET /asyncapi.yaml — see the
// http adapter's asyncapi handlers) without depending on the source tree
// being present at runtime. The final container image copies only the
// compiled binary, not the repo, so a disk-read-relative-path approach would
// 404/500 in that image; embedding avoids the problem entirely.
package api

import _ "embed"

// AsyncAPISpec is the verbatim contents of asyncapi.yaml, embedded at
// compile time.
//
//go:embed asyncapi.yaml
var AsyncAPISpec []byte
