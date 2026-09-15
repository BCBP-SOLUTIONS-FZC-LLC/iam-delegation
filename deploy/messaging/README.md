# Messaging Infrastructure — `iam-delegation`

This directory contains the AWS messaging infrastructure definitions
(SNS topics, SQS queues, SNS subscriptions) required by `iam-delegation`.

The application does **not** create these resources itself — they are managed
by platform Terraform. Apply `sns_subscriptions.tf.example` in the platform
Terraform module after adapting the ARN references.

---

## Queue: `delegation-cascade-q`

The cascade queue receives from **two** upstream SNS topics:

| Source Topic | Event Type | Status |
|---|---|---|
| `iam.membership.events` | `MembershipRevoked` | ✅ Already provisioned |
| `iam.membership.events` | `TenantMembershipsPurged` | ✅ Already provisioned |
| `iam.user.events` | `UserUpdated` | ❌ **Gap 6 — MISSING, must be provisioned** |

The `iam.user.events → UserUpdated` subscription is the one described in
`api/asyncapi.yaml §sqsDelegationCascade` (Bug 2 / DLG-D26). Until it is
provisioned, the delegate-disabled cascade (`CascadeService.EndForDisabledDelegate`)
will never fire — a disabled delegate's active delegations will remain open
indefinitely.

See `sns_subscriptions.tf.example` for the exact Terraform resources needed.
