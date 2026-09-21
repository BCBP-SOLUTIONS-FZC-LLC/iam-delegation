package consumer

import (
	"context"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/config"
)

// stubSQSClient satisfies events.SQSClientLike without touching real AWS —
// NewCascadeSQSConsumer only needs a value of this shape to construct the
// consumer; none of these methods are invoked until Start() polls, which
// this test does not call.
type stubSQSClient struct{}

func (stubSQSClient) ReceiveMessage(context.Context, *awssqs.ReceiveMessageInput, ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
	return &awssqs.ReceiveMessageOutput{}, nil
}

func (stubSQSClient) DeleteMessage(context.Context, *awssqs.DeleteMessageInput, ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error) {
	return &awssqs.DeleteMessageOutput{}, nil
}

func (stubSQSClient) ChangeMessageVisibility(context.Context, *awssqs.ChangeMessageVisibilityInput, ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
	return &awssqs.ChangeMessageVisibilityOutput{}, nil
}

func TestNewCascadeSQSConsumer_BuildsConsumer(t *testing.T) {
	cascadeConsumer := newTestConsumer(&fakeCascadeService{}, newFakeIdempotencyStore(), func(ctx context.Context, tenantID uuid.UUID, userID string) context.Context { return ctx })

	sqsEnv := eventcfg.SQSConfigEnv{
		QueueURL:          "https://sqs.example.com/queue/delegation-cascade-q",
		Region:            "us-east-1",
		MaxMessages:       10,
		WaitSeconds:       20,
		Concurrency:       4,
		VisibilityTimeout: 30 * time.Second,
	}
	c, err := NewCascadeSQSConsumer(stubSQSClient{}, sqsEnv, noopLogger{}, cascadeConsumer)
	require.NoError(t, err)
	assert.NotNil(t, c)
}
