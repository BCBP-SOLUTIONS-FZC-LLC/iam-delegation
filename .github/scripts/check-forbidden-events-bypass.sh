#!/usr/bin/env bash
# Events/outbox/dedup CI gate: reject any application code that talks to
# SNS/SQS directly, or hand-builds an events.Envelope, instead of going
# through platform-events.
#
# The only legitimate way to publish is eventbus.Publisher.EnqueueCtx
# (events.NewEnvelope + outbox.Enqueue); the only legitimate way to send to
# SNS is events.NewSNSPublisher; the only legitimate way to consume from SQS
# is events.NewSQSConsumerWithClient (internal/adapter/inbound/consumer/
# wiring.go). cmd/server/main.go is the one place allowed to construct a raw
# *sqs.Client — solely to hand it to NewCascadeSQSConsumer — and it must
# never call a transport method on it directly.
#
# Consumer-side dedup (processed_events) is intentionally NOT covered here:
# platform-events has no consumer-side idempotency mechanism (only the
# publish-side, SNS-FIFO-only WithMessageDeduplicationID), so hand-rolling
# that table is the correct pattern, not a bypass.
#
# Test files are exempt: e2e/integration tests legitimately talk to SNS/SQS
# directly to simulate external actors (Core, Tender, ...) that this
# service does not own.
set -euo pipefail

# ── 1. aws-sdk-go-v2 SNS/SQS packages may only be imported where the raw
#      client is constructed to hand off to platform-events. ─────────────
ALLOWED_SDK_IMPORT_FILES="cmd/server/main.go"

sdk_import_hits=$(grep -RIl -E '"github\.com/aws/aws-sdk-go-v2/service/(sns|sqs)"' \
  --include='*.go' cmd/ internal/ pkg/ 2>/dev/null | grep -v '_test\.go$' || true)

for f in $sdk_import_hits; do
  allowed=false
  for a in $ALLOWED_SDK_IMPORT_FILES; do
    if [ "$f" = "$a" ]; then
      allowed=true
      break
    fi
  done
  if [ "$allowed" = false ]; then
    echo "::error file=${f}::Imports aws-sdk-go-v2 SNS/SQS directly. Only ${ALLOWED_SDK_IMPORT_FILES} may construct a raw client (to hand to platform-events) — publish via events.NewSNSPublisher, consume via events.NewSQSConsumerWithClient (internal/adapter/inbound/consumer/wiring.go)."
    exit 1
  fi
done

# ── 2. Even in an allowed file, the SNS/SQS transport methods themselves
#      must never be called directly — only used to build the client. ────
method_hits=$(grep -REn '\.(SendMessage|ReceiveMessage|DeleteMessage|ChangeMessageVisibility|Publish|Subscribe)\(' \
  --include='*.go' cmd/ internal/ pkg/ 2>/dev/null | grep -v '_test\.go' || true)

if [ -n "$method_hits" ]; then
  echo "::error::Direct SNS/SQS transport method call detected outside platform-events. Publish via outbox.Enqueue (eventbus.Publisher.EnqueueCtx); consume via events.NewSQSConsumerWithClient's registered handler — never call the AWS SDK client's own methods."
  echo "$method_hits"
  exit 1
fi

# ── 3. events.Envelope must only ever be constructed via events.NewEnvelope,
#      never as a struct literal (which would skip its ID/time/schema
#      defaulting). ────────────────────────────────────────────────────────
envelope_literal_hits=$(grep -REn 'events\.Envelope(\[[^]]*\])?\{' \
  --include='*.go' cmd/ internal/ pkg/ 2>/dev/null | grep -v '_test\.go' || true)

if [ -n "$envelope_literal_hits" ]; then
  echo "::error::events.Envelope constructed as a struct literal instead of events.NewEnvelope(...). This skips platform-events' own ID/time/schema-version defaulting."
  echo "$envelope_literal_hits"
  exit 1
fi

echo "Events/outbox bypass check passed — no direct SNS/SQS transport calls or hand-built envelopes found outside platform-events."
