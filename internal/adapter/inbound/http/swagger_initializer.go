package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// SwaggerInitializerHandler serves swagger-initializer.js with a plugin that keeps
// model collapse toggles visible after expand (Swagger UI hides them via hideSelfOnExpand).
func SwaggerInitializerHandler(c *gin.Context) {
	c.Header("Content-Type", "application/javascript; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.String(http.StatusOK, swaggerInitializerJS)
}

// Mirrors gin-swagger defaults plus KeepModelTogglePlugin.
const swaggerInitializerJS = `
window.onload = function() {
  const KeepModelTogglePlugin = function() {
    return {
      wrapComponents: {
        ModelCollapse: function(Original, system) {
          return function(props) {
            var next = Object.assign({}, props);
            // Nested property rows use "[...]" — keep the collapse toggle after expand.
            // Top-level schemas use "{...}" — preserve hideSelfOnExpand to avoid duplicate titles.
            if (props.collapsedContent === '[...]') {
              next.hideSelfOnExpand = false;
            }
            return system.React.createElement(Original, next);
          };
        }
      }
    };
  };

  const TAG_ORDER = ['internal', 'delegations', 'infra'];

  const ui = SwaggerUIBundle({
    url: "doc.json",
    dom_id: '#swagger-ui',
    validatorUrl: null,
    oauth2RedirectUrl: ` + "`${window.location.protocol}//${window.location.host}${window.location.pathname.split('/').slice(0, window.location.pathname.split('/').length - 1).join('/')}/oauth2-redirect.html`" + `,
    persistAuthorization: false,
    presets: [
      SwaggerUIBundle.presets.apis,
      SwaggerUIStandalonePreset
    ],
    plugins: [
      SwaggerUIBundle.plugins.DownloadUrl,
      KeepModelTogglePlugin
    ],
    layout: "StandaloneLayout",
    docExpansion: "list",
    deepLinking: true,
    defaultModelsExpandDepth: 1,
    tagsSorter: function(a, b) {
      var ia = TAG_ORDER.indexOf(a);
      var ib = TAG_ORDER.indexOf(b);
      if (ia === -1) ia = TAG_ORDER.length;
      if (ib === -1) ib = TAG_ORDER.length;
      return ia - ib;
    }
  });

  window.ui = ui;
};
`
