package consumer

import (
	"context"
	"testing"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	awssqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// mockSQSClient satisfies events.SQSClientLike without real AWS credentials.
type mockSQSClient struct{}

func (m *mockSQSClient) ReceiveMessage(_ context.Context, _ *awssqs.ReceiveMessageInput, _ ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
	return &awssqs.ReceiveMessageOutput{Messages: []awssqstypes.Message{}}, nil
}
func (m *mockSQSClient) DeleteMessage(_ context.Context, _ *awssqs.DeleteMessageInput, _ ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error) {
	return &awssqs.DeleteMessageOutput{}, nil
}
func (m *mockSQSClient) ChangeMessageVisibility(_ context.Context, _ *awssqs.ChangeMessageVisibilityInput, _ ...func(*awssqs.Options)) (*awssqs.ChangeMessageVisibilityOutput, error) {
	return &awssqs.ChangeMessageVisibilityOutput{}, nil
}

// TestNewCascadeSQSConsumer_Constructs verifies the wiring function runs without
// panicking and that the resulting Consumer is non-nil when given a valid URL.
func TestNewCascadeSQSConsumer_Constructs(t *testing.T) {
	cascade := &fakeCascadeService{}
	idem := newFakeIdempotencyStore()
	var bindCalls []gucBindCall
	cc := NewCascadeConsumer(cascade, idem, fakeGUCBinder(&bindCalls), noopLogger{})

	consumer, err := NewCascadeSQSConsumer(
		&mockSQSClient{},
		"http://localhost:4566/000000000000/delegation-cascade-q",
		noopLogger{},
		cc,
	)
	if err != nil {
		t.Fatalf("NewCascadeSQSConsumer returned unexpected error: %v", err)
	}
	if consumer == nil {
		t.Fatal("NewCascadeSQSConsumer returned nil consumer")
	}
}
