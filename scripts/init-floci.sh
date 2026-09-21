#!/usr/bin/env bash
# Floci ready-hook — provisions this service's outbound SNS topic, its
# inbound SQS queue, the downstream fan-out subscriber queues, and the
# Glue Schema Registry with all four event schemas. Runs automatically
# on container start via the volume mount to /etc/floci/init/ready.d/.
#
# Unlike LocalStack Community, Floci includes Glue Schema Registry in its
# free tier — no Pro token required. GlueCodec therefore runs with the
# real wire format (18-byte header) locally by default.
#
# Topology (matches api/asyncapi.yaml):
#
#   OUTBOUND (this service publishes):
#   - iam-delegation-events SNS topic — DelegationStarted, DelegationEnded,
#     DelegationReviewRequested, DelegationEscalationRequested via the
#     platform-events transactional outbox (LLD §10.4)
#
#   DOWNSTREAM SUBSCRIBERS (owned by other services; created here for local dev):
#   - delegation-workflow-q / -dlq      — Workflow Service
#       filter: EventType IN [DelegationStarted, DelegationEnded, DelegationEscalationRequested]
#   - delegation-notification-q / -dlq  — Notification Service
#       filter: EventType IN [DelegationStarted, DelegationEnded, DelegationReviewRequested, DelegationEscalationRequested]
#   - delegation-audit-q / -dlq         — Audit Log Service (no filter — all four types)
#   - delegation-events-user-profile-q / -dlq — User Profile Service
#       filter: EventType IN [DelegationEnded] (Gap 7 Option C self-healing pointer clear)
#
#   INBOUND (this service consumes):
#   - delegation-cascade-q / -dlq — two upstream SNS topics:
#       1. iam.membership.events (Core) — MembershipRevoked, TenantMembershipsPurged
#          The SNS→SQS subscription is provisioned in iam-org-membership's
#          init-floci.sh (Gap OM-2 fix, Option A — the topic owner creates all
#          downstream subscriptions). For isolated local dev without org-membership
#          running, send directly to the SQS queue:
#            aws --endpoint-url http://localhost:4570 sqs send-message \
#              --queue-url http://localhost:4570/000000000000/delegation-cascade-q \
#              --message-body '...'
#       2. iam.user.events (User Profile) — UserUpdated{status:disabled}
#          This script creates the topic if absent and subscribes
#          delegation-cascade-q to it with an EventType=UserUpdated filter
#          (Gap 1 fix / DLG-D26).
#
#   GLUE SCHEMA REGISTRY:
#   - iam-delegation-events registry with 4 schemas populated from
#     internal/eventschema/ (mounted read-only at /etc/floci/init/schemas)

set -euo pipefail

AWS_REGION=ap-south-1
AWS_ACCOUNT=000000000000
MAX_RECEIVES=5
SCHEMAS_DIR=/etc/floci/init/schemas

# The AWS CLI baked into the floci compat image defaults AWS_DEFAULT_REGION
# to us-east-1 regardless of FLOCI_DEFAULT_REGION — pin both so every `aws`
# call below lands in ap-south-1, matching FLOCI_DEFAULT_REGION on the container.
export AWS_DEFAULT_REGION="$AWS_REGION"
export AWS_REGION="$AWS_REGION"

# ── Helpers ───────────────────────────────────────────────────────────────────

topic_arn() { printf 'arn:aws:sns:%s:%s:%s' "$AWS_REGION" "$AWS_ACCOUNT" "$1"; }
queue_arn()  { printf 'arn:aws:sqs:%s:%s:%s' "$AWS_REGION" "$AWS_ACCOUNT" "$1"; }

# create_queue_with_dlq <base-queue-name>
# Creates <base>-dlq then <base> with RedrivePolicy → <base>-dlq (maxReceiveCount=5).
create_queue_with_dlq() {
  local queue="$1"
  local dlq="${queue}-dlq"

  aws sqs create-queue --queue-name "$dlq" >/dev/null
  local dlq_arn
  dlq_arn=$(queue_arn "$dlq")

  local attrs
  attrs=$(mktemp)
  cat > "$attrs" <<EOF
{
  "RedrivePolicy": "{\"deadLetterTargetArn\":\"${dlq_arn}\",\"maxReceiveCount\":\"${MAX_RECEIVES}\"}"
}
EOF
  aws sqs create-queue --queue-name "$queue" --attributes "file://$attrs" >/dev/null
  rm -f "$attrs"
}

# subscribe_queue <topic-name> <queue-name> [filter-policy-json]
# Creates an SNS→SQS subscription with RawMessageDelivery=true and an
# optional EventType filter policy — mirrors iam-org-membership's helper.
subscribe_queue() {
  local topic="$1" queue="$2" filter="${3:-}"
  local t_arn q_arn
  t_arn=$(topic_arn "$topic")
  q_arn=$(queue_arn "$queue")

  local sub_arn
  sub_arn=$(aws sns subscribe \
    --topic-arn "$t_arn" \
    --protocol sqs \
    --notification-endpoint "$q_arn" \
    --attributes RawMessageDelivery=true \
    --query 'SubscriptionArn' --output text)

  if [ -n "$filter" ]; then
    local attrs
    attrs=$(mktemp)
    printf '%s' "$filter" > "$attrs"
    aws sns set-subscription-attributes \
      --subscription-arn "$sub_arn" \
      --attribute-name FilterPolicy \
      --attribute-value "file://$attrs" >/dev/null
    rm -f "$attrs"
    echo "  Subscribed: $queue  filter=$filter"
  else
    echo "  Subscribed: $queue  (no filter — receives all event types)"
  fi
}

# register_schema <registry> <schema-file> <schema-name>
# Registers a JSON Schema Draft-07 file as a new schema in the given registry.
# DataFormat=JSON + Compatibility=BACKWARD matches what schema-gov registers
# against real AWS Glue.
register_schema() {
  local registry="$1" file="$2" name="$3"
  aws glue create-schema \
    --registry-id "RegistryName=${registry}" \
    --schema-name "$name" \
    --data-format JSON \
    --compatibility BACKWARD \
    --schema-definition "file://${SCHEMAS_DIR}/${file}" >/dev/null
  echo "  Glue schema: $name"
}

# ── Glue Schema Registry ──────────────────────────────────────────────────────

echo "==> Glue registry: iam-delegation-events"
aws glue create-registry --registry-name iam-delegation-events >/dev/null

register_schema iam-delegation-events delegation_started.json              DelegationStarted
register_schema iam-delegation-events delegation_ended.json                DelegationEnded
register_schema iam-delegation-events delegation_review_requested.json     DelegationReviewRequested
register_schema iam-delegation-events delegation_escalation_requested.json DelegationEscalationRequested

# ── Outbound SNS topic ────────────────────────────────────────────────────────

echo ""
echo "==> Outbound SNS topic:"
TOPIC_ARN=$(aws sns create-topic --name iam-delegation-events --query TopicArn --output text)
echo "  $TOPIC_ARN"

# ── Inbound queue (this service consumes) ─────────────────────────────────────

echo ""
echo "==> Inbound (this service consumes):"
create_queue_with_dlq delegation-cascade-q

CASCADE_QUEUE_ARN=$(queue_arn "delegation-cascade-q")

# iam.user.events — owned by iam-user-profile; create the topic here so this
# service can be tested in isolation when user-profile's own init hasn't run.
IAM_USER_EVENTS_ARN=$(aws sns create-topic \
  --name iam-user-events \
  --query TopicArn --output text)
echo "  SNS topic (user events): $IAM_USER_EVENTS_ARN"

USER_SUB_ARN=$(aws sns subscribe \
  --topic-arn "$IAM_USER_EVENTS_ARN" \
  --protocol sqs \
  --notification-endpoint "$CASCADE_QUEUE_ARN" \
  --attributes RawMessageDelivery=true \
  --query SubscriptionArn --output text)

_USER_FILTER=$(mktemp)
printf '{"EventType":["UserUpdated"]}' > "$_USER_FILTER"
aws sns set-subscription-attributes \
  --subscription-arn "$USER_SUB_ARN" \
  --attribute-name FilterPolicy \
  --attribute-value "file://$_USER_FILTER" >/dev/null
rm -f "$_USER_FILTER"
echo "  Subscribed: delegation-cascade-q ← iam-user-events  filter=EventType=UserUpdated"

# ── Downstream fan-out subscribers ────────────────────────────────────────────

echo ""
echo "==> Downstream subscribers (Workflow, Notification, Audit, User Profile):"

create_queue_with_dlq delegation-workflow-q
subscribe_queue iam-delegation-events delegation-workflow-q \
  '{"EventType":["DelegationStarted","DelegationEnded","DelegationEscalationRequested"]}'

create_queue_with_dlq delegation-notification-q
subscribe_queue iam-delegation-events delegation-notification-q \
  '{"EventType":["DelegationStarted","DelegationEnded","DelegationReviewRequested","DelegationEscalationRequested"]}'

create_queue_with_dlq delegation-audit-q
subscribe_queue iam-delegation-events delegation-audit-q ""

# User Profile — DelegationEnded only (Gap 7 Option C: self-healing delegate
# pointer clear so a stale delegate_id in user_availability is fixed even when
# Delegation's own fire-and-forget ClearDelegatePointer HTTP call failed).
create_queue_with_dlq delegation-events-user-profile-q
subscribe_queue iam-delegation-events delegation-events-user-profile-q \
  '{"EventType":["DelegationEnded"]}'

# ── Summary ───────────────────────────────────────────────────────────────────

topic_count=$(aws sns list-topics --query 'length(Topics)' --output text)
queue_count=$(aws sqs list-queues  --query 'length(QueueUrls)' --output text)
sub_count=$(aws sns list-subscriptions --query 'length(Subscriptions)' --output text)
schema_count=$(aws glue list-schemas \
  --registry-id RegistryName=iam-delegation-events \
  --query 'length(Schemas)' --output text)

echo ""
echo "Floci init complete."
echo "  SNS topics:        ${topic_count} (expected 2)"
echo "  SQS queues+DLQs:   ${queue_count} (expected 10 = 5 pairs)"
echo "  SNS subscriptions: ${sub_count} (expected 5)"
echo "  Glue schemas:      ${schema_count} (expected 4 in iam-delegation-events)"
echo ""
echo "Resources:"
echo "  SNS topic  : $TOPIC_ARN"
echo "  Inbound  q : delegation-cascade-q  (MembershipRevoked / TenantMembershipsPurged — send direct)"
echo "  Subscribed : delegation-cascade-q ← iam-user-events (UserUpdated filter, DLG-D26)"
echo "  Subscriber : delegation-workflow-q              (DelegationStarted, DelegationEnded, DelegationEscalationRequested)"
echo "  Subscriber : delegation-notification-q          (DelegationStarted, DelegationEnded, DelegationReviewRequested, DelegationEscalationRequested)"
echo "  Subscriber : delegation-audit-q                 (all events — no filter)"
echo "  Subscriber : delegation-events-user-profile-q   (DelegationEnded — Gap 7 Option C self-heal)"
