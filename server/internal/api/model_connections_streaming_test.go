package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
)

func TestModelTextStreamingProbeAPIIsExplicitAndReportsProof(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &modelGatewayHTTPTest{checkCapabilities: app.ModelCapabilities{Text: true, TextStream: true}}
	deps := newServices().deps()
	deps.ModelGateway = service
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	response := requestWithCookieBodyAndHeader(router, http.MethodPost, "/api/model-connections/p1234567/check", `{"capability":"text_stream"}`,
		cookie, "https://portal.example.test", "Idempotency-Key", "stream-probe-api-001")
	if response.Code != http.StatusOK || service.checkCapability != "text_stream" || service.checkKey != "stream-probe-api-001" ||
		!strings.Contains(response.Body.String(), `"text_stream":true`) {
		t.Fatalf("stream probe response=%d capability=%q key=%q", response.Code, service.checkCapability, service.checkKey)
	}
}
