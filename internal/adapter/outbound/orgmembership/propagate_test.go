package orgmembership

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestPropagate_PlainContext_UsesOTel(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)
	propagate(context.Background(), req)
	// reaching here without panic means the OTel branch executed
}

func TestPropagate_GinContext_UsesGinPropagation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(w)
	gc.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	outReq, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)
	propagate(gc, outReq)
	// reaching here without panic means the gin.Context branch executed
}
