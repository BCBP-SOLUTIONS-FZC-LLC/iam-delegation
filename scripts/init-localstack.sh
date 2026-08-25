#!/usr/bin/env bash
# LocalStack ready-hook — provisions this service's outbound SNS topic,
# its one inbound SQS subscription, and the three downstream fan-out queues
# owned by consuming services (Workflow, Notification, Audit) that subscribe
# to iam-delegation-events (LLD §10):
#
#   OUTBOUND (this service publishes):
#   - iam-delegation-events SNS topic — DelegationStarted, DelegationEnded,
#     DelegationReviewRequested via platform-events transactional outbox (§10.4)
#
#   DOWNSTREAM SUBSCRIBERS (owned by other services; created here for local dev):
#   - delegation-workflow-q / -dlq      — Workflow Service
#       filter: EventType IN [DelegationStarted, DelegationEnded]
#   - delegation-notification-q / -dlq  — Notification Service
#       filter: EventType IN [DelegationStarted, DelegationEnded, DelegationReviewRequested]
#   - delegation-audit-q / -dlq         — Audit Log Service
#       filter: none (receives all three types)
#
#   INBOUND (this service consumes):
#   - delegation-cascade-q / -dlq       — MembershipRevoked, TenantOffboarded
#       (published by iam-org-membership; send directly to exercise the consumer)
#
#   To send a test message to the inbound queue:
#     awslocal sqs send-message --queue-url <cascade-q-url> --message-body \
#       '{"id":"...","type":"MembershipRevoked","source":"iam-org-membership",
#         "specversion":"1","tenant_id":"...","time":"...","data":{"user_id":"..."}}'
#
# Matches api/asyncapi.yaml. Runs automatically on container start via the
# volume mount to /etc/localstack/init/ready.d/.

set -euo pipefail

AWS_ACCOUNT=000000000000
AWS_REGION=us-east-1
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
  register_schema "${SCHEMA_DIR}/delegation_started.json"          DelegationStarted
  register_schema "${SCHEMA_DIR}/delegation_ended.json"            DelegationEnded
  register_schema "${SCHEMA_DIR}/delegation_review_requested.json" DelegationReviewRequested
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
# Uses Python to set the filter policy to avoid shell JSON-quoting issues.
provision_subscriber() {
  local queue="$1"
  local filter_policy="$2"

  provision_queue "$queue"

  local queue_arn_val
  queue_arn_val=$(queue_arn "$queue")

  # Subscribe to SNS topic (no filter first, then set filter via Python)
  local sub_arn
  sub_arn=$(awslocal sns subscribe \
    --topic-arn "$TOPIC_ARN" \
    --protocol sqs \
    --notification-endpoint "$queue_arn_val" \
    --query SubscriptionArn --output text)

  if [ -n "$filter_policy" ]; then
    python3 -c "
import boto3, json, sys
sns = boto3.client('sns', endpoint_url='http://localhost:4566',
    region_name='us-east-1', aws_access_key_id='test', aws_secret_access_key='test')
sns.set_subscription_attributes(
    SubscriptionArn='${sub_arn}',
    AttributeName='FilterPolicy',
    AttributeValue='${filter_policy}')
print('  Subscribed: ${queue}  filter=${filter_policy}')
"
  else
    echo "  Subscribed: $queue  (no filter — receives all event types)"
  fi
}

# ── Inbound queue (this service consumes) ─────────────────────────────────────
echo ""
echo "==> Inbound (this service consumes):"
provision_queue "delegation-cascade-q"

# ── Downstream fan-out subscribers ────────────────────────────────────────────
# SNS filter policies use the EventType MessageAttribute (PascalCase) that the
# outbox stamped on each SNS publish call — matching api/asyncapi.yaml §bindings.

echo ""
echo "==> Downstream subscribers (Workflow, Notification, Audit):"

# Workflow Service — reroute on DelegationStarted, restore on DelegationEnded
provision_subscriber "delegation-workflow-q" \
  '{"EventType":["DelegationStarted","DelegationEnded"]}'

# Notification Service — all lifecycle events including review warnings
provision_subscriber "delegation-notification-q" \
  '{"EventType":["DelegationStarted","DelegationEnded","DelegationReviewRequested"]}'

# Audit Log — all three event types, no filter (immutable audit trail)
provision_subscriber "delegation-audit-q" ""

echo ""
echo "LocalStack init complete."
echo ""
echo "Resources:"
echo "  SNS topic  : $TOPIC_ARN"
echo "  Inbound  q : delegation-cascade-q  (MembershipRevoked / TenantOffboarded)"
echo "  Subscriber : delegation-workflow-q      (DelegationStarted, DelegationEnded)"
echo "  Subscriber : delegation-notification-q  (DelegationStarted, DelegationEnded, DelegationReviewRequested)"
echo "  Subscriber : delegation-audit-q         (all events — no filter)"
