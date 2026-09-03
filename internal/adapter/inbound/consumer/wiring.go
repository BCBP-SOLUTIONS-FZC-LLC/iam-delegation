package consumer

import (
	events "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// cascadeCascadeQMaxMessages/WaitSeconds mirror the sibling services'
// long-poll defaults (10 messages, 20s wait) — see iam-user-profile's
// user_event_consumer.go New().
const (
	cascadeQMaxMessages int32 = 10
	cascadeQWaitSeconds int32 = 20
)

// NewCascadeSQSConsumer builds the platform-events SQS consumer for
// delegation-cascade-q (LLD §10.1), wired to cascadeConsumer.Handle. client
// satisfies events.SQSClientLike (the real *sqs.Client from aws-sdk-go-v2,
// or a LocalStack/test double). queueURL is delegation-cascade-q's URL.
// opts is forwarded verbatim to events.NewSQSConsumerWithClient — the
// composition root (cmd/server/main.go) passes
// events.WithConsumerCodec(eventbus.GlueDecodeCodec{}) here so a
// Glue-encoded MembershipRevoked/TenantMembershipsPurged from Core, or a
// Glue-encoded UserUpdated from User Profile (Bug 2's second subscription
// onto this queue), decodes correctly (LLD §10.1, DLG-D21) — the decode
// codec is registry-agnostic (the 18-byte Glue wire header is
// self-describing), so it needs no per-producer configuration to handle a
// second event source; this package stays free of any outbound adapter
// import by accepting the option opaquely rather than importing eventbus
// itself (Clean Architecture — inbound adapters never depend on outbound
// adapters directly).
//
// Kept separate from NewCascadeConsumer (cascade_consumer.go) so
// CascadeConsumer itself has zero SQS/events-transport dependency and stays
// trivially unit-testable — only this thin wiring function touches
// events.NewSQSConsumerWithClient. logger is passed straight through to
// SQSConfig.Logger: this package's Logger interface has the same method
// set as platform-events' internal port.Logger, so no adapter is needed.
func NewCascadeSQSConsumer(client events.SQSClientLike, queueURL, region, endpointURL string, logger Logger, cascadeConsumer *CascadeConsumer, opts ...events.ConsumerOption) (events.Consumer, error) {
	return events.NewSQSConsumerWithClient(
		events.SQSConfig{
			QueueURL:    queueURL,
			Region:      region,
			EndpointURL: endpointURL,
			MaxMessages: cascadeQMaxMessages,
			WaitSeconds: cascadeQWaitSeconds,
			Logger:      logger,
		},
		client,
		cascadeConsumer.Handle,
		opts...,
	)
}
