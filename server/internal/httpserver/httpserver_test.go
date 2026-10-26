package httpserver

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"morde/server/internal/config"
	"morde/server/internal/domain"
)

func TestBaseRequestIDLogAndMetrics(t *testing.T) {
	engine, registry, logs := testEngine(t, "api")
	engine.GET("/hosts/:id", func(c *gin.Context) {
		c.Set(authKindKey, "api_key")
		c.String(http.StatusCreated, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/hosts/host-secret", nil)
	req.RemoteAddr = "192.0.2.7:8123"
	req.Header.Set(requestIDHeader, "request-123")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, req)

	if response.Code != http.StatusCreated || response.Body.String() != "ok" {
		t.Fatalf("response = %d %q, want 201 %q", response.Code, response.Body.String(), "ok")
	}
	if got := response.Header().Get(requestIDHeader); got != "request-123" {
		t.Fatalf("response request id = %q", got)
	}
	if got := logs.FilterMessage("http request").Len(); got != 1 {
		t.Fatalf("access log count = %d, want 1", got)
	}
	fields := logs.FilterMessage("http request").All()[0].ContextMap()
	for key, want := range map[string]any{
		"request_id": "request-123",
		"server":     "api",
		"route":      "/hosts/:id",
		"method":     http.MethodGet,
		"status":     int64(http.StatusCreated),
		"bytes_out":  int64(2),
		"client_ip":  "192.0.2.7",
		"auth_kind":  "api_key",
	} {
		if got := fields[key]; got != want {
			t.Errorf("access log %s = %#v, want %#v", key, got, want)
		}
	}
	if strings.Contains(fields["route"].(string), "host-secret") {
		t.Fatal("access log contains a raw host id")
	}

	requestMetric := metricFamily(t, registry, "cloudhost_http_requests_total")
	if len(requestMetric.Metric) != 1 {
		t.Fatalf("request metric series = %d, want 1", len(requestMetric.Metric))
	}
	metric := requestMetric.Metric[0]
	if metric.Counter.GetValue() != 1 {
		t.Fatalf("request counter = %v, want 1", metric.Counter.GetValue())
	}
	assertLabels(t, metric.Label, map[string]string{
		"server": "api", "route": "/hosts/:id", "method": http.MethodGet, "code": "201",
	})
	histogram := metricFamily(t, registry, "cloudhost_http_request_duration_seconds")
	if got := histogram.Metric[0].Histogram.GetSampleCount(); got != 1 {
		t.Fatalf("duration sample count = %d, want 1", got)
	}
}

func TestGatewayAccessLogIncludesProxyContext(t *testing.T) {
	engine, _, logs := testEngine(t, "gateway")
	engine.NoRoute(func(c *gin.Context) {
		c.Set("gateway.host_id", "h1234567")
		c.Set("gateway.preview_port", 5173)
		c.Set("gateway.upstream_prefix", "/v1")
		c.Set("gateway.upstream_ms", int64(12))
		c.Set("gateway.ws", true)
		SetRoute(c, "proxy")
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/shell/exec", nil)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, req)
	fields := logs.FilterMessage("http request").All()[0].ContextMap()
	for key, want := range map[string]any{
		"host_id": "h1234567", "preview_port": int64(5173), "upstream_prefix": "/v1",
		"upstream_ms": int64(12), "ws": true,
	} {
		if got := fields[key]; got != want {
			t.Errorf("access log %s = %#v, want %#v", key, got, want)
		}
	}
}

func TestRequestIDGeneratedWhenMissing(t *testing.T) {
	engine, _, _ := testEngine(t, "api")
	engine.GET("/id", func(c *gin.Context) { c.String(http.StatusOK, c.GetString(requestIDKey)) })
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/id", nil))

	id := response.Header().Get(requestIDHeader)
	if !validRequestID(id) || len(id) != 36 || id[14] != '4' {
		t.Fatalf("generated request id = %q, want a version 4 UUID", id)
	}
	if response.Body.String() != id {
		t.Fatalf("handler request id = %q, response header = %q", response.Body.String(), id)
	}
}

func TestBaseCanShareRegistry(t *testing.T) {
	registry := prometheus.NewRegistry()
	for _, server := range []string{"api", "gateway"} {
		if _, err := Base(Options{Server: server, Logger: zap.NewNop(), Metrics: registry}); err != nil {
			t.Fatalf("Base(%q) with shared registry: %v", server, err)
		}
	}
}

func TestMetricsBoundUnknownMethods(t *testing.T) {
	engine, registry, _ := testEngine(t, "api")
	engine.NoRoute(func(c *gin.Context) { c.Status(http.StatusNoContent) })
	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("X-HOST-7", "/custom", nil))

	metric := metricFamily(t, registry, "cloudhost_http_requests_total").Metric[0]
	if got := labelMap(metric.Label)["method"]; got != "OTHER" {
		t.Fatalf("unknown method label = %q, want OTHER", got)
	}
}

func TestBaseRejectsInvalidOptions(t *testing.T) {
	tests := []struct {
		name string
		opts Options
	}{
		{name: "server", opts: Options{Server: "other", Logger: zap.NewNop(), Metrics: prometheus.NewRegistry()}},
		{name: "logger", opts: Options{Server: "api", Metrics: prometheus.NewRegistry()}},
		{name: "metrics", opts: Options{Server: "api", Logger: zap.NewNop()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Base(tt.opts); err == nil {
				t.Fatal("Base returned no error")
			}
		})
	}
}

func TestWriteErrorMappings(t *testing.T) {
	tests := []struct {
		code   domain.ErrorCode
		status int
	}{
		{domain.CodeInvalidArgument, http.StatusBadRequest},
		{domain.CodeUnauthenticated, http.StatusUnauthorized},
		{domain.CodeForbidden, http.StatusForbidden},
		{domain.CodeAccountPending, http.StatusForbidden},
		{domain.CodeAccountRejected, http.StatusForbidden},
		{domain.CodeAccountDisabled, http.StatusForbidden},
		{domain.CodeNotFound, http.StatusNotFound},
		{domain.CodeQuotaExceeded, http.StatusConflict},
		{domain.CodeCapacityExceeded, http.StatusConflict},
		{domain.CodeWorkspaceBusy, http.StatusConflict},
		{domain.CodeWorkspaceConflict, http.StatusConflict},
		{domain.CodeTransferExpired, http.StatusConflict},
		{domain.CodeInvalidState, http.StatusConflict},
		{domain.CodeConflict, http.StatusConflict},
		{domain.CodeIdempotencyConflict, http.StatusConflict},
		{domain.CodeEventCursorExpired, http.StatusConflict},
		{domain.CodeUnsupported, http.StatusUnprocessableEntity},
		{domain.CodeRequestTooLarge, http.StatusRequestEntityTooLarge},
		{domain.CodeResultTooLarge, http.StatusRequestEntityTooLarge},
		{domain.CodeRateLimited, http.StatusTooManyRequests},
		{domain.CodeInternal, http.StatusInternalServerError},
		{domain.CodeUpstreamError, http.StatusBadGateway},
		{domain.CodeHostNotReady, http.StatusServiceUnavailable},
		{domain.CodeUpstreamTimeout, http.StatusGatewayTimeout},
		{domain.ErrorCode("unknown"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(string(tt.code), func(t *testing.T) {
			response, status := errorResponseFor(domain.NewError(tt.code, "public message"))
			if status != tt.status {
				t.Fatalf("status = %d, want %d", status, tt.status)
			}
			if response.Error.Code == "" || response.Error.Details == nil {
				t.Fatalf("response = %#v, want nonempty code and details", response)
			}
			if tt.code == domain.ErrorCode("unknown") && response.Error.Code != string(domain.CodeInternal) {
				t.Fatalf("unknown code = %q, want internal", response.Error.Code)
			}
		})
	}
}

func TestWriteErrorBodyRetryAfterAndLogOnce(t *testing.T) {
	engine, _, logs := testEngine(t, "api")
	engine.GET("/rate", func(c *gin.Context) {
		err := domain.NewError(domain.CodeRateLimited, "slow down")
		err.RetryAfter = 1200 * time.Millisecond
		WriteError(c, err)
	})
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/rate", nil))

	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "2" {
		t.Fatalf("response = %d retry-after=%q", response.Code, response.Header().Get("Retry-After"))
	}
	if got := response.Body.String(); !strings.Contains(got, "\"details\":{}") || !strings.Contains(got, "\"code\":\"rate_limited\"") {
		t.Fatalf("error body = %s", got)
	}
	if got := logs.FilterLevelExact(zap.ErrorLevel).Len(); got != 0 {
		t.Fatalf("4xx error log count = %d, want 0", got)
	}

	engine.GET("/internal", func(c *gin.Context) {
		WriteError(c, errors.New("secret internal detail"))
	})
	response = httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/internal", nil))
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "secret internal detail") {
		t.Fatalf("raw error response = %d %s", response.Code, response.Body.String())
	}
	if got := logs.FilterMessage("http request failed").Len(); got != 1 {
		t.Fatalf("5xx log count = %d, want 1", got)
	}

	engine.GET("/waking", func(c *gin.Context) {
		WriteError(c, domain.NewError(domain.CodeHostNotReady, "host is not ready"))
	})
	response = httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/waking", nil))
	if got := response.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("host waking Retry-After = %q, want 5", got)
	}
}

func TestWriteErrorStopsDownstreamHandlers(t *testing.T) {
	engine, _, _ := testEngine(t, "gateway")
	downstreamCalls := 0
	engine.GET("/stop-after-error", func(c *gin.Context) {
		WriteError(c, domain.NewError(domain.CodeNotFound, "not found"))
	}, func(c *gin.Context) {
		downstreamCalls++
		c.JSON(http.StatusOK, map[string]string{"unexpected": "downstream ran"})
	})

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/stop-after-error", nil))
	if response.Code != http.StatusNotFound || response.Body.String() != "{\"error\":{\"code\":\"not_found\",\"message\":\"not found\",\"details\":{}}}" {
		t.Fatalf("response = %d %q, want one not_found JSON body", response.Code, response.Body.String())
	}
	if downstreamCalls != 0 {
		t.Fatalf("downstream handler ran %d times after WriteError", downstreamCalls)
	}
}

func TestRecoveryHidesPanicAndFinalizesRequest(t *testing.T) {
	engine, registry, logs := testEngine(t, "gateway")
	engine.GET("/panic", func(*gin.Context) { panic("panic secret") })
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/panic", nil))

	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "panic secret") {
		t.Fatalf("panic response = %d %s", response.Code, response.Body.String())
	}
	if logs.FilterMessage("http panic").Len() != 1 || logs.FilterMessage("http request").Len() != 1 {
		t.Fatalf("panic/access logs = %d/%d, want 1/1", logs.FilterMessage("http panic").Len(), logs.FilterMessage("http request").Len())
	}
	if got := logs.FilterMessage("http panic").All()[0].ContextMap(); got["stack"] == nil || strings.Contains(got["stack"].(string), "panic secret") {
		t.Fatalf("panic log does not contain a safe stack: %#v", got)
	}
	if got := metricFamily(t, registry, "cloudhost_http_requests_total").Metric[0].Counter.GetValue(); got != 1 {
		t.Fatalf("panic request metric = %v, want 1", got)
	}
}

func TestAbortHandlerIsRepanickedWithoutErrorLog(t *testing.T) {
	engine, _, logs := testEngine(t, "gateway")
	engine.GET("/abort", func(*gin.Context) { panic(http.ErrAbortHandler) })
	func() {
		defer func() {
			value := recover()
			recovered, ok := value.(error)
			if !ok || !errors.Is(recovered, http.ErrAbortHandler) {
				t.Fatalf("recovered = %#v, want http.ErrAbortHandler", value)
			}
		}()
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/abort", nil))
	}()
	if logs.FilterMessage("http panic").Len() != 0 {
		t.Fatal("http.ErrAbortHandler was logged as a panic")
	}
}

func TestSetRouteUsesFixedLabelAndFullPath(t *testing.T) {
	engine, registry, _ := testEngine(t, "gateway")
	engine.NoRoute(func(c *gin.Context) {
		SetRoute(c, "preview")
		c.Status(http.StatusNoContent)
	})
	engine.GET("/proxy/*path", func(c *gin.Context) {
		SetRoute(c, "proxy")
		c.Status(http.StatusNoContent)
	})
	for _, path := range []string{"/missing/path-with-id", "/proxy/host-secret"} {
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	metrics := metricFamily(t, registry, "cloudhost_http_requests_total").Metric
	if len(metrics) != 2 {
		t.Fatalf("route series = %d, want 2", len(metrics))
	}
	seen := map[string]bool{}
	for _, metric := range metrics {
		labels := labelMap(metric.Label)
		seen[labels["route"]] = true
	}
	if !seen["preview"] || !seen["/proxy/*path"] {
		t.Fatalf("route labels = %#v", seen)
	}
}

func TestNewServerUsesHTTPTimeouts(t *testing.T) {
	cfg := config.HTTP{
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       3 * time.Second,
		WriteTimeout:      4 * time.Second,
		IdleTimeout:       5 * time.Second,
	}
	handler := &testHandler{}
	server := NewServer("127.0.0.1:8080", handler, cfg)
	if server.Addr != "127.0.0.1:8080" || server.Handler != handler {
		t.Fatal("server address or handler was not retained")
	}
	if server.ReadHeaderTimeout != cfg.ReadHeaderTimeout || server.ReadTimeout != cfg.ReadTimeout || server.WriteTimeout != cfg.WriteTimeout || server.IdleTimeout != cfg.IdleTimeout {
		t.Fatalf("server timeouts = %#v", server)
	}
}

func testEngine(t *testing.T, server string) (*gin.Engine, *prometheus.Registry, *observer.ObservedLogs) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)
	registry := prometheus.NewRegistry()
	middleware, err := Base(Options{Server: server, Logger: logger, Metrics: registry})
	if err != nil {
		t.Fatal(err)
	}
	engine := gin.New()
	engine.Use(middleware...)
	return engine, registry, logs
}

type testHandler struct{}

func (*testHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}

func metricFamily(t *testing.T, registry *prometheus.Registry, name string) *dto.MetricFamily {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == name {
			return family
		}
	}
	t.Fatalf("metric family %q not found", name)
	return nil
}

func assertLabels(t *testing.T, labels []*dto.LabelPair, expected map[string]string) {
	t.Helper()
	for name, want := range expected {
		if got := labelMap(labels)[name]; got != want {
			t.Errorf("metric label %s = %q, want %q", name, got, want)
		}
	}
}

func labelMap(labels []*dto.LabelPair) map[string]string {
	values := make(map[string]string, len(labels))
	for _, label := range labels {
		values[label.GetName()] = label.GetValue()
	}
	return values
}
