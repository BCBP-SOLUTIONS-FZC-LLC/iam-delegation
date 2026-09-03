package eventbus

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLogger records Warn calls without any external dependency.
// warnCalls is accessed via sync/atomic: StartRefresher's background
// goroutine writes it concurrently with a test's own polling read.
type fakeLogger struct {
	warnCalls atomic.Int64
}

func (f *fakeLogger) Debug(string, map[string]interface{}) {}
func (f *fakeLogger) Info(string, map[string]interface{})  {}
func (f *fakeLogger) Warn(string, map[string]interface{})  { f.warnCalls.Add(1) }
func (f *fakeLogger) Error(string, map[string]interface{}) {}

func TestGlueCodec_WithLogger_ReturnsSameInstance(t *testing.T) {
	g := &GlueCodec{}
	log := &fakeLogger{}
	got := g.WithLogger(log)
	assert.Same(t, g, got)
	assert.Same(t, log, g.log)
}

// TestGlueCodec_Encode_CacheHit_NeverTouchesClient confirms Encode's happy
// path — the schema version ID is already cached (the common case after
// NewGlueCodec's startup pre-fetch) — never calls the Glue client at all,
// so it's testable without any AWS mocking.
func TestGlueCodec_Encode_CacheHit_NeverTouchesClient(t *testing.T) {
	versionID := uuid.New().String()
	g := &GlueCodec{versionCache: map[string]string{"DelegationStarted": versionID}}

	payload := json.RawMessage(`{"delegation_id":"018f4c3e-a1b2-7000-9d3e-4f8c1a2b3d52"}`)
	encoded, gotVersionID, err := g.Encode(context.Background(), "DelegationStarted", payload)
	require.NoError(t, err)
	assert.Equal(t, versionID, gotVersionID)

	decoded, err := stripGlueHeader(encoded)
	require.NoError(t, err)
	assert.JSONEq(t, string(payload), string(decoded))
}

func TestGlueCodec_VersionID_CacheHit(t *testing.T) {
	versionID := uuid.New().String()
	g := &GlueCodec{versionCache: map[string]string{"DelegationEnded": versionID}}

	got, err := g.versionID(context.Background(), "DelegationEnded")
	require.NoError(t, err)
	assert.Equal(t, versionID, got)
}

// TestGlueCodec_StartRefresher_ExitsOnContextCancel confirms the background
// goroutine's ctx.Done() branch returns cleanly without ever ticking (the
// interval is deliberately much longer than the test), so this exercises
// the refresher's shutdown path without any Glue client interaction.
func TestGlueCodec_StartRefresher_ExitsOnContextCancel(t *testing.T) {
	g := &GlueCodec{versionCache: map[string]string{}}
	ctx, cancel := context.WithCancel(context.Background())

	g.StartRefresher(ctx, time.Hour)
	cancel()
	time.Sleep(20 * time.Millisecond) // let the goroutine observe ctx.Done() and return
}
