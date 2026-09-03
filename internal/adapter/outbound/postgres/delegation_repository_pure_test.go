package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLimitOrDefault(t *testing.T) {
	assert.Equal(t, defaultFindLimit, limitOrDefault(0))
	assert.Equal(t, defaultFindLimit, limitOrDefault(-5))
	assert.Equal(t, 25, limitOrDefault(25))
}
