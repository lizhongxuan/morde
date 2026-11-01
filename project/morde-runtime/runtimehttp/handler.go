// Package runtimehttp implements the sole HTTP transport for morde-runtime v1.
package runtimehttp

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"morde-runtime/contract"
	"morde-runtime/runtimeapplications"
	"morde-runtime/runtimecore"
)

const (
	MaxRequestBytes  = runtimecore.MaxRequestBytes
	MaxResponseBytes = runtimecore.MaxResponseBytes
)

type handler struct {
	manager    *runtimecore.Manager
	credential string
	probeCodex func(context.Context) contract.ApplicationStatusItem
	installer  applicationInstaller
	launcher   applicationLauncher
}

type applicationInstaller interface {
	Statuses() []contract.ApplicationStatusItem
	Install(string, string) (contract.ApplicationInstallOperation, error)
}

type applicationLauncher interface {
	Launch(string, contract.HostFence, string) (contract.ApplicationLaunchOperation, error)
	QueryLaunch(string, contract.HostFence, string) (contract.ApplicationLaunchOperation, error)
	StopLaunch(string, contract.HostFence, string) (contract.ApplicationLaunchOperation, error)
}

// NewHandler constructs the only protocol handler. The credential is retained
// in memory solely for constant-time bearer checks; it is never logged.
func NewHandler(manager *runtimecore.Manager, credential string) (http.Handler, error) {
	if manager == nil || !validCredential(credential) {
		return nil, errors.New("runtime manager and 256-bit credential are required")
	}
	return newHandler(manager, credential, nil), nil
}

// NewHandlerWithApplications adds the fixed application catalog and installer.
func NewHandlerWithApplications(manager *runtimecore.Manager, credential string, applications applicationInstaller) (http.Handler, error) {
	if manager == nil || !validCredential(credential) || applications == nil {
		return nil, errors.New("runtime manager, applications, and 256-bit credential are required")
	}
	return newHandler(manager, credential, applications), nil
}

func newHandler(manager *runtimecore.Manager, credential string, applications applicationInstaller) *handler {
	handler := &handler{manager: manager, credential: credential, probeCodex: runtimeapplications.ProbeCodex, installer: applications}
	if launcher, ok := applications.(applicationLauncher); ok {
		handler.launcher = launcher
	}
	return handler
}

func (h *handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/healthz":
		writeHealth(writer, http.StatusOK)
		return
	case "/readyz":
		if h.manager.Ready() {
			writeHealth(writer, http.StatusOK)
		} else {
			writeHealth(writer, http.StatusServiceUnavailable)
		}
		return
	}
	if !strings.HasPrefix(request.URL.Path, "/runtime/v1/") {
		writeError(writer, runtimecore.ErrInvalidRequest)
		return
	}
	if !h.authorized(request) {
		writeError(writer, runtimecore.ErrRuntimeAuthentication)
		return
	}
	if !headerProtocolValid(request) {
		writeError(writer, runtimecore.ErrProtocolIncompatible)
		return
	}
	if err := h.dispatch(writer, request); err != nil {
		writeError(writer, err)
	}
}

func (h *handler) authorized(request *http.Request) bool {
	values := request.Header.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	want := "Bearer " + h.credential
	got := values[0]
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func headerProtocolValid(request *http.Request) bool {
	values := request.Header.Values(contract.ProtocolHeader)
	return len(values) == 1 && values[0] == contract.ProtocolVersion
}

func (h *handler) dispatch(writer http.ResponseWriter, request *http.Request) error {
	path := request.URL.Path
	switch {
	case path == "/runtime/v1/capabilities":
		if request.Method != http.MethodGet || !noBody(request) {
			return runtimecore.ErrInvalidRequest
		}
		query, err := strictQuery(request, "protocol_version", "host_id", "runtime_generation", "control_version")
		if err != nil {
			return err
		}
		if query["protocol_version"] != contract.ProtocolVersion {
			return runtimecore.ErrProtocolIncompatible
		}
		generation, err := parseInt(query["runtime_generation"], 1)
		if err != nil {
			return runtimecore.ErrInvalidRequest
		}
		control, err := parseInt(query["control_version"], 0)
		if err != nil {
			return runtimecore.ErrInvalidRequest
		}
		response, err := h.manager.Capabilities(contract.CapabilitiesRequest{
			ProtocolVersion: query["protocol_version"], HostID: query["host_id"], RuntimeGeneration: generation, ControlVersion: control,
		})
		if err != nil {
			return err
		}
		return writeJSON(writer, response)

	case path == "/runtime/v1/applications":
		return h.applications(writer, request)
	case strings.HasPrefix(path, "/runtime/v1/applications/"):
		parts := strings.Split(path, "/")
		if len(parts) < 6 || parts[1] != "runtime" || parts[2] != "v1" || parts[3] != "applications" || parts[4] == "" {
			return runtimecore.ErrInvalidRequest
		}
		applicationID := parts[4]
		if !runtimeapplications.SupportsApplication(applicationID) {
			return runtimecore.ErrCapabilityUnavailable
		}
		switch {
		case len(parts) == 6 && parts[5] == "install":
			return h.installApplication(writer, request, applicationID)
		case len(parts) == 6 && parts[5] == "launch":
			return h.launchApplication(writer, request, applicationID)
		case len(parts) == 7 && parts[5] == "launches" && parts[6] != "":
			return h.queryApplication(writer, request, applicationID, parts[6])
		case len(parts) == 8 && parts[5] == "launches" && parts[6] != "" && parts[7] == "stop":
			return h.stopApplication(writer, request, applicationID, parts[6])
		default:
			return runtimecore.ErrInvalidRequest
		}

	case path == "/runtime/v1/control" && request.Method == http.MethodPut:
		var body contract.SetControlRequest
		if err := readJSON(request, &body); err != nil {
			return err
		}
		response, err := h.manager.SetControl(body)
		if err != nil {
			return err
		}
		return writeJSON(writer, response)

	case path == "/runtime/v1/workspace-bindings" && request.Method == http.MethodPut:
		var body contract.SetWorkspaceBindingsRequest
		if err := readJSON(request, &body); err != nil {
			return err
		}
		response, err := h.manager.SetWorkspaceBindings(request.Context(), body)
		if err != nil {
			return err
		}
		return writeJSON(writer, response)

	case strings.HasPrefix(path, "/runtime/v1/tasks/"):
		parts := strings.Split(path, "/")
		if len(parts) == 6 && parts[1] == "runtime" && parts[2] == "v1" && parts[3] == "tasks" &&
			validTaskID(parts[4]) && parts[5] == "workspace-view" && request.Method == http.MethodDelete {
			var body contract.DeleteTaskWorkspaceViewRequest
			if err := readJSON(request, &body); err != nil {
				return err
			}
			response, err := h.manager.DeleteTaskWorkspaceView(request.Context(), parts[4], body)
			if err != nil {
				return err
			}
			return writeJSON(writer, response)
		}
		if len(parts) == 7 && parts[1] == "runtime" && parts[2] == "v1" && parts[3] == "tasks" &&
			validTaskID(parts[4]) && parts[5] == "workspace-view" && request.Method == http.MethodPost {
			switch parts[6] {
			case "manifest":
				var body contract.TaskWorkspaceManifestRequest
				if err := readJSON(request, &body); err != nil {
					return err
				}
				response, err := h.manager.TaskWorkspaceManifestPage(request.Context(), parts[4], body)
				if err != nil {
					return err
				}
				return writeJSON(writer, response)
			case "block":
				var body contract.TaskWorkspaceBlockRequest
				if err := readJSON(request, &body); err != nil {
					return err
				}
				response, err := h.manager.ReadTaskWorkspaceBlock(request.Context(), parts[4], body)
				if err != nil {
					return err
				}
				return writeJSON(writer, response)
			default:
				return runtimecore.ErrInvalidRequest
			}
		}
		if len(parts) != 6 || parts[1] != "runtime" || parts[2] != "v1" || parts[3] != "tasks" || parts[4] == "" || request.Method != http.MethodPut || !validTaskID(parts[4]) {
			return runtimecore.ErrInvalidRequest
		}
		switch parts[5] {
		case "lease":
			var body contract.SetTaskLeaseRequest
			if err := readJSON(request, &body); err != nil {
				return err
			}
			response, err := h.manager.SetTaskLease(parts[4], body)
			if err != nil {
				return err
			}
			return writeJSON(writer, response)
		case "workspace-view":
			var body contract.SetTaskWorkspaceViewRequest
			if err := readJSON(request, &body); err != nil {
				return err
			}
			response, err := h.manager.SetTaskWorkspaceView(request.Context(), parts[4], body)
			if err != nil {
				return err
			}
			return writeJSON(writer, response)
		default:
			return runtimecore.ErrInvalidRequest
		}

	case path == "/runtime/v1/operations" && request.Method == http.MethodPost:
		var body contract.ExecuteRequest
		if err := readJSON(request, &body); err != nil {
			return err
		}
		operation, err := h.manager.Submit(request.Context(), body)
		if err != nil {
			return err
		}
		return writeJSON(writer, contract.OperationResponse{ProtocolVersion: contract.ProtocolVersion, RequestFence: body.HostFence, Operation: operation})

	case strings.HasPrefix(path, "/runtime/v1/operations/"):
		parts := strings.Split(path, "/")
		if len(parts) == 6 && parts[1] == "runtime" && parts[2] == "v1" && parts[3] == "operations" && parts[4] != "" && parts[5] == "cancel" {
			if request.Method != http.MethodPost || !validOperationID(parts[4]) {
				return runtimecore.ErrInvalidRequest
			}
			var body contract.CancelOperationRequest
			if err := readJSON(request, &body); err != nil {
				return err
			}
			operation, err := h.manager.CancelOperation(request.Context(), body.ProtocolVersion, parts[4], body)
			if err != nil {
				return err
			}
			return writeJSON(writer, contract.OperationResponse{ProtocolVersion: contract.ProtocolVersion, RequestFence: body.HostFence, Operation: operation})
		}
		if len(parts) == 5 && parts[1] == "runtime" && parts[2] == "v1" && parts[3] == "operations" && parts[4] != "" {
			if request.Method != http.MethodGet || !validOperationID(parts[4]) || !noBody(request) {
				return runtimecore.ErrInvalidRequest
			}
			query, err := strictQuery(request, "host_id", "runtime_generation", "runtime_instance_id", "control_version", "task_id", "lease_version")
			if err != nil {
				return err
			}
			generation, genErr := parseInt(query["runtime_generation"], 1)
			control, controlErr := parseInt(query["control_version"], 0)
			lease, leaseErr := parseInt(query["lease_version"], 1)
			if genErr != nil || controlErr != nil || leaseErr != nil {
				return runtimecore.ErrInvalidRequest
			}
			fence := contract.HostFence{HostID: query["host_id"], RuntimeGeneration: generation, RuntimeInstanceID: query["runtime_instance_id"], ControlVersion: control}
			operation, err := h.manager.QueryOperation(request.Header.Get(contract.ProtocolHeader), parts[4], contract.OperationQuery{HostFence: fence, TaskID: query["task_id"], LeaseVersion: lease})
			if err != nil {
				return err
			}
			return writeJSON(writer, contract.OperationResponse{ProtocolVersion: contract.ProtocolVersion, RequestFence: fence, Operation: operation})
		}
		return runtimecore.ErrInvalidRequest

	case path == "/runtime/v1/quiesce" && request.Method == http.MethodPost:
		var body contract.QuiesceRequest
		if err := readJSON(request, &body); err != nil {
			return err
		}
		response, err := h.manager.Quiesce(request.Context(), body)
		if err != nil {
			return err
		}
		return writeJSON(writer, response)
	default:
		return runtimecore.ErrInvalidRequest
	}
}

func (h *handler) launchApplication(writer http.ResponseWriter, request *http.Request, applicationID string) error {
	if request.Method != http.MethodPost || h.launcher == nil {
		if h.launcher == nil {
			return runtimecore.ErrCapabilityUnavailable
		}
		return runtimecore.ErrInvalidRequest
	}
	var body contract.ApplicationLaunchRequest
	if err := readJSON(request, &body); err != nil || !contract.ValidApplicationLaunchRequest(body) {
		return runtimecore.ErrInvalidRequest
	}
	var operation contract.ApplicationLaunchOperation
	committedFence, err := h.manager.RunIfCurrentFence(body.ProtocolVersion, body.HostFence, func() error {
		var launchErr error
		operation, launchErr = h.launcher.Launch(applicationID, body.HostFence, body.LaunchID)
		return launchErr
	})
	if err != nil {
		return applicationLaunchError(err)
	}
	if operation.ID != body.LaunchID || operation.ApplicationID != applicationID {
		return runtimecore.ErrRuntimeStateUnavailable
	}
	response := contract.ApplicationLaunchResponse{ProtocolVersion: contract.ProtocolVersion, HostFence: committedFence, Operation: operation}
	if !contract.ValidApplicationLaunchResponse(response) {
		return runtimecore.ErrRuntimeStateUnavailable
	}
	setNoStore(writer)
	return writeJSONStatus(writer, http.StatusAccepted, response)
}

func (h *handler) queryApplication(writer http.ResponseWriter, request *http.Request, applicationID, launchID string) error {
	if request.Method != http.MethodGet || !noBody(request) || h.launcher == nil {
		if h.launcher == nil {
			return runtimecore.ErrCapabilityUnavailable
		}
		return runtimecore.ErrInvalidRequest
	}
	query, err := strictQuery(request, "protocol_version", "host_id", "runtime_generation", "runtime_instance_id", "control_version")
	if err != nil {
		return runtimecore.ErrInvalidRequest
	}
	if query["protocol_version"] != contract.ProtocolVersion {
		return runtimecore.ErrProtocolIncompatible
	}
	generation, genErr := parseInt(query["runtime_generation"], 1)
	control, controlErr := parseInt(query["control_version"], 0)
	if genErr != nil || controlErr != nil {
		return runtimecore.ErrInvalidRequest
	}
	fence := contract.HostFence{HostID: query["host_id"], RuntimeGeneration: generation, RuntimeInstanceID: query["runtime_instance_id"], ControlVersion: control}
	if !contract.ValidApplicationLaunchRequest(contract.ApplicationLaunchRequest{ProtocolVersion: contract.ProtocolVersion, HostFence: fence, LaunchID: launchID}) {
		return runtimecore.ErrInvalidRequest
	}
	var operation contract.ApplicationLaunchOperation
	committedFence, err := h.manager.RunIfCurrentFence(contract.ProtocolVersion, fence, func() error {
		var queryErr error
		operation, queryErr = h.launcher.QueryLaunch(applicationID, fence, launchID)
		return queryErr
	})
	if err != nil {
		return applicationLaunchError(err)
	}
	if operation.ID != launchID || operation.ApplicationID != applicationID {
		return runtimecore.ErrRuntimeStateUnavailable
	}
	response := contract.ApplicationLaunchResponse{ProtocolVersion: contract.ProtocolVersion, HostFence: committedFence, Operation: operation}
	if !contract.ValidApplicationLaunchResponse(response) {
		return runtimecore.ErrRuntimeStateUnavailable
	}
	setNoStore(writer)
	return writeJSON(writer, response)
}

func (h *handler) stopApplication(writer http.ResponseWriter, request *http.Request, applicationID, pathLaunchID string) error {
	if request.Method != http.MethodPost || h.launcher == nil {
		if h.launcher == nil {
			return runtimecore.ErrCapabilityUnavailable
		}
		return runtimecore.ErrInvalidRequest
	}
	var body contract.ApplicationLaunchRequest
	if err := readJSON(request, &body); err != nil || !contract.ValidApplicationLaunchRequest(body) || body.LaunchID != pathLaunchID {
		return runtimecore.ErrInvalidRequest
	}
	var operation contract.ApplicationLaunchOperation
	committedFence, err := h.manager.RunIfCurrentFence(body.ProtocolVersion, body.HostFence, func() error {
		var stopErr error
		operation, stopErr = h.launcher.StopLaunch(applicationID, body.HostFence, body.LaunchID)
		return stopErr
	})
	if err != nil {
		return applicationLaunchError(err)
	}
	if operation.ID != pathLaunchID || operation.ApplicationID != applicationID {
		return runtimecore.ErrRuntimeStateUnavailable
	}
	response := contract.ApplicationLaunchResponse{ProtocolVersion: contract.ProtocolVersion, HostFence: committedFence, Operation: operation}
	if !contract.ValidApplicationLaunchResponse(response) {
		return runtimecore.ErrRuntimeStateUnavailable
	}
	setNoStore(writer)
	return writeJSONStatus(writer, http.StatusAccepted, response)
}

func applicationLaunchError(err error) error {
	switch {
	case errors.Is(err, runtimeapplications.ErrInvalidLaunchRequest):
		return runtimecore.ErrInvalidRequest
	case errors.Is(err, runtimeapplications.ErrLaunchInProgress):
		return runtimecore.ErrOperationConflict
	case errors.Is(err, runtimeapplications.ErrLaunchNotFound):
		return runtimecore.ErrOperationNotFound
	case errors.Is(err, runtimeapplications.ErrLaunchFenceMismatch):
		return runtimecore.ErrFenceRejected
	default:
		var runtimeError *runtimecore.RuntimeError
		if errors.As(err, &runtimeError) {
			return err
		}
		return runtimecore.ErrRuntimeStateUnavailable
	}
}

func setNoStore(writer http.ResponseWriter) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Pragma", "no-cache")
}

func (h *handler) applications(writer http.ResponseWriter, request *http.Request) error {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Pragma", "no-cache")
	if request.Method != http.MethodGet || !noBody(request) {
		return runtimecore.ErrInvalidRequest
	}
	query, err := strictQuery(request, "protocol_version", "host_id", "runtime_generation", "control_version")
	if err != nil {
		return err
	}
	if query["protocol_version"] != contract.ProtocolVersion {
		return runtimecore.ErrProtocolIncompatible
	}
	generation, err := parseInt(query["runtime_generation"], 1)
	if err != nil {
		return runtimecore.ErrInvalidRequest
	}
	controlVersion, err := parseInt(query["control_version"], 0)
	if err != nil {
		return runtimecore.ErrInvalidRequest
	}
	capabilitiesRequest := contract.CapabilitiesRequest{
		ProtocolVersion: query["protocol_version"], HostID: query["host_id"],
		RuntimeGeneration: generation, ControlVersion: controlVersion,
	}
	before, err := h.manager.Capabilities(capabilitiesRequest)
	if err != nil {
		return err
	}
	if !applicationsFenceMatches(before, capabilitiesRequest) {
		return runtimecore.ErrFenceRejected
	}
	codex := h.probeCodex(request.Context())
	items := []contract.ApplicationStatusItem{codex}
	if h.installer != nil {
		items = append(items, h.installer.Statuses()...)
	} else {
		for _, applicationID := range runtimeapplications.ApplicationIDs() {
			items = append(items, contract.ApplicationStatusItem{ID: applicationID, LoginStatus: contract.ApplicationLoginNotApplicable})
		}
	}
	for _, item := range items {
		if !contract.ValidApplicationStatusItem(item) {
			return runtimecore.ErrRuntimeStateUnavailable
		}
	}
	after, err := h.manager.Capabilities(capabilitiesRequest)
	if err != nil {
		return err
	}
	if !applicationsFenceMatches(after, capabilitiesRequest) || before.RuntimeInstanceID != after.RuntimeInstanceID {
		return runtimecore.ErrFenceRejected
	}
	return writeJSON(writer, contract.ApplicationsResponse{
		ProtocolVersion: contract.ProtocolVersion, HostID: after.HostID, RuntimeGeneration: after.RuntimeGeneration,
		RuntimeInstanceID: after.RuntimeInstanceID, ControlVersion: after.ControlVersion,
		CheckedAt: time.Now().UTC(), Items: items,
	})
}

func (h *handler) installApplication(writer http.ResponseWriter, request *http.Request, applicationID string) error {
	if request.Method != http.MethodPost {
		return runtimecore.ErrInvalidRequest
	}
	var body contract.ApplicationInstallRequest
	if err := readJSON(request, &body); err != nil || !contract.ValidApplicationInstallRequest(body) {
		return runtimecore.ErrInvalidRequest
	}
	if body.HostID == "" || body.HostFence.HostID != body.HostID || body.ProtocolVersion != contract.ProtocolVersion {
		return runtimecore.ErrInvalidRequest
	}
	if h.installer == nil {
		return runtimecore.ErrCapabilityUnavailable
	}
	var operation contract.ApplicationInstallOperation
	committedFence, err := h.manager.RunIfCurrentFence(body.ProtocolVersion, body.HostFence, func() error {
		var installErr error
		operation, installErr = h.installer.Install(applicationID, body.RequestID)
		return installErr
	})
	if err != nil {
		switch {
		case errors.Is(err, runtimeapplications.ErrInvalidInstallRequest):
			return runtimecore.ErrInvalidRequest
		case errors.Is(err, runtimeapplications.ErrUnsupportedApplication):
			return runtimecore.ErrCapabilityUnavailable
		case errors.Is(err, runtimeapplications.ErrInstallInProgress):
			return runtimecore.ErrOperationConflict
		case errors.Is(err, runtimeapplications.ErrArchitectureUnsupported):
			return runtimecore.ErrCapabilityUnavailable
		default:
			var runtimeError *runtimecore.RuntimeError
			if errors.As(err, &runtimeError) {
				return err
			}
			return runtimecore.ErrRuntimeStateUnavailable
		}
	}
	response := contract.ApplicationInstallResponse{
		ProtocolVersion: contract.ProtocolVersion, HostFence: committedFence, Operation: operation,
	}
	if operation.ApplicationID != applicationID || !contract.ValidApplicationInstallResponse(response) {
		return runtimecore.ErrRuntimeStateUnavailable
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Pragma", "no-cache")
	return writeJSONStatus(writer, http.StatusAccepted, response)
}

func applicationsFenceMatches(value contract.CapabilitiesResponse, request contract.CapabilitiesRequest) bool {
	return value.HostID == request.HostID && value.RuntimeGeneration == request.RuntimeGeneration &&
		value.ControlVersion == request.ControlVersion && value.RuntimeInstanceID != ""
}

func readJSON(request *http.Request, target any) error {
	values := request.Header.Values("Content-Type")
	if len(values) != 1 {
		return runtimecore.ErrInvalidRequest
	}
	mediaType, _, err := mime.ParseMediaType(values[0])
	if err != nil || mediaType != "application/json" {
		return runtimecore.ErrInvalidRequest
	}
	data, err := io.ReadAll(io.LimitReader(request.Body, MaxRequestBytes+1))
	if err != nil || len(data) == 0 || len(data) > MaxRequestBytes {
		return runtimecore.ErrInvalidRequest
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return runtimecore.ErrInvalidRequest
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || !jsonEOF(decoder) {
		return runtimecore.ErrInvalidRequest
	}
	return nil
}

func noBody(request *http.Request) bool {
	data, err := io.ReadAll(io.LimitReader(request.Body, 1))
	return err == nil && len(data) == 0
}

func strictQuery(request *http.Request, required ...string) (map[string]string, error) {
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil || len(query) != len(required) {
		return nil, runtimecore.ErrInvalidRequest
	}
	values := make(map[string]string, len(query))
	for _, key := range required {
		entries, exists := query[key]
		if !exists || len(entries) != 1 || entries[0] == "" {
			return nil, runtimecore.ErrInvalidRequest
		}
		values[key] = entries[0]
	}
	return values, nil
}

func parseInt(raw string, minimum int64) (int64, error) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < minimum || strconv.FormatInt(value, 10) != raw {
		return 0, fmt.Errorf("invalid integer")
	}
	return value, nil
}

func writeJSON(writer http.ResponseWriter, value any) error {
	return writeJSONStatus(writer, http.StatusOK, value)
}

func writeJSONStatus(writer http.ResponseWriter, status int, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return runtimecore.ErrRuntimeStateUnavailable
	}
	if len(body) > MaxResponseBytes {
		return runtimecore.ErrResultTooLarge
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set(contract.ProtocolHeader, contract.ProtocolVersion)
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
	return nil
}

func writeError(writer http.ResponseWriter, err error) {
	code := "runtime_state_unavailable"
	var runtimeError *runtimecore.RuntimeError
	if errors.As(err, &runtimeError) {
		code = runtimeError.Code
	}
	status := statusFor(code)
	body, _ := json.Marshal(contract.ErrorResponse{ProtocolVersion: contract.ProtocolVersion, Code: code})
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set(contract.ProtocolHeader, contract.ProtocolVersion)
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}

func writeHealth(writer http.ResponseWriter, status int) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(status)
	if status == http.StatusOK {
		_, _ = io.WriteString(writer, "ok\n")
	} else {
		_, _ = io.WriteString(writer, "not ready\n")
	}
}

func statusFor(code string) int {
	switch code {
	case "invalid_request":
		return http.StatusBadRequest
	case "runtime_authentication_failed":
		return http.StatusUnauthorized
	case "fence_rejected", "operation_conflict", "protocol_incompatible", "capability_unavailable":
		return http.StatusConflict
	case "operation_not_found":
		return http.StatusNotFound
	case "result_too_large":
		return http.StatusRequestEntityTooLarge
	default:
		return http.StatusServiceUnavailable
	}
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanValue(decoder); err != nil {
		return err
	}
	if !jsonEOF(decoder) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func scanValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("duplicate JSON object key")
			}
			seen[key] = struct{}{}
			if err := scanValue(decoder); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := scanValue(decoder); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	default:
		return errors.New("invalid JSON delimiter")
	}
}

func jsonEOF(decoder *json.Decoder) bool {
	var extra any
	return errors.Is(decoder.Decode(&extra), io.EOF)
}

func validCredential(credential string) bool {
	// runtimeidentity performs the canonical 256-bit validation while deriving
	// TLS keys. Keep this package independent of identity derivation details.
	decoded, err := base64.RawURLEncoding.DecodeString(credential)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == credential
}

func validTaskID(value string) bool      { return validID(value, 'q') }
func validOperationID(value string) bool { return validID(value, 'o') }

func validID(value string, prefix byte) bool {
	if len(value) != 8 || value[0] != prefix {
		return false
	}
	for index := 1; index < len(value); index++ {
		if !(value[index] >= 'a' && value[index] <= 'z') && !(value[index] >= '0' && value[index] <= '9') {
			return false
		}
	}
	return true
}
