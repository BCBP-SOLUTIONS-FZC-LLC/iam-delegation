package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsDevLikeEnvironment(t *testing.T) {
	for _, env := range []string{"development", "dev", "local", "test", ""} {
		require.True(t, isDevLikeEnvironment(env), "expected %q to be dev-like", env)
	}
	for _, env := range []string{"production", "staging", "uat", "prod"} {
		require.False(t, isDevLikeEnvironment(env), "expected %q to NOT be dev-like", env)
	}
}
