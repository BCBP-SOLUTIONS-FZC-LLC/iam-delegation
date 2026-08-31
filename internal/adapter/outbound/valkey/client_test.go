package valkey

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNewClient_PlainHostPort covers the ParseURL-fails branch: a plain
// host:port address is not a valid URL, so ParseURL returns an error and
// NewClient falls back to &redis.Options{Addr: addr}.
func TestNewClient_PlainHostPort(t *testing.T) {
	client := NewClient("localhost:6379")
	require.NotNil(t, client)
	require.Equal(t, "localhost:6379", client.Options().Addr)
}

// TestNewClient_FullURL covers the ParseURL-succeeds branch.
func TestNewClient_FullURL(t *testing.T) {
	client := NewClient("redis://localhost:6380")
	require.NotNil(t, client)
	// ParseURL sets Addr from the URL's host
	require.Equal(t, "localhost:6380", client.Options().Addr)
}
