package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

func TestCascadeSQSEnv_OverlaysQueueAndConcurrency(t *testing.T) {
	base := eventcfg.SQSConfigEnv{
		QueueURL:    "https://sqs.example.com/from-load-sqs",
		Region:      "us-east-1",
		EndpointURL: "http://stale:4566",
		Concurrency: 1,
	}
	got := cascadeSQSEnv(base, "https://sqs.example.com/cascade", 4, "ap-south-1", "http://localhost:4570")
	assert.Equal(t, "https://sqs.example.com/cascade", got.QueueURL)
	assert.Equal(t, 4, got.Concurrency)
	assert.Equal(t, "ap-south-1", got.Region)
	assert.Equal(t, "http://localhost:4570", got.EndpointURL)
}

func TestCascadeSQSEnv_ZeroConcurrencyKeepsBase(t *testing.T) {
	base := eventcfg.SQSConfigEnv{Concurrency: 1}
	got := cascadeSQSEnv(base, "https://sqs.example.com/cascade", 0, "", "")
	assert.Equal(t, 1, got.Concurrency)
}

func TestBuildSNSPublisher_RequiresTopicARN(t *testing.T) {
	t.Setenv("SNS_TOPIC_ARN", "")
	t.Setenv("AWS_REGION", "ap-south-1")
	_, err := buildSNSPublisher(events.NoopCodec{}, nil, "ap-south-1", "")
	require.Error(t, err)
}
