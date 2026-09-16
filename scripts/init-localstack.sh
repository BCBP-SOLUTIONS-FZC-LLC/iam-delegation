#!/usr/bin/env bash
# LocalStack ready-hook — provisions this service's outbound SNS topic,
# its one inbound SQS subscription, and the three downstream fan-out queues
# owned by consuming services (Workflow, Notification, Audit) that subscribe
# to iam-delegation-events (LLD §10):
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
#       (published by iam-org-membership on iam.membership.events) and
#       UserUpdated (published by iam-user-profile on iam.user.events,
#       Bug 2/DLG-D26). Two SNS subscriptions feed this one queue.
#       This script provisions the iam.user.events topic (creating it if
#       absent — it is normally owned by iam-user-profile's own init
#       script) and subscribes delegation-cascade-q to it. Core's
#       iam.membership.events subscription is exercised by sending directly.
#
#   To send a test MembershipRevoked directly (bypassing SNS):
#     awslocal sqs send-message --queue-url <cascade-q-url> --message-body \
#       '{"id":"...","type":"MembershipRevoked","source":"iam-org-membership",
#         "specversion":"1","tenant_id":"...","time":"...","data":{"user_id":"..."}}'
#
# Matches api/asyncapi.yaml. Runs automatically on container start via the
# volume mount to /etc/localstack/init/ready.d/.

set -euo pipefail

AWS_ACCOUNT=000000000000
AWS_REGION=ap-south-1
MAX_RECEIVES=5

queue_arn() { printf 'arn:aws:sqs:%s:%s:%s' "$AWS_REGION" "$AWS_ACCOUNT" "$1"; }

# ── Glue Schema Registry ──────────────────────────────────────────────────────
# Best-effort: Glue requires LocalStack Pro (docker-compose.pro.yml). If
# unavailable the script continues so SNS/SQS are always created.
SCHEMA_DIR="/etc/localstack/init/schemas"
if awslocal glue create-registry --registry-name iam-delegation-events 2>/dev/null; then
  echo "Glue registry iam-delegation-events created"
  register_schema() {
    local FILE="$1" NAME="$2"
    awslocal glue create-schema \
      --registry-id "RegistryName=iam-delegation-events" \
      --schema-name "$NAME" \
      --data-format JSON \
      --compatibility BACKWARD \
      --schema-definition "$(cat "$FILE")"
    echo "Glue schema $NAME created (from $(basename "$FILE"))"
  }
  register_schema "${SCHEMA_DIR}/delegation_started.json"              DelegationStarted
  register_schema "${SCHEMA_DIR}/delegation_ended.json"                DelegationEnded
  register_schema "${SCHEMA_DIR}/delegation_review_requested.json"     DelegationReviewRequested
  register_schema "${SCHEMA_DIR}/delegation_escalation_requested.json" DelegationEscalationRequested
else
  echo "Glue not available — skipping registry setup (GLUE_REGISTRY_NAME must remain empty)"
fi

# ── Outbound SNS topic ────────────────────────────────────────────────────────
TOPIC_ARN=$(awslocal sns create-topic --name iam-delegation-events --query TopicArn --output text)
echo "SNS topic: $TOPIC_ARN"

# ── Helper: provision queue + DLQ (no SNS subscription) ──────────────────────
provision_queue() {
  local queue="$1"
  local dlq="${queue}-dlq"

  awslocal sqs create-queue --queue-name "$dlq" >/dev/null

  local attrs
  attrs=$(mktemp)
  cat >"$attrs" <<EOF
{
  "RedrivePolicy": "{\"deadLetterTargetArn\":\"$(queue_arn "$dlq")\",\"maxReceiveCount\":\"${MAX_RECEIVES}\"}"
}
EOF
  awslocal sqs create-queue --queue-name "$queue" --attributes "file://$attrs" >/dev/null
  rm -f "$attrs"

  echo "  Queue: $(awslocal sqs get-queue-url --queue-name "$queue" --output text)"
  echo "  DLQ:   $(awslocal sqs get-queue-url --queue-name "$dlq" --output text)"
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
  sub_arn=$(awslocal sns subscribe \
    --topic-arn "$TOPIC_ARN" \
    --protocol sqs \
    --notification-endpoint "$queue_arn_val" \
    --attributes RawMessageDelivery=true \
    --query SubscriptionArn --output text)

  if [ -n "$filter_policy" ]; then
    local attrs
    attrs=$(mktemp)
    printf '%s' "$filter_policy" > "$attrs"
    awslocal sns set-subscription-attributes \
      --subscription-arn "$sub_arn" \
      --attribute-name FilterPolicy \
      --attribute-value "file://$attrs" >/dev/null
    rm -f "$attrs"
    echo "  Subscribed: $queue  filter=$filter_policy"
  else
    echo "  Subscribed: $queue  (no filter — receives all event types)"
  fi
}

# ── Inbound queue (this service consumes) ─────────────────────────────────────
# Two upstream SNS topics feed delegation-cascade-q (LLD §10.1, Bug 2/DLG-D26):
#   1. iam.membership.events (Core) — MembershipRevoked, TenantMembershipsPurged
#      The SNS→SQS subscription is provisioned in org-membership's init-floci.sh
#      (Gap OM-2 fix, Option A — the topic owner creates all downstream
#      subscriptions). For isolated local dev without org-membership running,
#      send directly to the SQS queue:
#        awslocal sqs send-message --queue-url <cascade-q-url> --message-body '...'
#   2. iam.user.events (User Profile) — UserUpdated{status:disabled}
#      This script creates the topic if absent and subscribes this queue to it
#      with an EventType=UserUpdated filter policy (Gap 1 fix / DLG-D26).
echo ""
echo "==> Inbound (this service consumes):"
provision_queue "delegation-cascade-q"

CASCADE_QUEUE_ARN=$(queue_arn "delegation-cascade-q")

# iam.user.events — owned by iam-user-profile; create here so this service
# can be tested in isolation when user-profile's own init hasn't run.
IAM_USER_EVENTS_ARN=$(awslocal sns create-topic \
  --name iam-user-events \
  --query TopicArn --output text)
echo "  SNS topic (user events): $IAM_USER_EVENTS_ARN"

USER_SUB_ARN=$(awslocal sns subscribe \
  --topic-arn "$IAM_USER_EVENTS_ARN" \
  --protocol sqs \
  --notification-endpoint "$CASCADE_QUEUE_ARN" \
  --attributes RawMessageDelivery=true \
  --query SubscriptionArn --output text)

# Filter: only UserUpdated events — the cascade consumer acks other
# EventType values as no-ops, but filtering here reduces unnecessary traffic.
_USER_FILTER_FILE=$(mktemp)
printf '{"EventType":["UserUpdated"]}' > "$_USER_FILTER_FILE"
awslocal sns set-subscription-attributes \
  --subscription-arn "$USER_SUB_ARN" \
  --attribute-name FilterPolicy \
  --attribute-value "file://$_USER_FILTER_FILE" >/dev/null
rm -f "$_USER_FILTER_FILE"
echo "  Subscribed: delegation-cascade-q ← iam.user.events  filter=EventType=UserUpdated"

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

# User Profile — DelegationEnded only (Gap 7 Option C: self-healing delegate
# pointer clear so stale delegate_id in user_availability is fixed even when
# Delegation's own fire-and-forget ClearDelegatePointer HTTP call failed).
provision_subscriber "delegation-events-user-profile-q" \
  '{"EventType":["DelegationEnded"]}'

echo ""
echo "LocalStack init complete."
echo ""
echo "Resources:"
echo "  SNS topic  : $TOPIC_ARN"
echo "  Inbound  q : delegation-cascade-q  (MembershipRevoked / TenantMembershipsPurged — send direct)"
echo "  Subscribed : delegation-cascade-q ← iam.user.events (UserUpdated filter, Gap 1 / DLG-D26)"
echo "  Subscriber : delegation-workflow-q              (DelegationStarted, DelegationEnded, DelegationEscalationRequested)"
echo "  Subscriber : delegation-notification-q          (DelegationStarted, DelegationEnded, DelegationReviewRequested, DelegationEscalationRequested)"
echo "  Subscriber : delegation-audit-q                 (all events — no filter)"
echo "  Subscriber : delegation-events-user-profile-q   (DelegationEnded — Gap 7 Option C self-heal)"
