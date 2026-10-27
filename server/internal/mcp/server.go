package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/config"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

type Deps struct {
	PreviewPortMin  int
	PreviewPortDeny []int
	Config          config.MCP
	Domains         config.Domains
	Hosts           app.HostService
	Applications    applicationStatusService
	RuntimeTools    runtimeComputerTools
	Auth            app.Verifier
	Audit           app.AuditWriter
	Logger          *zap.Logger
	Metrics         prometheus.Registerer
}

type principalContextKey struct{}
type requestAuthorizationContextKey struct{}

const principalTokenInfoKey = "cloudhost_principal"
const requestAuthorizationTokenInfoKey = "cloudhost_request_authorization_context"

type service struct {
	deps          Deps
	metrics       toolMetrics
	authFailures  *prometheus.CounterVec
	auditFailures prometheus.Counter
}

// Bound API-key revocation delay for active Streamable HTTP requests.
const apiKeyRevalidationInterval = 5 * time.Second

const mcpAuditWriteTimeout = 2 * time.Second

type computerHostReader interface {
	GetForComputer(context.Context, identity.Principal, string, identity.Scope) (host.Host, error)
}

type runtimeComputerTools interface {
	Execute(context.Context, identity.Principal, string, string, string, string, json.RawMessage) (app.MCPRuntimeToolResult, error)
}

type applicationStatusService interface {
	Applications(context.Context, identity.Principal, string) (app.ApplicationsStatus, error)
}

type toolMetrics struct {
	calls    *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func New(d Deps) (http.Handler, error) {
	if !d.Config.Enabled {
		return http.NotFoundHandler(), nil
	}
	if d.Hosts == nil || d.Applications == nil || d.RuntimeTools == nil || d.Auth == nil || d.Audit == nil || d.Logger == nil || d.Metrics == nil {
		return nil, fmt.Errorf("mcp dependencies are required")
	}
	if d.Domains.Scheme != "http" && d.Domains.Scheme != "https" || strings.TrimSpace(d.Domains.HostSuffix) == "" {
		return nil, fmt.Errorf("mcp host domain configuration is invalid")
	}
	if d.Config.MaxOutputBytes <= 0 || d.Config.ScreenshotMaxWidth <= 0 || d.PreviewPortMin < 1 || d.PreviewPortMin > 65535 {
		return nil, fmt.Errorf("mcp output limits must be positive")
	}

	metrics, err := registerToolMetrics(d.Metrics)
	if err != nil {
		return nil, err
	}
	authFailures, err := registerCounterVec(d.Metrics, prometheus.CounterOpts{
		Name: "cloudhost_auth_failures_total",
		Help: "Authentication failures by server and credential kind.",
	}, "server", "kind")
	if err != nil {
		return nil, err
	}
	auditFailures, err := registerCounter(d.Metrics, prometheus.CounterOpts{
		Name: "cloudhost_audit_write_failures_total",
		Help: "Audit write failures.",
	})
	if err != nil {
		return nil, err
	}
	s := &service{deps: d, metrics: metrics, authFailures: authFailures, auditFailures: auditFailures}

	capabilities := &sdk.ServerCapabilities{}
	capabilities.AddExtension("io.modelcontextprotocol/ui", map[string]any{"mimeTypes": []string{observationResourceMIME}})
	server := sdk.NewServer(&sdk.Implementation{Name: "cloudhost", Version: "1.0.0"}, &sdk.ServerOptions{Capabilities: capabilities})
	if err := addObservationResource(server); err != nil {
		return nil, err
	}
	for _, tool := range tools {
		definition := tool
		metadata := sdk.Meta(nil)
		var annotations *sdk.ToolAnnotations
		switch definition.name {
		case desktopScreenshotToolName:
			metadata = sdk.Meta{"ui": map[string]any{
				"resourceUri": observationResourceURI,
				"visibility":  []string{"model"},
			}}
			annotations = &sdk.ToolAnnotations{ReadOnlyHint: true}
		case hostCodexStatusToolName:
			annotations = &sdk.ToolAnnotations{ReadOnlyHint: true}
		}
		server.AddTool(&sdk.Tool{
			Meta:        metadata,
			Annotations: annotations,
			Name:        definition.name,
			Description: definition.description,
			InputSchema: json.RawMessage(definition.schema),
		}, s.handleTool(definition))
	}
	// The loopback reverse proxy preserves the public Host; authenticate validates it before the SDK.
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{DisableLocalhostProtection: true})
	bindSessionUser := auth.RequireBearerToken(func(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
		principal, ok := ctx.Value(principalContextKey{}).(identity.Principal)
		if !ok {
			return nil, auth.ErrInvalidToken
		}
		extra := map[string]any{principalTokenInfoKey: principal}
		if requestCtx, ok := ctx.Value(requestAuthorizationContextKey{}).(context.Context); ok {
			extra[requestAuthorizationTokenInfoKey] = requestCtx
		}
		return &auth.TokenInfo{
			UserID: principal.UserID,
			Extra:  extra,
		}, nil
	}, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(handler)
	return s.authenticate(bindSessionUser), nil
}

func (s *service) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Fields(r.Header.Get("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || !strings.HasPrefix(parts[1], "chk_") {
			s.unauthorized(w)
			return
		}
		principal, err := s.deps.Auth.VerifyAPIKey(r.Context(), parts[1])
		if err != nil {
			s.unauthorized(w)
			return
		}
		if !allowedMCPHost(r.Host, s.deps.Domains.Portal) ||
			(r.Header.Get("Origin") != "" && r.Header.Get("Origin") != s.deps.Domains.Scheme+"://"+r.Host) ||
			len(r.Header.Values("Origin")) > 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), principalContextKey{}, principal)
		requestCtx, cancel := context.WithCancel(ctx)
		requestCtx = context.WithValue(requestCtx, requestAuthorizationContextKey{}, requestCtx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			ticker := time.NewTicker(apiKeyRevalidationInterval)
			defer ticker.Stop()
			for {
				select {
				case <-requestCtx.Done():
					return
				case <-ticker.C:
					current, err := s.deps.Auth.VerifyAPIKey(requestCtx, parts[1])
					if err != nil || !reflect.DeepEqual(current, principal) {
						cancel()
						return
					}
				}
			}
		}()
		next.ServeHTTP(w, r.WithContext(requestCtx))
		cancel()
		<-done
	})
}

func allowedMCPHost(authority, portal string) bool {
	if portal != "" && strings.EqualFold(authority, portal) {
		return true
	}
	parsed, err := url.Parse("//" + authority)
	if err != nil || parsed.Host != authority || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	if strings.EqualFold(parsed.Hostname(), "localhost") {
		return true
	}
	address, err := netip.ParseAddr(parsed.Hostname())
	return err == nil && address.IsLoopback()
}

func (s *service) unauthorized(w http.ResponseWriter) {
	s.authFailures.WithLabelValues("api", "api_key").Inc()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": string(domain.CodeUnauthenticated), "message": "unauthenticated", "details": map[string]any{},
	}})
}

func registerToolMetrics(reg prometheus.Registerer) (toolMetrics, error) {
	calls, err := registerCounterVec(reg, prometheus.CounterOpts{
		Name: "cloudhost_mcp_tool_calls_total",
		Help: "MCP tool calls.",
	}, "tool", "result")
	if err != nil {
		return toolMetrics{}, err
	}
	duration, err := registerHistogramVec(reg, prometheus.HistogramOpts{
		Name: "cloudhost_mcp_tool_duration_seconds",
		Help: "MCP tool call duration in seconds.",
	}, "tool")
	if err != nil {
		return toolMetrics{}, err
	}
	return toolMetrics{calls: calls, duration: duration}, nil
}

func registerCounterVec(reg prometheus.Registerer, opts prometheus.CounterOpts, labels ...string) (*prometheus.CounterVec, error) {
	collector := prometheus.NewCounterVec(opts, labels)
	if err := reg.Register(collector); err != nil {
		var already prometheus.AlreadyRegisteredError
		if !errors.As(err, &already) {
			return nil, err
		}
		existing, ok := already.ExistingCollector.(*prometheus.CounterVec)
		if !ok {
			return nil, fmt.Errorf("metric %s was registered with an incompatible collector", opts.Name)
		}
		collector = existing
	}
	return collector, nil
}

func registerHistogramVec(reg prometheus.Registerer, opts prometheus.HistogramOpts, labels ...string) (*prometheus.HistogramVec, error) {
	collector := prometheus.NewHistogramVec(opts, labels)
	if err := reg.Register(collector); err != nil {
		var already prometheus.AlreadyRegisteredError
		if !errors.As(err, &already) {
			return nil, err
		}
		existing, ok := already.ExistingCollector.(*prometheus.HistogramVec)
		if !ok {
			return nil, fmt.Errorf("metric %s was registered with an incompatible collector", opts.Name)
		}
		collector = existing
	}
	return collector, nil
}

func registerCounter(reg prometheus.Registerer, opts prometheus.CounterOpts) (prometheus.Counter, error) {
	collector := prometheus.NewCounter(opts)
	if err := reg.Register(collector); err != nil {
		var already prometheus.AlreadyRegisteredError
		if !errors.As(err, &already) {
			return nil, err
		}
		existing, ok := already.ExistingCollector.(prometheus.Counter)
		if !ok {
			return nil, fmt.Errorf("metric %s was registered with an incompatible collector", opts.Name)
		}
		collector = existing
	}
	return collector, nil
}

func (s *service) recordAudit(ctx context.Context, p identity.Principal, name, hostID string, elapsed time.Duration, ok bool, requestID string) {
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mcpAuditWriteTimeout)
	defer cancel()
	if err := s.deps.Audit.Write(auditCtx, audit.Entry{
		ActorID: p.UserID, ActorKind: audit.ActorAPIKey, Action: audit.ActionMCPCall, Target: hostID,
		Detail: map[string]any{"tool": name, "host_id": hostID, "duration_ms": elapsed.Milliseconds(), "ok": ok},
	}); err != nil {
		s.deps.Logger.Error("write audit failed", zap.String("action", string(audit.ActionMCPCall)), zap.String("tool", name), zap.String("host_id", hostID), zap.String("request_id", requestID))
		s.auditFailures.Inc()
	}
}

func (s *service) computerHost(ctx context.Context, p identity.Principal, hostID string, scope identity.Scope) (host.Host, error) {
	reader, ok := s.deps.Hosts.(computerHostReader)
	if !ok {
		return host.Host{}, domain.NewError(domain.CodeInternal, "computer host authorization is unavailable")
	}
	value, err := reader.GetForComputer(ctx, p, hostID, scope)
	if err != nil {
		return host.Host{}, err
	}
	if value.State == host.StateCreating {
		return host.Host{}, domain.NewError(domain.CodeHostNotReady, "host is not ready")
	}
	if value.State != host.StateRunning {
		return host.Host{}, domain.NewError(domain.CodeInvalidState, "host is not running")
	}
	return value, nil
}
