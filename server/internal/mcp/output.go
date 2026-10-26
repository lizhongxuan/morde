package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"runtime/debug"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/runtimeclient"
)

type codexStatusResult struct {
	HostID      string    `json:"host_id"`
	Installed   bool      `json:"installed"`
	Version     string    `json:"version,omitempty"`
	LoginStatus string    `json:"login_status"`
	CheckedAt   time.Time `json:"checked_at"`
}

func (s *service) handleTool(spec toolSpec) sdk.ToolHandler {
	return func(ctx context.Context, request *sdk.CallToolRequest) (result *sdk.CallToolResult, _ error) {
		var requestAuthorizationCtx context.Context
		if request != nil && request.Extra != nil && request.Extra.TokenInfo != nil {
			if requestCtx, ok := request.Extra.TokenInfo.Extra[requestAuthorizationTokenInfoKey].(context.Context); ok {
				requestAuthorizationCtx = requestCtx
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				stop := context.AfterFunc(requestCtx, cancel)
				if requestCtx.Err() != nil {
					cancel()
				}
				defer func() {
					stop()
					cancel()
				}()
			}
		}
		started := time.Now()
		var args map[string]any
		var output any
		var image *sdk.ImageContent
		var serialized *sdk.CallToolResult
		var callErr error
		var imageObservedAt time.Time
		principal, authenticated := identity.Principal{}, false
		if request != nil && request.Extra != nil && request.Extra.TokenInfo != nil {
			principal, authenticated = request.Extra.TokenInfo.Extra[principalTokenInfoKey].(identity.Principal)
		}
		requestID := requestIDFrom(request)
		if !authenticated {
			callErr = domain.NewError(domain.CodeUnauthenticated, "unauthenticated")
		}
		if callErr == nil {
			if request == nil || request.Params == nil {
				callErr = domain.InvalidArgument("arguments", "tool arguments are required")
			} else {
				args, callErr = validateToolArguments(spec, request.Params.Arguments)
			}
		}
		hostID := stringArg(args, "host_id")
		if callErr == nil {
			callErr = authorizeTool(principal, spec.name, args)
		}
		if callErr == nil {
			func() {
				defer func() {
					if value := recover(); value != nil {
						s.deps.Logger.Error("MCP tool panic", zap.String("tool", spec.name), zap.String("host_id", hostID), zap.String("request_id", requestID), zap.ByteString("stack", debug.Stack()))
						callErr = domain.NewError(domain.CodeInternal, "internal error")
					}
				}()
				protocolSessionID := ""
				if request != nil && request.Session != nil {
					protocolSessionID = request.Session.ID()
				}
				if requestAuthorizationCtx != nil && requestAuthorizationCtx.Err() != nil {
					callErr = context.Canceled
					return
				}
				output, image, callErr = s.dispatch(ctx, principal, protocolSessionID, spec, args)
				if callErr == nil && image != nil && spec.name == desktopScreenshotToolName {
					imageObservedAt = time.Now().UTC()
				}
			}()
		}
		if callErr == nil && image == nil {
			serialized, callErr = s.successResult(output)
		}
		elapsed := time.Since(started)
		resultLabel := "ok"
		if callErr != nil {
			resultLabel = "error"
		}
		s.metrics.calls.WithLabelValues(spec.name, resultLabel).Inc()
		s.metrics.duration.WithLabelValues(spec.name).Observe(elapsed.Seconds())
		if authenticated {
			s.recordAudit(ctx, principal, spec.name, hostID, elapsed, callErr == nil, requestID)
		}
		if callErr != nil {
			code := domain.CodeOf(callErr)
			if code == domain.CodeInternal || code == domain.CodeUpstreamError || code == domain.CodeUpstreamTimeout {
				s.deps.Logger.Warn("MCP tool failed", zap.String("tool", spec.name), zap.String("host_id", hostID), zap.String("request_id", requestID), zap.String("code", string(code)))
			}
			return toolErrorResult(callErr), nil
		}
		if image != nil {
			result := &sdk.CallToolResult{Content: []sdk.Content{image}}
			if spec.name == desktopScreenshotToolName {
				result.Meta = sdk.Meta{observationMetaKey: map[string]any{
					"host_id": hostID, "observed_at": imageObservedAt.Format(time.RFC3339Nano),
				}}
			}
			return result, nil
		}
		return serialized, nil
	}
}

func requestIDFrom(request *sdk.CallToolRequest) string {
	if request == nil || request.Extra == nil || request.Extra.Header == nil {
		return ""
	}
	return request.Extra.Header.Get("X-Request-Id")
}

func (s *service) dispatch(ctx context.Context, p identity.Principal, protocolSessionID string, spec toolSpec, args map[string]any) (any, *sdk.ImageContent, error) {
	name := spec.name
	switch name {
	case hostCodexStatusToolName:
		value, err := s.codexStatus(ctx, p, stringArg(args, "host_id"))
		return value, nil, err
	case "host_create", "host_list", "host_get", "host_delete", "host_renew":
		value, err := s.lifecycle(ctx, p, name, args)
		return value, nil, err
	case "preview_url":
		value, err := s.preview(ctx, p, args)
		return value, nil, err
	case "task_submit", "task_list", "task_get", "task_pause", "task_resume", "task_cancel", "task_respond":
		value, err := s.task(ctx, p, name, args)
		return value, nil, err
	case "schedule_prepare", "schedule_create", "schedule_list", "schedule_get", "schedule_update", "schedule_enable", "schedule_disable", "schedule_runs":
		value, err := s.schedule(ctx, p, name, args)
		return value, nil, err
	default:
		value, image, err := s.hostTool(ctx, p, protocolSessionID, spec, args)
		return value, image, err
	}
}

func (s *service) codexStatus(ctx context.Context, p identity.Principal, hostID string) (codexStatusResult, error) {
	status, err := s.deps.Applications.Applications(ctx, p, hostID)
	if err != nil {
		return codexStatusResult{}, err
	}
	if !domain.ValidHostID(status.HostID) || status.HostID != hostID || status.RuntimeGeneration < 1 || status.RuntimeInstanceID == "" ||
		status.ControlVersion < 0 || status.CheckedAt.IsZero() {
		return codexStatusResult{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	var codex *runtimeclient.ApplicationStatusItem
	for i := range status.Items {
		if status.Items[i].ID != "codex" {
			continue
		}
		if codex != nil || !runtimeclient.ValidApplicationStatusItem(status.Items[i]) {
			return codexStatusResult{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		codex = &status.Items[i]
	}
	if codex == nil {
		return codexStatusResult{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return codexStatusResult{
		HostID: status.HostID, Installed: codex.Installed, Version: codex.Version,
		LoginStatus: string(codex.LoginStatus), CheckedAt: status.CheckedAt.UTC(),
	}, nil
}

func authorizeTool(principal identity.Principal, name string, args map[string]any) error {
	if principal.Kind != identity.KindAPIKey {
		return domain.NewError(domain.CodeUnauthenticated, "API key required")
	}
	scopes := []identity.Scope(nil)
	switch name {
	case "host_create":
		scopes = []identity.Scope{identity.ScopeHostCreate}
	case "host_list", "host_get":
		scopes = []identity.Scope{identity.ScopeHostRead}
	case "host_delete":
		scopes = []identity.Scope{identity.ScopeHostManage}
	case "host_renew":
		scopes = []identity.Scope{identity.ScopeHostManage}
	case "task_submit":
		scopes = []identity.Scope{identity.ScopeTaskSubmit}
	case "task_list", "task_get":
		scopes = []identity.Scope{identity.ScopeTaskRead}
	case "task_pause", "task_resume", "task_cancel", "task_respond":
		scopes = []identity.Scope{identity.ScopeTaskControl}
	case "schedule_prepare", "schedule_create", "schedule_update", "schedule_enable", "schedule_disable":
		scopes = []identity.Scope{identity.ScopeScheduleManage}
	case "schedule_list", "schedule_get", "schedule_runs":
		scopes = []identity.Scope{identity.ScopeScheduleRead}
	default:
		if scope, ok := computerScopeForTool(name); ok {
			scopes = []identity.Scope{scope}
		} else {
			return domain.NewError(domain.CodeForbidden, "MCP tool is not authorized")
		}
	}
	for _, scope := range scopes {
		if !principal.HasScope(scope) {
			return domain.NewError(domain.CodeForbidden, "API key scope is insufficient")
		}
	}
	if (name == "task_submit" || name == "schedule_prepare" || name == "schedule_create") && !principal.AllowsHost(stringArg(args, "host_id")) {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	if name != "host_create" && name != "host_list" && name != "task_list" && name != "task_get" &&
		name != "task_pause" && name != "task_resume" && name != "task_cancel" && name != "task_respond" &&
		name != "schedule_list" && name != "schedule_get" && name != "schedule_update" && name != "schedule_enable" && name != "schedule_disable" && name != "schedule_runs" &&
		!principal.AllowsHost(stringArg(args, "host_id")) {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	return nil
}

func computerScopeForTool(name string) (identity.Scope, bool) {
	if name == "preview_url" || name == hostCodexStatusToolName {
		return identity.ScopeComputerObserve, true
	}
	for _, definition := range app.RuntimeComputerTools() {
		if definition.Name == name {
			return definition.Scope, true
		}
	}
	return "", false
}

func (s *service) successResult(value any) (*sdk.CallToolResult, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, domain.NewError(domain.CodeInternal, "internal error")
	}
	content := string(data)
	if len(data) > s.deps.Config.MaxOutputBytes {
		const marker = "..."
		limit := s.deps.Config.MaxOutputBytes
		if limit <= len(marker) {
			content = marker[:limit]
		} else {
			content = strings.ToValidUTF8(string(data[:limit-len(marker)]), "") + marker
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: content}}}, nil
	}
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: content}}, StructuredContent: value}, nil
}

func toolErrorResult(err error) *sdk.CallToolResult {
	code := domain.CodeOf(err)
	message := "internal error"
	var domainErr *domain.Error
	if errors.As(err, &domainErr) {
		message = domainErr.Message
	}
	switch code {
	case domain.CodeUpstreamError:
		message = "upstream request failed"
	case domain.CodeUpstreamTimeout:
		message = "upstream request timed out"
	case domain.CodeInternal:
		message = "internal error"
	}
	data, _ := json.Marshal(map[string]string{"code": string(code), "message": message})
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(data)}}, IsError: true}
}

func stringArg(args map[string]any, name string) string {
	value, _ := args[name].(string)
	return value
}

func intArg(args map[string]any, name string) (int64, bool) {
	value, ok := args[name].(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := value.Int64()
	return parsed, err == nil
}
