#!/usr/bin/env bash
# Floci ready-hook — provisions this service's outbound SNS topic, its one
# inbound SQS queue, the downstream fan-out queues owned by consuming
# services, and the Glue Schema Registry + all 4 schemas (LLD §10). Runs
# automatically on container start via the volume mount to
# /etc/floci/init/ready.d/.
#
#   OUTBOUND (this service publishes):
#   - iam-delegation-events SNS topic — DelegationStarted, DelegationEnded,
#     DelegationReviewRequested, DelegationEscalationRequested (Bug 2a) via
#     platform-events transactional outbox (§10.4)
#
#   DOWNSTREAM SUBSCRIBERS (owned by other services; created here for local dev):
#   - delegation-workflow-q / -dlq      — Workflow Service
#       filter: EventType IN [DelegationStarted, DelegationEnded, DelegationEscalationRequested]
#   - delegation-notification-q / -dlq  — Notification Service
#       filter: EventType IN [DelegationStarted, DelegationEnded, DelegationReviewRequested, DelegationEscalationRequested]
#   - delegation-audit-q / -dlq         — Audit Log Service
#       filter: none (receives all four types)
#
#   INBOUND (this service consumes):
#   - delegation-cascade-q / -dlq       — MembershipRevoked, TenantMembershipsPurged
#       (published by iam-org-membership) and UserUpdated (published by
#       iam-user-profile, Bug 2/DLG-D26, a second upstream topic feeding the
#       same queue) — send directly to exercise the consumer, no SNS
#       subscription is modeled by this script for the inbound side
#
#   GLUE SCHEMA REGISTRY (LLD §7.6.7-adjacent event contract, DLG-D20/D21):
#   - iam-delegation-events registry — 4 schemas, one per published event
#     type, registered from the same JSON Schema Draft-07 files
#     eventbus.GlueCodec ships, so GLUE_REGISTRY_NAME can stay set in
#     docker-compose.yml and the app runs with the real Glue wire-format
#     codec locally — Floci includes Glue Schema Registry in its free tier
#     (unlike LocalStack Community, which gated it behind Pro), so there's
#     no NoopCodec fallback needed for local dev.
#
#   To send a test message to the inbound queue:
#     aws --endpoint-url http://localhost:4570 --region ap-south-1 sqs send-message \
#       --queue-url <cascade-q-url> --message-body \
#       '{"id":"...","type":"MembershipRevoked","source":"iam-org-membership",
#         "specversion":"1","tenant_id":"...","time":"...","data":{"user_id":"..."}}'
#
# Matches api/asyncapi.yaml. The docker-compose `floci` service mounts this
# repo's internal/eventschema/ directory read-only at
# /etc/floci/init/schemas so this script can read the schema definitions
# straight from source — one place to update when a schema changes.

set -euo pipefail

AWS_ACCOUNT=000000000000
AWS_REGION=ap-south-1
MAX_RECEIVES=5
SCHEMAS_DIR=/etc/floci/init/schemas

# The AWS CLI baked into the floci compat image defaults AWS_DEFAULT_REGION
# to us-east-1 regardless of FLOCI_DEFAULT_REGION — pin both region env vars
# so every `aws` call below lands in ap-south-1.
export AWS_DEFAULT_REGION="$AWS_REGION"
export AWS_REGION="$AWS_REGION"

queue_arn() { printf 'arn:aws:sqs:%s:%s:%s' "$AWS_REGION" "$AWS_ACCOUNT" "$1"; }

# ── Helper: provision queue + DLQ (no SNS subscription) ──────────────────────
provision_queue() {
  local queue="$1"
  local dlq="${queue}-dlq"

  aws sqs create-queue --queue-name "$dlq" >/dev/null

  local attrs
  attrs=$(mktemp)
  cat >"$attrs" <<EOF
{
  "RedrivePolicy": "{\"deadLetterTargetArn\":\"$(queue_arn "$dlq")\",\"maxReceiveCount\":\"${MAX_RECEIVES}\"}"
}
EOF
  aws sqs create-queue --queue-name "$queue" --attributes "file://$attrs" >/dev/null
  rm -f "$attrs"

  echo "  Queue: $(aws sqs get-queue-url --queue-name "$queue" --output text)"
  echo "  DLQ:   $(aws sqs get-queue-url --queue-name "$dlq" --output text)"
}

# ── Helper: provision queue + DLQ + SNS subscription with optional filter ─────
# $1 = queue name   $2 = SNS filter policy JSON (empty string = no filter)
# RawMessageDelivery=true so consumers receive the event envelope directly,
# not wrapped in an SNS notification object — mirrors iam-org-membership's
# subscribe_queue helper.
provision_subscriber() {
  local queue="$1"
  local filter_policy="$2"

  provision_queue "$queue"

  local queue_arn_val
  queue_arn_val=$(queue_arn "$queue")

  local sub_arn
  sub_arn=$(aws sns subscribe \
    --topic-arn "$TOPIC_ARN" \
    --protocol sqs \
    --notification-endpoint "$queue_arn_val" \
    --attributes RawMessageDelivery=true \
    --query SubscriptionArn --output text)

  if [ -n "$filter_policy" ]; then
    local attrs
    attrs=$(mktemp)
    printf '%s' "$filter_policy" >"$attrs"
    aws sns set-subscription-attributes \
      --subscription-arn "$sub_arn" \
      --attribute-name FilterPolicy \
      --attribute-value "file://$attrs" >/dev/null
    rm -f "$attrs"
    echo "  Subscribed: $queue  filter=$filter_policy"
  else
    echo "  Subscribed: $queue  (no filter — receives all event types)"
  fi
}

# ── Helper: register a Glue schema (idempotent create-registry, then create-schema) ─
register_schema() {
  local file="$1" name="$2"
  aws glue create-schema \
    --registry-id "RegistryName=iam-delegation-events" \
    --schema-name "$name" \
    --data-format JSON \
    --compatibility BACKWARD \
    --schema-definition "file://${SCHEMAS_DIR}/${file}" >/dev/null
  echo "Glue schema $name created (from $(basename "$file"))"
}

# ── Outbound SNS topic ────────────────────────────────────────────────────────
TOPIC_ARN=$(aws sns create-topic --name iam-delegation-events --query TopicArn --output text)
echo "SNS topic: $TOPIC_ARN"

# ── Inbound queue (this service consumes) ─────────────────────────────────────
echo ""
echo "==> Inbound (this service consumes):"
provision_queue "delegation-cascade-q"

# ── Downstream fan-out subscribers ────────────────────────────────────────────
# SNS filter policies use the EventType MessageAttribute (PascalCase) that the
# outbox stamped on each SNS publish call — matching api/asyncapi.yaml §bindings.

echo ""
echo "==> Downstream subscribers (Workflow, Notification, Audit):"

# Workflow Service — reroute on DelegationStarted, restore on DelegationEnded;
# DelegationEscalationRequested (Bug 2a) is this service's hook for Workflow
# to build a hold/escalate fallback on — not yet consumed as of this
# revision (tracked as a cross-team dependency, LLD §11.5b), included here
# for local-dev parity with api/asyncapi.yaml's documented fan-out anyway.
provision_subscriber "delegation-workflow-q" \
  '{"EventType":["DelegationStarted","DelegationEnded","DelegationEscalationRequested"]}'

# Notification Service — all lifecycle events including review warnings and
# delegate-disable escalations (Bug 2a, tenant_admin/tenant_owner only)
provision_subscriber "delegation-notification-q" \
  '{"EventType":["DelegationStarted","DelegationEnded","DelegationReviewRequested","DelegationEscalationRequested"]}'

# Audit Log — all four event types, no filter (immutable audit trail)
provision_subscriber "delegation-audit-q" ""

# ── Glue Schema Registry — registered LAST, deliberately ─────────────────────
# The floci healthcheck (docker-compose.yml) polls for the last schema
# registered here (DelegationEscalationRequested) to decide the container is
# "healthy" and unblock iam-delegation's own `depends_on: condition:
# service_healthy`. Registering Glue after every SNS/SQS resource above (not
# before, as an earlier revision of this script did) makes that healthcheck
# a true signal that the FULL provisioning run — topic, inbound queue, and
# all downstream subscriptions — has actually completed, not just the Glue
# portion; matches iam-org-membership's scripts/init-floci.sh ordering.
aws glue create-registry --registry-name iam-delegation-events >/dev/null
echo "Glue registry iam-delegation-events created"

register_schema delegation_started.json              DelegationStarted
register_schema delegation_ended.json                 DelegationEnded
register_schema delegation_review_requested.json      DelegationReviewRequested
register_schema delegation_escalation_requested.json  DelegationEscalationRequested

echo ""
echo "Floci init complete."
echo ""
echo "Resources:"
echo "  Glue registry : iam-delegation-events (4 schemas)"
echo "  SNS topic     : $TOPIC_ARN"
echo "  Inbound  q    : delegation-cascade-q  (MembershipRevoked / TenantMembershipsPurged / UserUpdated)"
echo "  Subscriber    : delegation-workflow-q      (DelegationStarted, DelegationEnded, DelegationEscalationRequested)"
echo "  Subscriber    : delegation-notification-q  (DelegationStarted, DelegationEnded, DelegationReviewRequested, DelegationEscalationRequested)"
echo "  Subscriber    : delegation-audit-q         (all events — no filter)"
