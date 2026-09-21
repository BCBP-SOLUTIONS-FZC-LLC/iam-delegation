package tender

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestPropagate_GinContext_UsesPropagateHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(w)
	gc.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	req := httptest.NewRequest(http.MethodPost, "http://example.com/exists", nil)
	assert.NotPanics(t, func() { propagate(gc, req) })
}

func TestPropagate_PlainContext_FallsBackToOTelInject(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://example.com/exists", nil)
	assert.NotPanics(t, func() { propagate(t.Context(), req) })
}
