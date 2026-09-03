# AWS IAM policy for iam-delegation

This directory holds the reference IAM policy that must be attached to the
service's IRSA role. The application does not create the role itself — that is
managed by platform Terraform. Copy the JSON in `policy.json` into the
Terraform module or apply it directly via `aws iam create-policy` /
`aws iam put-role-policy`.

The role is assumed by the service's Kubernetes ServiceAccount via IRSA. Wire
the ARN into Helm via `serviceAccount.annotations`:

```yaml
serviceAccount:
  create: true
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::ACCOUNT_ID:role/iam-delegation
```

The SAME role is assumed by both the `cmd/server` Deployment (publishes to
the `iam-delegation-events` SNS topic; consumes `delegation-cascade-q`) and
all four `cmd/reconciler` CronJobs (LLD §16.1 — one ServiceAccount, shared
by every pod this chart creates).

## Grants breakdown

| Action | Purpose | LLD ref |
|---|---|---|
| `sns:Publish` | Outbox → SNS `iam-delegation-events` topic (DLG-D5) | §10.4 |
| `sqs:ReceiveMessage` / `DeleteMessage` / `GetQueueAttributes` / `ChangeMessageVisibility` | Cascade consumer on `delegation-cascade-q` (Core's `MembershipRevoked`/`TenantMembershipsPurged`) | §10.1/§11.5/§11.6 |
| `sqs:GetQueueAttributes` / `ReceiveMessage` on the DLQ | Ops visibility into `delegation-cascade-q-dlq` depth (`iam_delegation_cascade_dlq_total`) — not a consume-and-delete grant, read-only for triage | §10.1 |
| `glue:GetSchemaVersion` (+ read-only siblings) | `GlueCodec` pre-fetches schema version IDs at startup, refreshed every 5 min | §10.3.1 |
| `logs:CreateLogStream` / `PutLogEvents` | Container stdout to CloudWatch (if not using an OTel collector for logs) | — |

No S3/KMS grants — this service has no object-storage or GDPR-erasure
concern of its own (soft-delete → hard-purge is a plain SQL `DELETE`, LLD
§18.4, `delegation-cleanup` CronJob).

## Least-privilege scoping

The SNS topic policy and the two SQS queue policies should independently
restrict `Publish`/`ReceiveMessage` to this role and to Core's removal/
offboarding publishers respectively — that is managed by platform
infrastructure and is not represented here.
