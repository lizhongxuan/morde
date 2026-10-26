package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/control"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/httpserver"
	"morde/server/internal/runtimeclient"
)

const maxControlActionBytes = 4096

type ControlService interface {
	Status(context.Context, identity.Principal, string) (app.ControlStatus, error)
	Applications(context.Context, identity.Principal, string) (app.ApplicationsStatus, error)
	InstallApplication(context.Context, identity.Principal, string, string, string) (app.ApplicationInstallResult, error)
	LaunchApplication(context.Context, identity.Principal, string, string, string) (app.ApplicationLaunchResult, error)
	CurrentApplicationLaunch(context.Context, identity.Principal, string, string) (app.CurrentApplicationLaunchResult, error)
	GetApplicationLaunch(context.Context, identity.Principal, string, string, string) (app.ApplicationLaunchResult, error)
	StopApplicationLaunch(context.Context, identity.Principal, string, string, string) (app.ApplicationLaunchResult, error)
	Act(context.Context, identity.Principal, string, app.ControlActionCommand) (app.ControlActionResult, error)
}

type controlTransitionSummary struct {
	RequestID              string         `json:"request_id"`
	Action                 control.Action `json:"action"`
	Phase                  control.Phase  `json:"phase"`
	FailureReason          string         `json:"failure_reason"`
	ExpectedControlVersion int64          `json:"expected_control_version"`
	BlockedControlVersion  int64          `json:"blocked_control_version"`
	TargetControlVersion   int64          `json:"target_control_version"`
	CreatedAt              time.Time      `json:"created_at"`
	UpdatedAt              time.Time      `json:"updated_at"`
}

type controlStatusResponse struct {
	HostID           string                    `json:"host_id"`
	ControlState     host.ControlState         `json:"control_state"`
	ControlVersion   int64                     `json:"control_version"`
	ActiveTransition *controlTransitionSummary `json:"active_transition"`
}

type applicationsStatusResponse struct {
	HostID            string                         `json:"host_id"`
	RuntimeGeneration int64                          `json:"runtime_generation"`
	RuntimeInstanceID string                         `json:"runtime_instance_id"`
	ControlVersion    int64                          `json:"control_version"`
	CheckedAt         time.Time                      `json:"checked_at"`
	Items             []runtimeApplicationStatusItem `json:"items"`
}

type runtimeApplicationStatusItem struct {
	ID               string                              `json:"id"`
	Installed        bool                                `json:"installed"`
	Installable      bool                                `json:"installable"`
	Version          string                              `json:"version"`
	LoginStatus      string                              `json:"login_status"`
	InstallOperation *runtimeApplicationInstallOperation `json:"install_operation"`
}

type runtimeApplicationInstallOperation struct {
	ID              string                        `json:"id"`
	ApplicationID   string                        `json:"application_id"`
	Status          runtimeclient.OperationStatus `json:"status"`
	ProgressPercent int                           `json:"progress_percent"`
	ErrorCode       string                        `json:"error_code"`
	CreatedAt       time.Time                     `json:"created_at"`
	UpdatedAt       time.Time                     `json:"updated_at"`
}

type applicationInstallResponse struct {
	HostID            string                             `json:"host_id"`
	RuntimeGeneration int64                              `json:"runtime_generation"`
	RuntimeInstanceID string                             `json:"runtime_instance_id"`
	ControlVersion    int64                              `json:"control_version"`
	Operation         runtimeApplicationInstallOperation `json:"operation"`
}

type applicationLaunchRequest struct {
	LaunchID string `json:"launch_id"`
}

type applicationLaunchOperationResponse struct {
	ID            string                                `json:"id"`
	ApplicationID string                                `json:"application_id"`
	Status        runtimeclient.ApplicationLaunchStatus `json:"status"`
	ErrorCode     string                                `json:"error_code"`
	CreatedAt     time.Time                             `json:"created_at"`
	UpdatedAt     time.Time                             `json:"updated_at"`
}

type applicationLaunchResponse struct {
	HostID            string                              `json:"host_id"`
	RuntimeGeneration int64                               `json:"runtime_generation"`
	RuntimeInstanceID string                              `json:"runtime_instance_id"`
	ControlVersion    int64                               `json:"control_version"`
	Operation         *applicationLaunchOperationResponse `json:"operation"`
}

type applicationInstallRequest struct {
	RequestID string `json:"request_id"`
}

type controlActionResponse struct {
	controlStatusResponse
	PendingOperationIDs []string `json:"pending_operation_ids"`
}

type controlActionRequest struct {
	Action                 control.Action `json:"action"`
	RequestID              string         `json:"request_id"`
	ExpectedControlVersion *int64         `json:"expected_control_version"`
}

func (g *gateway) getControl(c *gin.Context) {
	hostID := c.GetString(hostIDKey)
	status, err := g.deps.Control.Status(c.Request.Context(), hostControlPrincipal(c), hostID)
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	response, err := toControlStatusResponse(status, hostID)
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (g *gateway) getApplications(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	if c.Request.URL.RawQuery != "" {
		httpserver.WriteError(c, domain.InvalidArgument("query", "unexpected query parameters"))
		return
	}
	hostID := c.GetString(hostIDKey)
	status, err := g.deps.Control.Applications(c.Request.Context(), hostControlPrincipal(c), hostID)
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	response, err := toApplicationsStatusResponse(status, hostID)
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (g *gateway) installApplication(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	if c.Request.URL.RawQuery != "" {
		httpserver.WriteError(c, domain.InvalidArgument("query", "unexpected query parameters"))
		return
	}
	appID := c.Param("app_id")
	if appID != "wechat" {
		httpserver.WriteError(c, domain.InvalidArgument("app_id", "application is not installable"))
		return
	}
	var request applicationInstallRequest
	if !decodeControlActionJSON(c, &request) {
		return
	}
	if !control.ValidRequestID(request.RequestID) {
		httpserver.WriteError(c, domain.InvalidArgument("request_id", "request id is invalid"))
		return
	}
	result, err := g.deps.Control.InstallApplication(c.Request.Context(), hostControlPrincipal(c), c.GetString(hostIDKey), appID, request.RequestID)
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	response, err := toApplicationInstallResponse(result, c.GetString(hostIDKey), appID)
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, response)
}

func (g *gateway) launchApplication(c *gin.Context) {
	g.applicationLaunchHeaders(c)
	if c.Request.URL.RawQuery != "" {
		httpserver.WriteError(c, domain.InvalidArgument("query", "unexpected query parameters"))
		return
	}
	appID := c.Param("app_id")
	if !app.ValidHostApplicationID(appID) {
		httpserver.WriteError(c, domain.InvalidArgument("app_id", "application is not supported"))
		return
	}
	var request applicationLaunchRequest
	if !decodeControlActionJSON(c, &request) {
		return
	}
	if !app.ValidApplicationLaunchID(request.LaunchID) {
		httpserver.WriteError(c, domain.InvalidArgument("launch_id", "launch id is invalid"))
		return
	}
	if !g.requireApplicationControlReady(c) {
		return
	}
	guard, leaseCtx, ok := g.acquireApplicationActivityLease(c)
	if !ok {
		return
	}
	defer g.closeApplicationActivityLease(guard)
	c.Request = c.Request.WithContext(leaseCtx)
	result, err := g.deps.Control.LaunchApplication(leaseCtx, hostControlPrincipal(c), c.GetString(hostIDKey), appID, request.LaunchID)
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	response, err := toApplicationLaunchResponse(result.HostID, result.RuntimeGeneration, result.RuntimeInstanceID, result.ControlVersion, &result.Operation, c.GetString(hostIDKey), appID)
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, response)
}

func (g *gateway) getCurrentApplicationLaunch(c *gin.Context) {
	g.applicationLaunchHeaders(c)
	if c.Request.URL.RawQuery != "" {
		httpserver.WriteError(c, domain.InvalidArgument("query", "unexpected query parameters"))
		return
	}
	appID := c.Param("app_id")
	if !app.ValidHostApplicationID(appID) {
		httpserver.WriteError(c, domain.InvalidArgument("app_id", "application is not supported"))
		return
	}
	result, err := g.deps.Control.CurrentApplicationLaunch(c.Request.Context(), hostControlPrincipal(c), c.GetString(hostIDKey), appID)
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	response, err := toApplicationLaunchResponse(result.HostID, result.RuntimeGeneration, result.RuntimeInstanceID, result.ControlVersion, result.Operation, c.GetString(hostIDKey), appID)
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (g *gateway) getApplicationLaunch(c *gin.Context) {
	g.applicationLaunchHeaders(c)
	if c.Request.URL.RawQuery != "" {
		httpserver.WriteError(c, domain.InvalidArgument("query", "unexpected query parameters"))
		return
	}
	appID := c.Param("app_id")
	if !app.ValidHostApplicationID(appID) {
		httpserver.WriteError(c, domain.InvalidArgument("app_id", "application is not supported"))
		return
	}
	launchID := c.Param("launch_id")
	if !app.ValidApplicationLaunchID(launchID) {
		httpserver.WriteError(c, domain.NewError(domain.CodeNotFound, "application launch not found"))
		return
	}
	result, err := g.deps.Control.GetApplicationLaunch(c.Request.Context(), hostControlPrincipal(c), c.GetString(hostIDKey), appID, launchID)
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	response, err := toApplicationLaunchResponse(result.HostID, result.RuntimeGeneration, result.RuntimeInstanceID, result.ControlVersion, &result.Operation, c.GetString(hostIDKey), appID)
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func (g *gateway) stopApplicationLaunch(c *gin.Context) {
	g.applicationLaunchHeaders(c)
	if c.Request.URL.RawQuery != "" {
		httpserver.WriteError(c, domain.InvalidArgument("query", "unexpected query parameters"))
		return
	}
	appID := c.Param("app_id")
	if !app.ValidHostApplicationID(appID) {
		httpserver.WriteError(c, domain.InvalidArgument("app_id", "application is not supported"))
		return
	}
	launchID := c.Param("launch_id")
	if !app.ValidApplicationLaunchID(launchID) {
		httpserver.WriteError(c, domain.NewError(domain.CodeNotFound, "application launch not found"))
		return
	}
	body, bodyErr := io.ReadAll(io.LimitReader(c.Request.Body, 1))
	if bodyErr != nil || len(body) > 0 {
		httpserver.WriteError(c, domain.InvalidArgument("body", "request body must be empty"))
		return
	}
	if !g.requireApplicationControlReady(c) {
		return
	}
	guard, leaseCtx, ok := g.acquireApplicationActivityLease(c)
	if !ok {
		return
	}
	defer g.closeApplicationActivityLease(guard)
	c.Request = c.Request.WithContext(leaseCtx)
	result, err := g.deps.Control.StopApplicationLaunch(leaseCtx, hostControlPrincipal(c), c.GetString(hostIDKey), appID, launchID)
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	response, err := toApplicationLaunchResponse(result.HostID, result.RuntimeGeneration, result.RuntimeInstanceID, result.ControlVersion, &result.Operation, c.GetString(hostIDKey), appID)
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, response)
}

func (g *gateway) requireApplicationControlReady(c *gin.Context) bool {
	ref := hostRefFrom(c)
	return g.requireRunning(c, ref) && g.requireStableControl(c, ref)
}

func (g *gateway) acquireApplicationActivityLease(c *gin.Context) (*hostActivityLeaseGuard, context.Context, bool) {
	guard, leaseCtx, err := g.acquireHostActivityLease(c.Request.Context(), hostRefFrom(c), app.HostActivityLeaseKindGateway, g.memberAuthorizationCheck(c))
	if err != nil {
		if domain.CodeOf(err) == domain.CodeConflict {
			notReady := domain.NewError(domain.CodeHostNotReady, "Host control or runtime state changed")
			notReady.RetryAfter = time.Second
			err = notReady
		}
		httpserver.WriteError(c, err)
		return nil, nil, false
	}
	return guard, leaseCtx, true
}

func (g *gateway) closeApplicationActivityLease(guard *hostActivityLeaseGuard) {
	if guard != nil && guard.Close() != nil {
		g.deps.Logger.Warn("gateway activity lease release failed", zap.String("kind", app.HostActivityLeaseKindGateway))
	}
}

func (g *gateway) applicationLaunchHeaders(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
}

func toApplicationsStatusResponse(value app.ApplicationsStatus, expectedHostID string) (applicationsStatusResponse, error) {
	if !domain.ValidHostID(value.HostID) || value.HostID != expectedHostID || value.RuntimeGeneration < 1 ||
		value.RuntimeInstanceID == "" || value.ControlVersion < 0 || value.CheckedAt.IsZero() || value.Items == nil {
		return applicationsStatusResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	items := make([]runtimeApplicationStatusItem, len(value.Items))
	seen := make(map[string]struct{}, len(value.Items))
	for index, item := range value.Items {
		if !runtimeclient.ValidApplicationStatusItem(item) {
			return applicationsStatusResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		if _, ok := seen[item.ID]; ok {
			return applicationsStatusResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		seen[item.ID] = struct{}{}
		items[index] = runtimeApplicationStatusItem{ID: item.ID, Installed: item.Installed, Installable: item.Installable, Version: item.Version, LoginStatus: string(item.LoginStatus)}
		if item.InstallOperation != nil {
			if !runtimeclient.ValidApplicationInstallOperation(*item.InstallOperation) || item.InstallOperation.ApplicationID != item.ID {
				return applicationsStatusResponse{}, domain.NewError(domain.CodeInternal, "internal error")
			}
			operation := toRuntimeApplicationInstallOperation(*item.InstallOperation)
			items[index].InstallOperation = &operation
		}
	}
	_, hasCodex := seen["codex"]
	_, hasWechat := seen["wechat"]
	if len(items) != 2 || !hasCodex || !hasWechat {
		return applicationsStatusResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return applicationsStatusResponse{
		HostID: value.HostID, RuntimeGeneration: value.RuntimeGeneration, RuntimeInstanceID: value.RuntimeInstanceID,
		ControlVersion: value.ControlVersion, CheckedAt: value.CheckedAt.UTC(), Items: items,
	}, nil
}

func toApplicationInstallResponse(value app.ApplicationInstallResult, expectedHostID, expectedAppID string) (applicationInstallResponse, error) {
	if !domain.ValidHostID(value.HostID) || value.HostID != expectedHostID || value.RuntimeGeneration < 1 ||
		value.RuntimeInstanceID == "" || value.ControlVersion < 0 || !runtimeclient.ValidApplicationInstallOperation(value.Operation) ||
		value.Operation.ApplicationID != expectedAppID {
		return applicationInstallResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return applicationInstallResponse{
		HostID: value.HostID, RuntimeGeneration: value.RuntimeGeneration, RuntimeInstanceID: value.RuntimeInstanceID,
		ControlVersion: value.ControlVersion, Operation: toRuntimeApplicationInstallOperation(value.Operation),
	}, nil
}

func toApplicationLaunchResponse(hostID string, generation int64, instanceID string, controlVersion int64, operation *runtimeclient.ApplicationLaunchOperation, expectedHostID, expectedApplicationID string) (applicationLaunchResponse, error) {
	if !domain.ValidHostID(hostID) || hostID != expectedHostID || generation < 1 || len(instanceID) > 128 || controlVersion < 0 {
		return applicationLaunchResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	response := applicationLaunchResponse{
		HostID: hostID, RuntimeGeneration: generation, RuntimeInstanceID: instanceID, ControlVersion: controlVersion,
	}
	if operation != nil {
		if !runtimeclient.ValidApplicationLaunchOperation(*operation) || operation.ApplicationID != expectedApplicationID {
			return applicationLaunchResponse{}, domain.NewError(domain.CodeInternal, "internal error")
		}
		response.Operation = &applicationLaunchOperationResponse{
			ID: operation.ID, ApplicationID: operation.ApplicationID, Status: operation.Status, ErrorCode: operation.ErrorCode,
			CreatedAt: operation.CreatedAt.UTC(), UpdatedAt: operation.UpdatedAt.UTC(),
		}
	}
	return response, nil
}

func toRuntimeApplicationInstallOperation(value runtimeclient.ApplicationInstallOperation) runtimeApplicationInstallOperation {
	return runtimeApplicationInstallOperation{
		ID: value.ID, ApplicationID: value.ApplicationID, Status: value.Status, ProgressPercent: value.ProgressPercent,
		ErrorCode: value.ErrorCode, CreatedAt: value.CreatedAt.UTC(), UpdatedAt: value.UpdatedAt.UTC(),
	}
}

func (g *gateway) actOnControl(c *gin.Context) {
	var request controlActionRequest
	if !decodeControlActionJSON(c, &request) {
		return
	}
	if request.Action != control.ActionTakeOver && request.Action != control.ActionRelease {
		httpserver.WriteError(c, domain.InvalidArgument("action", "control action is invalid"))
		return
	}
	if !control.ValidRequestID(request.RequestID) {
		httpserver.WriteError(c, domain.InvalidArgument("request_id", "request id is invalid"))
		return
	}
	if request.ExpectedControlVersion == nil || *request.ExpectedControlVersion < 0 || *request.ExpectedControlVersion > math.MaxInt64-2 {
		httpserver.WriteError(c, domain.InvalidArgument("expected_control_version", "control version is out of range"))
		return
	}

	hostID := c.GetString(hostIDKey)
	result, err := g.deps.Control.Act(c.Request.Context(), hostControlPrincipal(c), hostID, app.ControlActionCommand{
		Action: request.Action, RequestID: request.RequestID, ExpectedControlVersion: *request.ExpectedControlVersion,
	})
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	status, err := toControlStatusResponse(result.Status, hostID)
	if err != nil {
		httpserver.WriteError(c, err)
		return
	}
	pending := make([]string, 0, len(result.PendingOperationIDs))
	seen := make(map[string]struct{}, len(result.PendingOperationIDs))
	for _, operationID := range result.PendingOperationIDs {
		if !domain.ValidOperationID(operationID) {
			httpserver.WriteError(c, domain.NewError(domain.CodeInternal, "internal error"))
			return
		}
		if _, ok := seen[operationID]; ok {
			httpserver.WriteError(c, domain.NewError(domain.CodeInternal, "internal error"))
			return
		}
		seen[operationID] = struct{}{}
		pending = append(pending, operationID)
	}
	response := controlActionResponse{controlStatusResponse: status, PendingOperationIDs: pending}
	statusCode := http.StatusOK
	if len(pending) > 0 {
		statusCode = http.StatusAccepted
	}
	c.JSON(statusCode, response)
}

func hostControlPrincipal(c *gin.Context) identity.Principal {
	// The verified cookie and Host header already bind this request to one owner Host.
	p := principalFrom(c)
	p.Kind = identity.KindSession
	p.HostScope = ""
	return p
}

func decodeControlActionJSON(c *gin.Context, target any) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		httpserver.WriteError(c, domain.InvalidArgument("body", "content type must be application/json"))
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maxControlActionBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeControlDecodeError(c, err)
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple json values")
		}
		writeControlDecodeError(c, err)
		return false
	}
	return true
}

func writeControlDecodeError(c *gin.Context, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		httpserver.WriteError(c, domain.NewError(domain.CodeRequestTooLarge, "request body too large"))
		return
	}
	httpserver.WriteError(c, domain.InvalidArgument("body", "invalid json request"))
}

func toControlStatusResponse(value app.ControlStatus, expectedHostID string) (controlStatusResponse, error) {
	if !domain.ValidHostID(value.HostID) || value.HostID != expectedHostID || !validControlState(value.ControlState) || value.ControlVersion < 0 {
		return controlStatusResponse{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	var transition *controlTransitionSummary
	if value.ActiveTransition != nil {
		converted, err := toControlTransitionSummary(*value.ActiveTransition, value.HostID)
		if err != nil {
			return controlStatusResponse{}, err
		}
		transition = &converted
	}
	return controlStatusResponse{
		HostID: value.HostID, ControlState: value.ControlState,
		ControlVersion: value.ControlVersion, ActiveTransition: transition,
	}, nil
}

func toControlTransitionSummary(value control.Transition, hostID string) (controlTransitionSummary, error) {
	if value.HostID != hostID || !control.ValidRequestID(value.RequestID) ||
		(value.Action != control.ActionTakeOver && value.Action != control.ActionRelease) || !control.ValidPhase(value.Phase) ||
		value.ExpectedControlVersion < 0 || value.ExpectedControlVersion > math.MaxInt64-2 ||
		value.BlockedControlVersion != value.ExpectedControlVersion+1 || value.TargetControlVersion != value.ExpectedControlVersion+2 ||
		value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() || !validPublicControlFailureReason(value.FailureReason) {
		return controlTransitionSummary{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return controlTransitionSummary{
		RequestID: value.RequestID, Action: value.Action, Phase: value.Phase, FailureReason: value.FailureReason,
		ExpectedControlVersion: value.ExpectedControlVersion, BlockedControlVersion: value.BlockedControlVersion,
		TargetControlVersion: value.TargetControlVersion, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}, nil
}

func validControlState(state host.ControlState) bool {
	switch state {
	case host.ControlAI, host.ControlTakingOver, host.ControlUser, host.ControlReturning:
		return true
	default:
		return false
	}
}

func validPublicControlFailureReason(reason string) bool {
	switch reason {
	case "", "audit_unavailable", "task_snapshot_failed", "runtime_unavailable", "transition_persistence_failed",
		"operations_pending", "runtime_fence_mismatch", "runtime_quiesce_failed", "receipt_invalid",
		"runtime_control_failed", "invalid_action":
		return true
	default:
		return false
	}
}
