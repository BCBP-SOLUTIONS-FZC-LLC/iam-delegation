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
