#!/usr/bin/env bash
# check-observability-compliance.sh
#
# Enforces that ALL logs, metrics, and traces in production Go code route
# exclusively through platform-gincommon.  Direct use of stdlib log, raw
# Prometheus constructors, or OpenTelemetry SDK setup outside the approved
# composition roots (cmd/) is rejected.
#
# Nine rules across three signal families:
#
#   LOGGING
#     L-1  No stdlib "log" import in any production file
#     L-2  No direct "go.uber.org/zap" import (platform-gincommon owns Zap)
#     L-3  No fmt.Printf / fmt.Println / fmt.Fprintln (console-print as log)
#     L-4  No fmt.Fprintf(os.Stderr/os.Stdout) (console-write as log)
#
#   METRICS
#     M-1  Prometheus instrument construction only in metrics/metrics.go
#     M-2  No prometheus.DefaultRegisterer/Gatherer in internal/ or pkg/
#     M-3  No prometheus.NewRegistry() in production code
#
#   TRACING
#     T-1  No sdktrace.NewTracerProvider / otel.SetTracerProvider in internal/
#     T-2  No direct OTLP exporter import in internal/ or pkg/
#
# Composition roots (cmd/) are exempted from M-2, T-1, T-2 because wiring
# platform-gincommon's own pipeline requires exactly those primitives at
# startup.  The rules above target adapter / service / domain code that must
# NEVER bypass the platform abstraction.
#
# asyncapi.go is exempted from L-3/L-4: its fmt.Fprintf calls write HTML to
# an http.ResponseWriter (legitimate HTTP response rendering, not logging).
#
# Run:
#   bash .github/scripts/check-observability-compliance.sh
# Exit codes: 0 = pass, 1 = at least one violation.

set -euo pipefail
FAIL=0

METRICS_FILE="internal/adapter/outbound/metrics/metrics.go"
ASYNCAPI_FILE="internal/adapter/inbound/http/asyncapi.go"

# ── LOGGING ──────────────────────────────────────────────────────────────────

# L-1: No stdlib "log" import in any production file.
while IFS= read -r f; do
  if grep -qP '^\s+"log"\s*$' "$f" 2>/dev/null; then
    echo "::error file=${f},title=Observability/Logging [L-1]::stdlib \"log\" import — use port.Logger backed by platform-gincommon/pkg/logger.NewLogger instead" >&2
    FAIL=1
  fi
done < <(find . -name "*.go" ! -name "*_test.go" ! -path "*/vendor/*" | sort)

# L-2: No direct go.uber.org/zap import (platform-gincommon owns the logger).
while IFS= read -r f; do
  if grep -q '"go.uber.org/zap"' "$f" 2>/dev/null; then
    echo "::error file=${f},title=Observability/Logging [L-2]::direct \"go.uber.org/zap\" import — use port.Logger (platform-gincommon); only platform-gincommon/pkg/logger may import zap directly" >&2
    FAIL=1
  fi
done < <(find . -name "*.go" ! -name "*_test.go" ! -path "*/vendor/*" | sort)

# L-3: No fmt.Printf / fmt.Println / fmt.Fprintln (always console-print).
# asyncapi.go exempted (HTML rendering to http.ResponseWriter, not logging).
while IFS= read -r f; do
  [[ "$f" == "./$ASYNCAPI_FILE" || "$f" == "$ASYNCAPI_FILE" ]] && continue
  if grep -qP 'fmt\.(Printf|Println|Fprintln)\b' "$f" 2>/dev/null; then
    lineno=$(grep -nP 'fmt\.(Printf|Println|Fprintln)\b' "$f" | head -1 | cut -d: -f1)
    echo "::error file=${f},line=${lineno},title=Observability/Logging [L-3]::fmt.Printf/Println/Fprintln — use port.Logger for structured output" >&2
    FAIL=1
  fi
done < <(find . -name "*.go" ! -name "*_test.go" ! -path "*/vendor/*" | sort)

# L-4: No fmt.Fprintf(os.Stderr/os.Stdout) — writing to stdio as a log sink.
while IFS= read -r f; do
  if grep -qP 'fmt\.Fprintf\s*\(\s*os\.(Stderr|Stdout)' "$f" 2>/dev/null; then
    lineno=$(grep -nP 'fmt\.Fprintf\s*\(\s*os\.(Stderr|Stdout)' "$f" | head -1 | cut -d: -f1)
    echo "::error file=${f},line=${lineno},title=Observability/Logging [L-4]::fmt.Fprintf(os.Stderr/Stdout) — use port.Logger; write HTTP responses to http.ResponseWriter only" >&2
    FAIL=1
  fi
done < <(find . -name "*.go" ! -name "*_test.go" ! -path "*/vendor/*" | sort)

# ── METRICS ──────────────────────────────────────────────────────────────────

# M-1: Prometheus instrument construction only in metrics/metrics.go.
while IFS= read -r f; do
  [[ "$f" == "./$METRICS_FILE" || "$f" == "$METRICS_FILE" ]] && continue
  if grep -qP 'prometheus\.New(Counter|Histogram|Gauge|Summary)(Vec)?\b' "$f" 2>/dev/null; then
    lineno=$(grep -nP 'prometheus\.New(Counter|Histogram|Gauge|Summary)(Vec)?\b' "$f" | head -1 | cut -d: -f1)
    echo "::error file=${f},line=${lineno},title=Observability/Metrics [M-1]::prometheus instrument constructor outside metrics/metrics.go — declare the collector in metrics/metrics.go and access it through metrics.Live or an injected Metrics interface" >&2
    FAIL=1
  fi
done < <(find . -name "*.go" ! -name "*_test.go" ! -path "*/vendor/*" | sort)

# M-2: No prometheus.DefaultRegisterer/Gatherer in internal/ or pkg/ code.
# cmd/ composition roots are exempted.
while IFS= read -r f; do
  if grep -qP 'prometheus\.Default(Registerer|Gatherer)\b' "$f" 2>/dev/null; then
    lineno=$(grep -nP 'prometheus\.Default(Registerer|Gatherer)\b' "$f" | head -1 | cut -d: -f1)
    echo "::error file=${f},line=${lineno},title=Observability/Metrics [M-2]::prometheus.DefaultRegisterer/Gatherer in internal/pkg — use gincommon.MetricsRegisterer() so collectors land on the platform-gincommon registry" >&2
    FAIL=1
  fi
done < <(find ./internal ./pkg -name "*.go" ! -name "*_test.go" | sort)

# M-3: No prometheus.NewRegistry() in production code.
while IFS= read -r f; do
  if grep -q 'prometheus\.NewRegistry()' "$f" 2>/dev/null; then
    lineno=$(grep -n 'prometheus\.NewRegistry()' "$f" | head -1 | cut -d: -f1)
    echo "::error file=${f},line=${lineno},title=Observability/Metrics [M-3]::prometheus.NewRegistry() in production code — use gincommon.MetricsRegisterer() for the shared platform registry" >&2
    FAIL=1
  fi
done < <(find . -name "*.go" ! -name "*_test.go" ! -path "*/vendor/*" | sort)

# ── TRACING ──────────────────────────────────────────────────────────────────

# T-1: No direct OTel TracerProvider construction or global override in
# internal/ or pkg/.  gincommon.InitTracingFromEnv() is the single approved
# construction point and lives in cmd/.
while IFS= read -r f; do
  if grep -qP '(sdktrace|tracesdk)\.NewTracerProvider|otel\.SetTracerProvider' "$f" 2>/dev/null; then
    lineno=$(grep -nP '(sdktrace|tracesdk)\.NewTracerProvider|otel\.SetTracerProvider' "$f" | head -1 | cut -d: -f1)
    echo "::error file=${f},line=${lineno},title=Observability/Tracing [T-1]::TracerProvider construction/override in internal/pkg — use gincommon.InitTracingFromEnv() in cmd/ only" >&2
    FAIL=1
  fi
done < <(find ./internal ./pkg -name "*.go" ! -name "*_test.go" | sort)

# T-2: No direct OTLP exporter import in internal/ or pkg/.
# OTLP setup is owned by gincommon.InitTracingFromEnv; a second import would
# create a disconnected exporter pipeline.
while IFS= read -r f; do
  if grep -q '"go.opentelemetry.io/otel/exporters/otlp' "$f" 2>/dev/null; then
    echo "::error file=${f},title=Observability/Tracing [T-2]::direct OTLP exporter import in internal/pkg — tracing export is exclusively owned by gincommon.InitTracingFromEnv() in cmd/" >&2
    FAIL=1
  fi
done < <(find ./internal ./pkg -name "*.go" ! -name "*_test.go" | sort)

# ── Result ────────────────────────────────────────────────────────────────────

if [[ $FAIL -eq 0 ]]; then
  echo "Observability compliance checks passed."
  echo "  Logging  : all output through port.Logger / platform-gincommon (L-1..L-4) ✓"
  echo "  Metrics  : all collectors in metrics/metrics.go via gincommon.MetricsRegisterer() (M-1..M-3) ✓"
  echo "  Tracing  : TracerProvider and OTLP owned by gincommon.InitTracingFromEnv() in cmd/ (T-1..T-2) ✓"
fi
exit $FAIL
