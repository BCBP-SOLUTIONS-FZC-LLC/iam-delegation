package main

import (
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-delegation/internal/core/port"
	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// buildSNSPublisher constructs the SNS publisher from platform-events
// config.LoadSNS (SNS_TOPIC_ARN / AWS_REGION / AWS_ENDPOINT_URL), matching
// iam-user-profile. Region and EndpointURL are overlaid from loadConfig so
// this service's ap-south-1 default applies when AWS_REGION is unset
// (LoadSNS's own default is us-east-1).
func buildSNSPublisher(codec events.Codec, log port.Logger, region, endpointURL string) (events.Publisher, error) {
	snsEnv := eventcfg.LoadSNS()
	if region != "" {
		snsEnv.Region = region
	}
	if endpointURL != "" {
		snsEnv.EndpointURL = endpointURL
	}
	return events.NewSNSPublisher(eventcfg.SNSConfigFromEnv(snsEnv, log), events.WithCodec(codec))
}

// cascadeSQSEnv copies LoadSQS() values (region, endpoint, long-poll,
// visibility, max receive count) onto delegation-cascade-q, then overlays
// CASCADE_QUEUE_URL / CASCADE_SQS_CONCURRENCY. LoadSQS only reads
// SQS_QUEUE_URL / SQS_CONCURRENCY; this service cannot use those names
// as-is (matching iam-org-membership's sqsEnvForQueue).
func cascadeSQSEnv(base eventcfg.SQSConfigEnv, queueURL string, concurrency int, region, endpointURL string) eventcfg.SQSConfigEnv {
	env := base
	env.QueueURL = queueURL
	if concurrency > 0 {
		env.Concurrency = concurrency
	}
	if region != "" {
		env.Region = region
	}
	if endpointURL != "" {
		env.EndpointURL = endpointURL
	}
	return env
}
