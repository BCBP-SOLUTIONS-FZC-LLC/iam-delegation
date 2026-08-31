package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSwaggerInitializerHandler_Returns200WithJS(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/swagger-initializer.js", nil)

	SwaggerInitializerHandler(c)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/javascript") {
		t.Fatalf("Content-Type %q does not contain application/javascript", ct)
	}
	if w.Body.Len() == 0 {
		t.Fatal("body must not be empty")
	}
}

func TestSwaggerThemeHandler_Returns200WithCSS(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/swagger-theme.css", nil)

	SwaggerThemeHandler(c)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/css") {
		t.Fatalf("Content-Type %q does not contain text/css", ct)
	}
	if w.Body.Len() == 0 {
		t.Fatal("body must not be empty")
	}
}
