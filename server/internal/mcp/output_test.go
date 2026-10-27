package mcp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"

	"morde/server/internal/config"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
)

func TestOutputLimitsAndSafeErrors(t *testing.T) {
	service := &service{deps: Deps{Config: config.MCP{MaxOutputBytes: 8}}}
	result, err := service.successResult(map[string]any{"text": strings.Repeat("x", 20)})
	if err != nil || len(result.Content) != 1 || len(textResult(result)) > 8 || !strings.HasSuffix(textResult(result), "...") || result.StructuredContent != nil {
		t.Fatalf("truncated result=%+v err=%v", result, err)
	}
	for limit := 1; limit <= 3; limit++ {
		service.deps.Config.MaxOutputBytes = limit
		result, err = service.successResult("long output")
		if err != nil || len(textResult(result)) > limit {
			t.Fatalf("limit %d result=%q err=%v", limit, textResult(result), err)
		}
	}
	service.deps.Config.MaxOutputBytes = 8
	result, err = service.successResult(map[string]any{"text": "中文结果"})
	if err != nil || len(textResult(result)) > 8 {
		t.Fatalf("UTF-8 truncated result=%q err=%v", textResult(result), err)
	}
	if _, err := service.successResult(make(chan int)); err == nil {
		t.Fatal("successResult accepted non-JSON output")
	}
	errorResult := toolErrorResult(domain.NewError(domain.CodeInternal, "secret detail"))
	if !errorResult.IsError || strings.Contains(textResult(errorResult), "secret detail") || !strings.Contains(textResult(errorResult), `"code":"internal"`) {
		t.Fatalf("unsafe tool error: %+v", errorResult)
	}
}

func TestToolErrorsPanicAndAuditFailure(t *testing.T) {
	for _, panicGet := range []bool{false, true} {
		registry := prometheus.NewRegistry()
		metrics, err := registerToolMetrics(registry)
		if err != nil {
			t.Fatal(err)
		}
		auditFailures, err := registerCounter(registry, prometheus.CounterOpts{Name: "cloudhost_audit_write_failures_total", Help: "Audit write failures."})
		if err != nil {
			t.Fatal(err)
		}
		hosts := &hostServiceStub{getErr: domain.NewError(domain.CodeNotFound, "host not found"), panicGet: panicGet}
		service := &service{
			deps: Deps{Hosts: hosts, Audit: failingAudit{}, Logger: zap.NewNop()}, metrics: metrics, auditFailures: auditFailures,
		}
		request := &sdk.CallToolRequest{
			Params: &sdk.CallToolParamsRaw{Arguments: []byte(`{"host_id":"h1234567"}`)},
			Extra: &sdk.RequestExtra{
				TokenInfo: &auth.TokenInfo{UserID: "user_test", Extra: map[string]any{
					principalTokenInfoKey: identity.Principal{
						UserID: "user_test", Role: identity.RoleUser, Kind: identity.KindAPIKey,
						TokenID: "token_test",
					},
				}},
				Header: http.Header{"X-Request-Id": []string{"request_test"}},
			},
		}
		ctx := context.Background()
		result, err := service.handleTool(toolByName("host_get"))(ctx, request)
		if err != nil || !result.IsError {
			t.Fatalf("panic=%v result=%+v err=%v", panicGet, result, err)
		}
		wantCode := `"code":"not_found"`
		if panicGet {
			wantCode = `"code":"internal"`
		}
		if !strings.Contains(textResult(result), wantCode) {
			t.Fatalf("panic=%v result=%s, want %s", panicGet, textResult(result), wantCode)
		}
		if got := metricValue(t, auditFailures); got != 1 {
			t.Fatalf("audit failure metric = %v, want 1", got)
		}
	}
	registry := prometheus.NewRegistry()
	metrics, err := registerToolMetrics(registry)
	if err != nil {
		t.Fatal(err)
	}
	service := &service{deps: Deps{Config: config.MCP{MaxOutputBytes: 100}}, metrics: metrics}
	if result, err := service.handleTool(toolByName("host_get"))(context.Background(), nil); err != nil || !result.IsError {
		t.Fatalf("missing call params result=%+v err=%v", result, err)
	}
	legacyCtx := context.WithValue(context.Background(), principalContextKey{}, identity.Principal{UserID: "user_test"})
	request := &sdk.CallToolRequest{Params: &sdk.CallToolParamsRaw{Arguments: []byte(`{"host_id":"host_test"}`)}}
	result, err := service.handleTool(toolByName("host_get"))(legacyCtx, request)
	if err != nil || !result.IsError || !strings.Contains(textResult(result), `"code":"unauthenticated"`) {
		t.Fatalf("tool used a principal from connection context: result=%+v err=%v", result, err)
	}
}

func TestMCPToolAuditSurvivesRequestCancellation(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics, err := registerToolMetrics(registry)
	if err != nil {
		t.Fatal(err)
	}
	auditFailures, err := registerCounter(registry, prometheus.CounterOpts{
		Name: "cloudhost_audit_write_failures_total",
		Help: "Audit write failures.",
	})
	if err != nil {
		t.Fatal(err)
	}
	audits := &contextAwareAudit{}
	service := &service{
		deps: Deps{
			Hosts: &hostServiceStub{}, Audit: audits, Logger: zap.NewNop(),
		},
		metrics: metrics, auditFailures: auditFailures,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := &sdk.CallToolRequest{
		Params: &sdk.CallToolParamsRaw{Arguments: []byte(`{}`)},
		Extra: &sdk.RequestExtra{TokenInfo: &auth.TokenInfo{UserID: "user_test", Extra: map[string]any{
			principalTokenInfoKey: identity.Principal{
				UserID: "user_test", Role: identity.RoleUser, Kind: identity.KindAPIKey,
				TokenID: "token_test", Scopes: []identity.Scope{identity.ScopeHostRead},
			},
		}}},
	}

	result, err := service.handleTool(toolByName("host_list"))(ctx, request)
	if err != nil || result == nil || result.IsError {
		t.Fatalf("host_list result=%+v err=%v", result, err)
	}
	if audits.writes != 1 {
		t.Fatalf("audit writes = %d, want 1; audit context error = %v", audits.writes, audits.ctxErr)
	}
	if got := metricValue(t, auditFailures); got != 0 {
		t.Fatalf("audit failure metric = %v, want 0", got)
	}
}

type contextAwareAudit struct {
	writes int
	ctxErr error
}

func (a *contextAwareAudit) Write(ctx context.Context, _ audit.Entry) error {
	if err := ctx.Err(); err != nil {
		a.ctxErr = err
		return err
	}
	a.writes++
	return nil
}

type failingAudit struct{}

func (failingAudit) Write(context.Context, audit.Entry) error { return errors.New("db secret detail") }

func metricValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	metrics := make(chan prometheus.Metric, 1)
	counter.Collect(metrics)
	metric := <-metrics
	var value dto.Metric
	if err := metric.Write(&value); err != nil {
		t.Fatal(err)
	}
	return value.GetCounter().GetValue()
}

func textResult(result *sdk.CallToolResult) string {
	if result == nil || len(result.Content) == 0 {
		return ""
	}
	content, _ := result.Content[0].(*sdk.TextContent)
	if content == nil {
		return ""
	}
	return content.Text
}
