package http

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestAsyncAPIHandler_ServesRenderedHTML(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/asyncapi", nil)
	c.Set(envLabelContextKey, "staging")

	AsyncAPIHandler(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
	assert.Contains(t, w.Body.String(), "STAGING")
}

func TestAsyncAPIYAMLHandler_ServesRawSpec(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/asyncapi.yaml", nil)

	AsyncAPIYAMLHandler(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/yaml")
	assert.Contains(t, w.Body.String(), "asyncapi")
}

func TestParseAsyncSpec_MalformedYAML_Errors(t *testing.T) {
	_, err := parseAsyncSpec([]byte("not: valid: yaml: at: all: :::"))
	require.Error(t, err)
}

func TestParseAsyncSpec_ValidYAML(t *testing.T) {
	s, err := parseAsyncSpec([]byte(`
info:
  title: Test Spec
  version: "1.0"
`))
	require.NoError(t, err)
	assert.Equal(t, "Test Spec", s.Info.Title)
}

func TestEnvLabelFromGin(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("value present", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Set(envLabelContextKey, "production")
		assert.Equal(t, "production", envLabelFromGin(c))
	})

	t.Run("value absent", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		assert.Equal(t, "", envLabelFromGin(c))
	})

	t.Run("wrong type stored", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Set(envLabelContextKey, 42)
		assert.Equal(t, "", envLabelFromGin(c))
	})
}

func TestPropType(t *testing.T) {
	tests := []struct {
		name string
		prop *asyncProp
		want string
	}{
		{"ref", &asyncProp{Ref: "#/components/schemas/DelegationEnded"}, "DelegationEnded"},
		{"plain type", &asyncProp{Type: "string"}, "string"},
		{"type with format", &asyncProp{Type: "string", Format: "uuid"}, "string(uuid)"},
		{"array of primitive", &asyncProp{Type: "array", Items: &asyncProp{Type: "string"}}, "array<string>"},
		{"array of ref", &asyncProp{Type: "array", Items: &asyncProp{Ref: "#/components/schemas/Foo"}}, "array<Foo>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, propType(tc.prop))
		})
	}
}

func TestTypeHTML(t *testing.T) {
	tests := []struct {
		name   string
		prop   *asyncProp
		wantIn string
	}{
		{"ref", &asyncProp{Ref: "#/components/schemas/DelegationEnded"}, `href="#schema-DelegationEnded"`},
		{"array of ref", &asyncProp{Type: "array", Items: &asyncProp{Ref: "#/components/schemas/Foo"}}, `href="#schema-Foo"`},
		{"plain type", &asyncProp{Type: "string"}, `<span class="prop-type">string</span>`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Contains(t, typeHTML(tc.prop), tc.wantIn)
		})
	}
}

func TestWalkYAML(t *testing.T) {
	var node yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(`
sns:
  messageAttributes:
    EventType:
      value: DelegationEnded
`), &node))

	assert.Equal(t, "DelegationEnded", walkYAML(&node, "sns", "messageAttributes", "EventType", "value"))
	assert.Equal(t, "", walkYAML(&node, "sns", "messageAttributes", "missing", "value"))
	assert.Equal(t, "", walkYAML(nil, "sns"))
	assert.Equal(t, "", walkYAML(&node))
}

func TestSNSEventType(t *testing.T) {
	t.Run("nil node", func(t *testing.T) {
		assert.Equal(t, "", snsEventType(nil))
	})

	t.Run("EventType present", func(t *testing.T) {
		var node yaml.Node
		require.NoError(t, yaml.Unmarshal([]byte(`
sns:
  messageAttributes:
    EventType:
      value: DelegationStarted
`), &node))
		assert.Equal(t, "DelegationStarted", snsEventType(&node))
	})

	t.Run("snake_case fallback", func(t *testing.T) {
		var node yaml.Node
		require.NoError(t, yaml.Unmarshal([]byte(`
sns:
  messageAttributes:
    event_type:
      value: DelegationReviewRequested
`), &node))
		assert.Equal(t, "DelegationReviewRequested", snsEventType(&node))
	})
}

func TestRenderPropsTable(t *testing.T) {
	t.Run("nil schema", func(t *testing.T) {
		var buf bytes.Buffer
		renderPropsTable(&buf, nil, "")
		assert.Contains(t, buf.String(), "No properties")
	})

	t.Run("empty properties", func(t *testing.T) {
		var buf bytes.Buffer
		renderPropsTable(&buf, &asyncSchema{}, "")
		assert.Contains(t, buf.String(), "No properties")
	})

	t.Run("required, enum, item-enum, long description, example, unordered extra key", func(t *testing.T) {
		longDesc := strings.Repeat("a", 250)
		sc := &asyncSchema{
			Required:      []string{"status"},
			PropertyOrder: []string{"status"},
			Properties: map[string]asyncProp{
				"status": {
					Type: "string", Desc: longDesc, Enum: []string{"active", "ended"},
					Example: "active",
				},
				"tags": {
					Type: "array", Items: &asyncProp{Type: "string", Enum: []string{"a", "b"}},
				},
			},
		}
		var buf bytes.Buffer
		renderPropsTable(&buf, sc, "DelegationEnded")
		out := buf.String()
		assert.Contains(t, out, "required")
		assert.Contains(t, out, "enum-val")
		assert.Contains(t, out, "…", "description over 200 chars must be truncated with an ellipsis")
		assert.Contains(t, out, "status")
		assert.Contains(t, out, "tags", "a property not in PropertyOrder must still be rendered")
		assert.Contains(t, out, "values:", "item-level enum must render its own label")
	})
}
