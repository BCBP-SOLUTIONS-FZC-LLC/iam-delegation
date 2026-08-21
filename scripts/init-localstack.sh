#!/usr/bin/env bash
# LocalStack ready-hook — provisions this service's outbound SNS topic and
# its one inbound SQS subscription (LLD §10):
#   - iam-delegation-events   — outbound: DelegationStarted/Ended/
#     ReviewRequested, published via the platform-events outbox (§10.4).
#     No local subscriber is created — nothing in this repo consumes its
#     own published events; downstream services (Workflow, Audit Log) would
#     each own their own queue/subscription in production the same way
#     iam-user-profile's fan-out queues do.
#   - delegation-cascade-q    — inbound: MembershipRevoked / TenantOffboarded
#     (§11.5/§11.6), DLQ delegation-cascade-q-dlq, maxReceiveCount=5. In
#     production this is populated by iam-org-membership's own event
#     emission; there is no local producer here, so send a message directly
#     to exercise the consumer, e.g.:
#       awslocal sqs send-message --queue-url <queue url> --message-body \
#         '{"id":"...","type":"MembershipRevoked","source":"iam-org-membership","specversion":"1","tenant_id":"...","time":"...","data":{"user_id":"..."}}'
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
# Schema definitions are read from /etc/localstack/init/schemas/ (mounted from
# internal/eventschema/ in docker-compose.pro.yml) so local dev stays in sync
# with the source-of-truth JSON files automatically.
# Registered under the PascalCase names codec.go/docs/runbook-schema-registry.md
# prescribe — matching what production Glue holds. The JSON filenames on disk
# stay snake_case; only the registered Glue schema name is PascalCase.
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

# ── Outbound SNS topic ──────────────────────────────────────────────────────
TOPIC_ARN=$(awslocal sns create-topic --name iam-delegation-events --query TopicArn --output text)
echo "SNS topic iam-delegation-events created: $TOPIC_ARN"

# ── Inbound SQS queue + DLQ ─────────────────────────────────────────────────
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

provision_queue "delegation-cascade-q"

echo "LocalStack init complete."
