package consumer

import (
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/config"
	events "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// NewCascadeSQSConsumer builds the platform-events SQS consumer for
// delegation-cascade-q (LLD §10.1), wired to cascadeConsumer.Handle. client
// satisfies events.SQSClientLike (the real *sqs.Client from aws-sdk-go-v2,
// or a LocalStack/test double). sqsEnv comes from config.LoadSQS after
// cascadeSQSEnv overlays CASCADE_QUEUE_URL / CASCADE_SQS_CONCURRENCY
// (matching iam-user-profile's consumer.New and iam-org-membership's
// buildSQSConsumer). opts is forwarded after SQSConsumerOptions so the
// composition root can pass events.WithConsumerCodec(eventbus.GlueDecodeCodec{})
// — Core Glue-encodes MembershipRevoked/TenantMembershipsPurged independently
// of this service's outbound GLUE_REGISTRY_NAME, and User Profile Glue-encodes
// UserUpdated (Bug 2's second subscription). GlueDecodeCodec is
// registry-agnostic (the 18-byte Glue wire header is self-describing).
//
// Kept separate from NewCascadeConsumer (cascade_consumer.go) so
// CascadeConsumer itself has zero SQS/events-transport dependency and stays
// trivially unit-testable — only this thin wiring function touches
// events.NewSQSConsumerWithClient. logger is passed straight through to
// SQSConfig.Logger: port.Logger matches platform-events' internal
// port.Logger method set, so no adapter is needed.
func NewCascadeSQSConsumer(client events.SQSClientLike, sqsEnv eventcfg.SQSConfigEnv, logger port.Logger, cascadeConsumer *CascadeConsumer, opts ...events.ConsumerOption) (events.Consumer, error) {
	opts = append(eventcfg.SQSConsumerOptions(sqsEnv), opts...)
	return events.NewSQSConsumerWithClient(
		eventcfg.SQSConfigFromEnv(sqsEnv, logger),
		client,
		cascadeConsumer.Handle,
		opts...,
	)
}
