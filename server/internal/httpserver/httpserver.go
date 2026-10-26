package httpserver

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/config"
	"morde/server/internal/domain"
)

const (
	requestIDHeader = "X-Request-Id"
	requestIDKey    = "request_id"
	authKindKey     = "auth_kind"
	routeKey        = "httpserver.route"
	loggerKey       = "httpserver.logger"
	serverKey       = "httpserver.server"
	stateKey        = "httpserver.state"
)

type Options struct {
	Server  string
	Logger  *zap.Logger
	Metrics prometheus.Registerer
}

type requestState struct {
	finalizers []func(*gin.Context)
	finalized  bool
}

func Base(o Options) ([]gin.HandlerFunc, error) {
	if o.Server != "api" && o.Server != "gateway" {
		return nil, fmt.Errorf("httpserver server must be api or gateway")
	}
	if o.Logger == nil {
		return nil, fmt.Errorf("httpserver logger is required")
	}
	if o.Metrics == nil {
		return nil, fmt.Errorf("httpserver metrics registerer is required")
	}
	counter, histogram, err := registerHTTPMetrics(o.Metrics)
	if err != nil {
		return nil, err
	}
	return []gin.HandlerFunc{
		recovery(o.Server, o.Logger),
		requestID(o.Logger),
		accessLog(o.Server, o.Logger),
		metrics(o.Server, counter, histogram),
	}, nil
}

func WriteError(c *gin.Context, err error) {
	if err == nil {
		return
	}

	response, status := errorResponseFor(err)
	if status >= http.StatusInternalServerError {
		if logger, ok := c.Get(loggerKey); ok {
			if l, ok := logger.(*zap.Logger); ok && l != nil {
				l.Error("http request failed",
					zap.String("request_id", c.GetString(requestIDKey)),
					zap.String("server", c.GetString(serverKey)),
					zap.String("code", response.Error.Code),
				)
			}
		}
	}
	writeErrorResponse(c, response, status)
	c.Abort()
}

func SetRoute(c *gin.Context, label string) {
	if c.FullPath() != "" {
		return
	}
	if !validLabel(label) {
		label = "other"
	}
	c.Set(routeKey, label)
}

func NewServer(addr string, h http.Handler, cfg config.HTTP) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}
}

func recovery(server string, logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		state := &requestState{}
		c.Set(stateKey, state)
		c.Set(loggerKey, logger)
		c.Set(serverKey, server)
		defer func() {
			if recovered := recover(); recovered != nil {
				if isAbortHandler(recovered) {
					finalize(c, state)
					panic(recovered)
				}
				logger.Error("http panic",
					zap.String("request_id", c.GetString(requestIDKey)),
					zap.String("server", c.GetString(serverKey)),
					zap.String("stack", string(debug.Stack())),
				)
				c.Abort()
				if !c.Writer.Written() {
					writeErrorResponse(c, errorResponse{
						Error: errorBody{Code: string(domain.CodeInternal), Message: "internal error", Details: map[string]any{}},
					}, http.StatusInternalServerError)
				}
			}
			finalize(c, state)
		}()
		c.Next()
	}
}

func requestID(logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Request.Header.Get(requestIDHeader)
		if !validRequestID(id) {
			var err error
			id, err = newRequestID()
			if err != nil {
				logger.Error("generate request id failed", zap.Error(err))
				id = "unavailable"
			}
		}
		c.Set(requestIDKey, id)
		c.Writer.Header().Set(requestIDHeader, id)
		c.Next()
	}
}

func accessLog(server string, logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		state := getState(c)
		if state != nil {
			state.finalizers = append(state.finalizers, func(c *gin.Context) {
				status := c.Writer.Status()
				bytesOut := c.Writer.Size()
				if bytesOut < 0 {
					bytesOut = 0
				}
				fields := []zap.Field{
					zap.String("request_id", c.GetString(requestIDKey)),
					zap.String("server", server),
					zap.String("route", route(c)),
					zap.String("method", c.Request.Method),
					zap.Int("status", status),
					zap.Int("bytes_out", bytesOut),
					zap.Int64("duration_ms", time.Since(started).Milliseconds()),
					zap.String("client_ip", clientIP(c.Request.RemoteAddr)),
					zap.String("auth_kind", authKind(c)),
				}
				if hostID := c.GetString("gateway.host_id"); hostID != "" {
					fields = append(fields, zap.String("host_id", hostID))
				}
				if port, ok := c.Get("gateway.preview_port"); ok {
					if value, valid := port.(int); valid {
						fields = append(fields, zap.Int("preview_port", value))
					}
				}
				if prefix := c.GetString("gateway.upstream_prefix"); prefix != "" {
					fields = append(fields, zap.String("upstream_prefix", prefix))
				}
				if millis, ok := c.Get("gateway.upstream_ms"); ok {
					if value, valid := millis.(int64); valid {
						fields = append(fields, zap.Int64("upstream_ms", value))
					}
				}
				if websocket, ok := c.Get("gateway.ws"); ok {
					if value, valid := websocket.(bool); valid {
						fields = append(fields, zap.Bool("ws", value))
					}
				}
				logger.Info("http request",
					fields...,
				)
			})
		}
		c.Next()
	}
}

func metrics(server string, counter *prometheus.CounterVec, histogram *prometheus.HistogramVec) gin.HandlerFunc {
	return func(c *gin.Context) {
		if state := getState(c); state != nil {
			started := time.Now()
			state.finalizers = append(state.finalizers, func(c *gin.Context) {
				status := c.Writer.Status()
				method := metricMethod(c.Request.Method)
				counter.WithLabelValues(server, route(c), method, strconv.Itoa(status)).Inc()
				histogram.WithLabelValues(server, route(c), method).Observe(time.Since(started).Seconds())
			})
		}
		c.Next()
	}
}

func metricMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return method
	default:
		return "OTHER"
	}
}

func route(c *gin.Context) string {
	if path := c.FullPath(); path != "" {
		return path
	}
	if label := c.GetString(routeKey); validLabel(label) {
		return label
	}
	return "unmatched"
}

func authKind(c *gin.Context) string {
	if kind := c.GetString(authKindKey); validLabel(kind) {
		return kind
	}
	return "none"
}

func clientIP(remoteAddr string) string {
	// No trusted-proxy list exists in config, so forwarded headers would be spoofable.
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return remoteAddr
}

func validLabel(label string) bool {
	if len(label) == 0 || len(label) > 64 || label[0] < 'a' || label[0] > 'z' {
		return false
	}
	for i := 1; i < len(label); i++ {
		ch := label[i]
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '_' && ch != '-' {
			return false
		}
	}
	return true
}

func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

func newRequestID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("read request id randomness: %w", err)
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], id[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], id[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], id[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], id[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], id[10:16])
	return string(encoded), nil
}

func getState(c *gin.Context) *requestState {
	value, ok := c.Get(stateKey)
	if !ok {
		return nil
	}
	state, _ := value.(*requestState)
	return state
}

func finalize(c *gin.Context, state *requestState) {
	if state.finalized {
		return
	}
	state.finalized = true
	for _, fn := range state.finalizers {
		fn(c)
	}
}

func registerHTTPMetrics(reg prometheus.Registerer) (*prometheus.CounterVec, *prometheus.HistogramVec, error) {
	counter, err := registerCollector(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cloudhost_http_requests_total",
		Help: "Total HTTP requests handled by the API and gateway.",
	}, []string{"server", "route", "method", "code"}))
	if err != nil {
		return nil, nil, fmt.Errorf("register HTTP request counter: %w", err)
	}
	counterVec, ok := counter.(*prometheus.CounterVec)
	if !ok {
		return nil, nil, fmt.Errorf("cloudhost_http_requests_total is already registered with an incompatible collector")
	}
	histogram, err := registerCollector(reg, prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cloudhost_http_request_duration_seconds",
		Help:    "HTTP request duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"server", "route", "method"}))
	if err != nil {
		return nil, nil, fmt.Errorf("register HTTP request duration histogram: %w", err)
	}
	histogramVec, ok := histogram.(*prometheus.HistogramVec)
	if !ok {
		return nil, nil, fmt.Errorf("cloudhost_http_request_duration_seconds is already registered with an incompatible collector")
	}
	return counterVec, histogramVec, nil
}

func registerCollector(reg prometheus.Registerer, collector prometheus.Collector) (prometheus.Collector, error) {
	err := reg.Register(collector)
	if err == nil {
		return collector, nil
	}
	var already prometheus.AlreadyRegisteredError
	if errors.As(err, &already) {
		return already.ExistingCollector, nil
	}
	return nil, err
}

type errorResponse struct {
	Error      errorBody     `json:"error"`
	RetryAfter time.Duration `json:"-"`
}

type errorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

func errorResponseFor(err error) (errorResponse, int) {
	var appErr *domain.Error
	if !errors.As(err, &appErr) || appErr == nil {
		return errorResponse{Error: errorBody{Code: string(domain.CodeInternal), Message: "internal error", Details: map[string]any{}}}, http.StatusInternalServerError
	}

	code := appErr.Code
	status := statusForCode(code)
	if status == http.StatusInternalServerError && code != domain.CodeInternal {
		return errorResponse{Error: errorBody{Code: string(domain.CodeInternal), Message: "internal error", Details: map[string]any{}}}, status
	}
	message := appErr.Message
	if strings.TrimSpace(message) == "" {
		message = "internal error"
	}
	details := appErr.Details
	if details == nil {
		details = map[string]any{}
	}
	response := errorResponse{Error: errorBody{Code: string(code), Message: message, Details: details}}
	response.RetryAfter = appErr.RetryAfter
	return response, status
}

func statusForCode(code domain.ErrorCode) int {
	switch code {
	case domain.CodeInvalidArgument:
		return http.StatusBadRequest
	case domain.CodeUnauthenticated:
		return http.StatusUnauthorized
	case domain.CodeForbidden:
		return http.StatusForbidden
	case domain.CodeNotFound:
		return http.StatusNotFound
	case domain.CodeQuotaExceeded, domain.CodeCapacityExceeded, domain.CodeWorkspaceBusy, domain.CodeWorkspaceConflict, domain.CodeTransferExpired, domain.CodeInvalidState, domain.CodeConflict,
		domain.CodeIdempotencyConflict, domain.CodeEventCursorExpired:
		return http.StatusConflict
	case domain.CodeUnsupported:
		return http.StatusUnprocessableEntity
	case domain.CodeRequestTooLarge, domain.CodeResultTooLarge:
		return http.StatusRequestEntityTooLarge
	case domain.CodeRateLimited:
		return http.StatusTooManyRequests
	case domain.CodeUpstreamError:
		return http.StatusBadGateway
	case domain.CodeHostNotReady:
		return http.StatusServiceUnavailable
	case domain.CodeUpstreamTimeout:
		return http.StatusGatewayTimeout
	case domain.CodeInternal:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

func writeErrorResponse(c *gin.Context, response errorResponse, status int) {
	if response.Error.Details == nil {
		response.Error.Details = map[string]any{}
	}
	if retryAfter := retryAfterHeader(response.Error.Code, response.RetryAfter); retryAfter != "" {
		c.Header("Retry-After", retryAfter)
	}
	c.JSON(status, response)
}

func retryAfterHeader(code string, duration time.Duration) string {
	if code == string(domain.CodeHostNotReady) && duration <= 0 {
		duration = 5 * time.Second
	}
	if duration <= 0 {
		return ""
	}
	seconds := duration / time.Second
	if duration%time.Second != 0 {
		seconds++
	}
	return strconv.FormatInt(int64(seconds), 10)
}

func isAbortHandler(value any) bool {
	err, ok := value.(error)
	return ok && errors.Is(err, http.ErrAbortHandler)
}
