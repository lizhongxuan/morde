package runtimeclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"morde-runtime/contract"
	"morde/server/internal/domain/workspace"
)

const (
	requestTimeout       = 60 * time.Second
	maxRequestBodyBytes  = 1 << 20
	maxResponseBodyBytes = 8 << 20
)

// HTTPDoer is the only transport dependency held by Client. Callers may wrap
// it for transport policy; credentials are added by Client and never logged.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type Client struct {
	baseURL    string
	credential string
	http       HTTPDoer
}

func New(baseURL, runtimeCredential string, httpClient HTTPDoer) (*Client, error) {
	if httpClient == nil {
		return nil, errors.New("runtime HTTP client is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed == nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("runtime base URL must be an HTTPS URL without credentials or query")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(runtimeCredential)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != runtimeCredential {
		return nil, errors.New("runtime credential must encode 256 bits")
	}
	return &Client{
		baseURL:    strings.TrimRight(parsed.String(), "/"),
		credential: runtimeCredential,
		http:       httpClient,
	}, nil
}

func (c *Client) Capabilities(ctx context.Context, request CapabilitiesRequest) (CapabilitiesResponse, error) {
	if err := validateHandshake(request); err != nil {
		return CapabilitiesResponse{}, err
	}
	if request.ProtocolVersion != "" && request.ProtocolVersion != ProtocolVersion {
		return CapabilitiesResponse{}, incompatibleProtocol()
	}
	request.ProtocolVersion = ProtocolVersion
	query := url.Values{
		"protocol_version":   {request.ProtocolVersion},
		"host_id":            {request.HostID},
		"runtime_generation": {fmt.Sprint(request.RuntimeGeneration)},
		"control_version":    {fmt.Sprint(request.ControlVersion)},
	}
	var response CapabilitiesResponse
	if err := c.doJSON(ctx, http.MethodGet, "/runtime/v1/capabilities", query, nil, &response); err != nil {
		return CapabilitiesResponse{}, err
	}
	// The handshake reports the runtime's actual persisted control fence. The
	// caller reconciles a behind runtime to the current Host version and fails
	// closed if the runtime is ahead; rejecting a mismatch here would prevent
	// safe recovery after a process restart.
	if response.HostID != request.HostID || response.RuntimeGeneration != request.RuntimeGeneration || response.ControlVersion < 0 || response.RuntimeInstanceID == "" || !validMode(response.Mode) || response.Capabilities == nil {
		return CapabilitiesResponse{}, invalidResponse()
	}
	return response, nil
}

func (c *Client) Applications(ctx context.Context, fence HostFence) (ApplicationsResponse, error) {
	if validateHostFence(fence) != nil {
		return ApplicationsResponse{}, invalidRequest()
	}
	query := url.Values{
		"protocol_version":   {ProtocolVersion},
		"host_id":            {fence.HostID},
		"runtime_generation": {fmt.Sprint(fence.RuntimeGeneration)},
		"control_version":    {fmt.Sprint(fence.ControlVersion)},
	}
	var wire json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, "/runtime/v1/applications", query, nil, &wire); err != nil {
		return ApplicationsResponse{}, err
	}
	if err := rejectDuplicateJSONKeys(wire); err != nil {
		return ApplicationsResponse{}, invalidResponse()
	}
	var response ApplicationsResponse
	decoder := json.NewDecoder(bytes.NewReader(wire))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil || decoder.Decode(new(any)) != io.EOF {
		return ApplicationsResponse{}, invalidResponse()
	}
	if response.HostID != fence.HostID || response.RuntimeGeneration != fence.RuntimeGeneration ||
		response.RuntimeInstanceID != fence.RuntimeInstanceID || response.ControlVersion != fence.ControlVersion ||
		!isUTC(response.CheckedAt) || !validApplicationCatalog(response.Items) {
		return ApplicationsResponse{}, invalidResponse()
	}
	return response, nil
}

func (c *Client) InstallApplication(ctx context.Context, appID string, request ApplicationInstallRequest) (ApplicationInstallResponse, error) {
	if !validHostApplicationID(appID) || validateHostFence(request.HostFence) != nil {
		return ApplicationInstallResponse{}, invalidRequest()
	}
	if request.ProtocolVersion != "" && request.ProtocolVersion != ProtocolVersion {
		return ApplicationInstallResponse{}, incompatibleProtocol()
	}
	request.ProtocolVersion = ProtocolVersion
	if !contract.ValidApplicationInstallRequest(request) {
		return ApplicationInstallResponse{}, invalidRequest()
	}
	var wire json.RawMessage
	path := "/runtime/v1/applications/" + url.PathEscape(appID) + "/install"
	if err := c.doJSON(ctx, http.MethodPost, path, nil, request, &wire); err != nil {
		return ApplicationInstallResponse{}, err
	}
	if err := rejectDuplicateJSONKeys(wire); err != nil {
		return ApplicationInstallResponse{}, invalidResponse()
	}
	var response ApplicationInstallResponse
	decoder := json.NewDecoder(bytes.NewReader(wire))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil || decoder.Decode(new(any)) != io.EOF {
		return ApplicationInstallResponse{}, invalidResponse()
	}
	if !contract.ValidApplicationInstallResponse(response) || response.HostFence != request.HostFence ||
		response.Operation.ApplicationID != appID || !isUTC(response.Operation.CreatedAt) || !isUTC(response.Operation.UpdatedAt) {
		return ApplicationInstallResponse{}, invalidResponse()
	}
	return response, nil
}

func (c *Client) LaunchApplication(ctx context.Context, appID string, fence HostFence, launchID string) (ApplicationLaunchOperation, error) {
	if !validHostApplicationID(appID) {
		return ApplicationLaunchOperation{}, invalidRequest()
	}
	request := ApplicationLaunchRequest{ProtocolVersion: ProtocolVersion, HostFence: fence, LaunchID: launchID}
	if !contract.ValidApplicationLaunchRequest(request) {
		return ApplicationLaunchOperation{}, invalidRequest()
	}
	var wire json.RawMessage
	path := "/runtime/v1/applications/" + url.PathEscape(appID) + "/launch"
	if err := c.doJSONStatus(ctx, http.MethodPost, path, nil, request, &wire, http.StatusAccepted); err != nil {
		return ApplicationLaunchOperation{}, err
	}
	response, err := decodeApplicationLaunchResponse(wire, fence, launchID)
	if err != nil {
		return ApplicationLaunchOperation{}, err
	}
	return response.Operation, nil
}

func (c *Client) GetApplicationLaunch(ctx context.Context, appID string, fence HostFence, launchID string) (ApplicationLaunchOperation, error) {
	if !validHostApplicationID(appID) {
		return ApplicationLaunchOperation{}, invalidRequest()
	}
	request := ApplicationLaunchRequest{ProtocolVersion: ProtocolVersion, HostFence: fence, LaunchID: launchID}
	if !contract.ValidApplicationLaunchRequest(request) {
		return ApplicationLaunchOperation{}, invalidRequest()
	}
	query := url.Values{
		"protocol_version":    {ProtocolVersion},
		"host_id":             {fence.HostID},
		"runtime_generation":  {fmt.Sprint(fence.RuntimeGeneration)},
		"runtime_instance_id": {fence.RuntimeInstanceID},
		"control_version":     {fmt.Sprint(fence.ControlVersion)},
	}
	var wire json.RawMessage
	path := "/runtime/v1/applications/" + url.PathEscape(appID) + "/launches/" + url.PathEscape(launchID)
	if err := c.doJSON(ctx, http.MethodGet, path, query, nil, &wire); err != nil {
		return ApplicationLaunchOperation{}, err
	}
	response, err := decodeApplicationLaunchResponse(wire, fence, launchID)
	if err != nil {
		return ApplicationLaunchOperation{}, err
	}
	return response.Operation, nil
}

func (c *Client) StopApplication(ctx context.Context, appID string, fence HostFence, launchID string) (ApplicationLaunchOperation, error) {
	if !validHostApplicationID(appID) {
		return ApplicationLaunchOperation{}, invalidRequest()
	}
	request := ApplicationLaunchRequest{ProtocolVersion: ProtocolVersion, HostFence: fence, LaunchID: launchID}
	if !contract.ValidApplicationLaunchRequest(request) {
		return ApplicationLaunchOperation{}, invalidRequest()
	}
	var wire json.RawMessage
	path := "/runtime/v1/applications/" + url.PathEscape(appID) + "/launches/" + url.PathEscape(launchID) + "/stop"
	if err := c.doJSONStatus(ctx, http.MethodPost, path, nil, request, &wire, http.StatusAccepted); err != nil {
		return ApplicationLaunchOperation{}, err
	}
	response, err := decodeApplicationLaunchResponse(wire, fence, launchID)
	if err != nil {
		return ApplicationLaunchOperation{}, err
	}
	return response.Operation, nil
}

func validHostApplicationID(appID string) bool {
	return appID == "wechat"
}

func decodeApplicationLaunchResponse(wire []byte, fence HostFence, launchID string) (ApplicationLaunchResponse, error) {
	if err := rejectDuplicateJSONKeys(wire); err != nil {
		return ApplicationLaunchResponse{}, invalidResponse()
	}
	var response ApplicationLaunchResponse
	decoder := json.NewDecoder(bytes.NewReader(wire))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil || decoder.Decode(new(any)) != io.EOF {
		return ApplicationLaunchResponse{}, invalidResponse()
	}
	if !contract.ValidApplicationLaunchResponse(response) || response.HostFence != fence || response.Operation.ID != launchID ||
		!ValidApplicationLaunchOperation(response.Operation) {
		return ApplicationLaunchResponse{}, invalidResponse()
	}
	return response, nil
}

func validApplicationCatalog(items []ApplicationStatusItem) bool {
	if len(items) != 2 {
		return false
	}
	seen := map[string]bool{}
	for _, item := range items {
		if !ValidApplicationStatusItem(item) || seen[item.ID] {
			return false
		}
		seen[item.ID] = true
	}
	return seen["codex"] && seen["wechat"]
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON data")
		}
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
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
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
		return nil
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
		return nil
	default:
		return errors.New("invalid JSON delimiter")
	}
}

func (c *Client) SetControl(ctx context.Context, request SetControlRequest) (SetControlResponse, error) {
	if err := validateHostFence(request.HostFence); err != nil || !validMode(request.Mode) {
		return SetControlResponse{}, invalidRequest()
	}
	if request.ProtocolVersion != "" && request.ProtocolVersion != ProtocolVersion {
		return SetControlResponse{}, incompatibleProtocol()
	}
	request.ProtocolVersion = ProtocolVersion
	if request.Mode == ModeHuman {
		if !receiptMatchesFence(request.DrainedReceipt, request.HostFence) || request.DrainedReceipt.TargetControlVersion != request.ControlVersion {
			return SetControlResponse{}, invalidRequest()
		}
	} else if request.DrainedReceipt != nil {
		return SetControlResponse{}, invalidRequest()
	}
	var response SetControlResponse
	if err := c.doJSON(ctx, http.MethodPut, "/runtime/v1/control", nil, request, &response); err != nil {
		return SetControlResponse{}, err
	}
	if response.HostFence != request.HostFence || response.Mode != request.Mode {
		return SetControlResponse{}, invalidResponse()
	}
	return response, nil
}

func (c *Client) SetTaskLease(ctx context.Context, taskID string, request SetTaskLeaseRequest) (SetTaskLeaseResponse, error) {
	if !validTaskID(taskID) || validateHostFence(request.HostFence) != nil || request.LeaseVersion < 1 || !isUTC(request.ExpiresAt) {
		return SetTaskLeaseResponse{}, invalidRequest()
	}
	if request.ProtocolVersion != "" && request.ProtocolVersion != ProtocolVersion {
		return SetTaskLeaseResponse{}, incompatibleProtocol()
	}
	request.ProtocolVersion = ProtocolVersion
	var response SetTaskLeaseResponse
	path := "/runtime/v1/tasks/" + url.PathEscape(taskID) + "/lease"
	if err := c.doJSON(ctx, http.MethodPut, path, nil, request, &response); err != nil {
		return SetTaskLeaseResponse{}, err
	}
	if response.HostFence != request.HostFence || response.TaskID != taskID || response.LeaseVersion != request.LeaseVersion || !response.ExpiresAt.Equal(request.ExpiresAt) {
		return SetTaskLeaseResponse{}, invalidResponse()
	}
	return response, nil
}

// SetTaskWorkspaceView asks the Runtime to bind a Task to its immutable private
// view of the submitted Workspace checkpoint. The Runtime chooses the path.
func (c *Client) SetTaskWorkspaceView(ctx context.Context, taskID string, request SetTaskWorkspaceViewRequest) (SetTaskWorkspaceViewResponse, error) {
	if !validTaskID(taskID) || validateHostFence(request.HostFence) != nil || request.TaskID != taskID ||
		!validWorkspaceID(request.WorkspaceID) || request.BindingVersion < 1 || request.InputRevision < 0 ||
		request.WorkspaceCheckpointRevision < 0 || !validSHA256(request.ManifestSHA256) {
		return SetTaskWorkspaceViewResponse{}, invalidRequest()
	}
	if request.ProtocolVersion != "" && request.ProtocolVersion != ProtocolVersion {
		return SetTaskWorkspaceViewResponse{}, incompatibleProtocol()
	}
	request.ProtocolVersion = ProtocolVersion
	var response SetTaskWorkspaceViewResponse
	path := "/runtime/v1/tasks/" + url.PathEscape(taskID) + "/workspace-view"
	if err := c.doJSON(ctx, http.MethodPut, path, nil, request, &response); err != nil {
		return SetTaskWorkspaceViewResponse{}, err
	}
	if response.ProtocolVersion != ProtocolVersion || response.HostFence != request.HostFence || response.TaskID != taskID ||
		response.WorkspaceID != request.WorkspaceID || response.BindingVersion != request.BindingVersion ||
		response.InputRevision != request.InputRevision || response.WorkspaceCheckpointRevision != request.WorkspaceCheckpointRevision ||
		response.ManifestSHA256 != request.ManifestSHA256 || !validTaskWorkspaceViewID(response.ViewID) {
		return SetTaskWorkspaceViewResponse{}, invalidResponse()
	}
	return response, nil
}

func (c *Client) TaskWorkspaceManifestPage(ctx context.Context, taskID string, request TaskWorkspaceManifestRequest) (TaskWorkspaceManifestResponse, error) {
	if !validTaskID(taskID) || validateTaskWorkspaceManifestRequest(taskID, request) != nil {
		return TaskWorkspaceManifestResponse{}, invalidRequest()
	}
	if request.ProtocolVersion != "" && request.ProtocolVersion != ProtocolVersion {
		return TaskWorkspaceManifestResponse{}, incompatibleProtocol()
	}
	request.ProtocolVersion = ProtocolVersion
	var response TaskWorkspaceManifestResponse
	path := "/runtime/v1/tasks/" + url.PathEscape(taskID) + "/workspace-view/manifest"
	if err := c.doJSON(ctx, http.MethodPost, path, nil, request, &response); err != nil {
		return TaskWorkspaceManifestResponse{}, err
	}
	if !taskWorkspaceManifestResponseMatches(response, taskID, request) {
		return TaskWorkspaceManifestResponse{}, invalidResponse()
	}
	return response, nil
}

func (c *Client) ReadTaskWorkspaceBlock(ctx context.Context, taskID string, request TaskWorkspaceBlockRequest) (TaskWorkspaceBlockResponse, error) {
	if !validTaskID(taskID) || validateTaskWorkspaceBlockRequest(taskID, request) != nil {
		return TaskWorkspaceBlockResponse{}, invalidRequest()
	}
	if request.ProtocolVersion != "" && request.ProtocolVersion != ProtocolVersion {
		return TaskWorkspaceBlockResponse{}, incompatibleProtocol()
	}
	request.ProtocolVersion = ProtocolVersion
	var response TaskWorkspaceBlockResponse
	path := "/runtime/v1/tasks/" + url.PathEscape(taskID) + "/workspace-view/block"
	if err := c.doJSON(ctx, http.MethodPost, path, nil, request, &response); err != nil {
		return TaskWorkspaceBlockResponse{}, err
	}
	if response.ProtocolVersion != ProtocolVersion || response.HostFence != request.HostFence || response.TaskID != taskID ||
		response.WorkspaceID != request.WorkspaceID || response.BindingVersion != request.BindingVersion ||
		response.InputRevision != request.InputRevision || response.WorkspaceCheckpointRevision != request.WorkspaceCheckpointRevision ||
		response.InputManifestSHA256 != request.InputManifestSHA256 || response.ViewID != request.ViewID ||
		response.ManifestSHA256 != request.ManifestSHA256 || response.SHA256 != request.SHA256 ||
		response.SizeBytes < 1 || response.SizeBytes > 4<<20 || int64(len(response.Data)) != response.SizeBytes || !validBlockSHA256(response.Data, request.SHA256) {
		return TaskWorkspaceBlockResponse{}, invalidResponse()
	}
	return response, nil
}

func validateTaskWorkspaceManifestRequest(taskID string, request TaskWorkspaceManifestRequest) error {
	if validateHostFence(request.HostFence) != nil || request.TaskID != taskID || !validWorkspaceID(request.WorkspaceID) ||
		request.BindingVersion < 1 || request.InputRevision < 0 || request.WorkspaceCheckpointRevision < request.InputRevision ||
		!validSHA256(request.InputManifestSHA256) || !validTaskWorkspaceViewID(request.ViewID) || request.Limit < 1 || request.Limit > 1000 || len(request.AfterPath) > 4096 ||
		request.ManifestSHA256 != "" && !validSHA256(request.ManifestSHA256) || request.ManifestSHA256 == "" && request.AfterPath != "" {
		return invalidRequest()
	}
	return nil
}

func validateTaskWorkspaceBlockRequest(taskID string, request TaskWorkspaceBlockRequest) error {
	if validateHostFence(request.HostFence) != nil || request.TaskID != taskID || !validWorkspaceID(request.WorkspaceID) ||
		request.BindingVersion < 1 || request.InputRevision < 0 || request.WorkspaceCheckpointRevision < request.InputRevision ||
		!validSHA256(request.InputManifestSHA256) || !validTaskWorkspaceViewID(request.ViewID) ||
		!validSHA256(request.ManifestSHA256) || !validSHA256(request.SHA256) {
		return invalidRequest()
	}
	return nil
}

func taskWorkspaceManifestResponseMatches(response TaskWorkspaceManifestResponse, taskID string, request TaskWorkspaceManifestRequest) bool {
	if response.ProtocolVersion != ProtocolVersion || response.HostFence != request.HostFence || response.TaskID != taskID ||
		response.WorkspaceID != request.WorkspaceID || response.BindingVersion != request.BindingVersion ||
		response.InputRevision != request.InputRevision || response.WorkspaceCheckpointRevision != request.WorkspaceCheckpointRevision ||
		response.InputManifestSHA256 != request.InputManifestSHA256 || !validTaskWorkspaceViewID(response.ViewID) || response.ViewID != request.ViewID ||
		!validSHA256(response.ManifestSHA256) || response.Files == nil || len(response.Files) > request.Limit || len(response.NextCursor) > 4096 ||
		response.NextCursor != "" && len(response.Files) == 0 {
		return false
	}
	previous := request.AfterPath
	for _, file := range response.Files {
		chunks := make([]workspace.Chunk, len(file.Chunks))
		for index, chunk := range file.Chunks {
			chunks[index] = workspace.Chunk{SHA256: chunk.SHA256, SizeBytes: chunk.SizeBytes}
		}
		if file.Path <= previous || workspace.ValidateFile(workspace.File{
			Path: file.Path, SizeBytes: file.SizeBytes, SHA256: file.SHA256, Mode: file.Mode, Chunks: chunks,
		}) != nil {
			return false
		}
		previous = file.Path
	}
	return response.NextCursor == "" || len(response.Files) > 0 && response.NextCursor == response.Files[len(response.Files)-1].Path
}

func validBlockSHA256(data []byte, expected string) bool {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]) == expected
}

// SetWorkspaceBindings atomically replaces the Runtime's active Workspace
// bindings from the current HostStore snapshot. The response must echo the
// exact fenced snapshot before an operation can be dispatched.
func (c *Client) SetWorkspaceBindings(ctx context.Context, request SetWorkspaceBindingsRequest) (SetWorkspaceBindingsResponse, error) {
	if validateHostFence(request.HostFence) != nil || !validWorkspaceBindings(request.Bindings) {
		return SetWorkspaceBindingsResponse{}, invalidRequest()
	}
	if request.ProtocolVersion != "" && request.ProtocolVersion != ProtocolVersion {
		return SetWorkspaceBindingsResponse{}, incompatibleProtocol()
	}
	request.ProtocolVersion = ProtocolVersion
	var response SetWorkspaceBindingsResponse
	if err := c.doJSON(ctx, http.MethodPut, "/runtime/v1/workspace-bindings", nil, request, &response); err != nil {
		return SetWorkspaceBindingsResponse{}, err
	}
	if response.HostFence != request.HostFence || !sameWorkspaceBindings(response.Bindings, request.Bindings) {
		return SetWorkspaceBindingsResponse{}, invalidResponse()
	}
	return response, nil
}

func (c *Client) Execute(ctx context.Context, request ExecuteRequest) (Operation, error) {
	if err := validateExecute(request); err != nil {
		return Operation{}, err
	}
	if request.ProtocolVersion != "" && request.ProtocolVersion != ProtocolVersion {
		return Operation{}, incompatibleProtocol()
	}
	request.ProtocolVersion = ProtocolVersion
	var response OperationResponse
	if err := c.doJSON(ctx, http.MethodPost, "/runtime/v1/operations", nil, request, &response); err != nil {
		return Operation{}, err
	}
	if response.RequestFence != request.HostFence || !operationMatchesExecute(response.Operation, request) || !validOperationStatus(response.Operation.Status) {
		return Operation{}, invalidResponse()
	}
	return response.Operation, nil
}

func (c *Client) GetOperation(ctx context.Context, operationID string, query OperationQuery) (Operation, error) {
	if !validOperationID(operationID) || validateHostFence(query.HostFence) != nil || !validTaskID(query.TaskID) || query.LeaseVersion < 1 {
		return Operation{}, invalidRequest()
	}
	params := url.Values{
		"host_id":             {query.HostID},
		"runtime_generation":  {fmt.Sprint(query.RuntimeGeneration)},
		"runtime_instance_id": {query.RuntimeInstanceID},
		"control_version":     {fmt.Sprint(query.ControlVersion)},
		"task_id":             {query.TaskID},
		"lease_version":       {fmt.Sprint(query.LeaseVersion)},
	}
	var response OperationResponse
	path := "/runtime/v1/operations/" + url.PathEscape(operationID)
	if err := c.doJSON(ctx, http.MethodGet, path, params, nil, &response); err != nil {
		return Operation{}, err
	}
	if response.RequestFence != query.HostFence || !operationMatchesQuery(response.Operation, operationID, query) || !validOperationStatus(response.Operation.Status) {
		return Operation{}, invalidResponse()
	}
	return response.Operation, nil
}

func (c *Client) CancelOperation(ctx context.Context, operationID string, request CancelOperationRequest) (Operation, error) {
	if !validOperationID(operationID) || validateHostFence(request.HostFence) != nil {
		return Operation{}, invalidRequest()
	}
	if request.ProtocolVersion != "" && request.ProtocolVersion != ProtocolVersion {
		return Operation{}, incompatibleProtocol()
	}
	request.ProtocolVersion = ProtocolVersion
	var response OperationResponse
	path := "/runtime/v1/operations/" + url.PathEscape(operationID) + "/cancel"
	if err := c.doJSON(ctx, http.MethodPost, path, nil, request, &response); err != nil {
		return Operation{}, err
	}
	if response.RequestFence != request.HostFence || response.Operation.OperationID != operationID || response.Operation.HostID != request.HostID || !validOperation(response.Operation) || !validOperationStatus(response.Operation.Status) {
		return Operation{}, invalidResponse()
	}
	return response.Operation, nil
}

func (c *Client) Quiesce(ctx context.Context, request QuiesceRequest) (QuiesceResponse, error) {
	if validateHostFence(request.HostFence) != nil || request.TargetControlVersion <= request.ControlVersion || request.TaskIDs == nil || !validTaskIDs(request.TaskIDs) {
		return QuiesceResponse{}, invalidRequest()
	}
	if request.ProtocolVersion != "" && request.ProtocolVersion != ProtocolVersion {
		return QuiesceResponse{}, incompatibleProtocol()
	}
	request.ProtocolVersion = ProtocolVersion
	var response QuiesceResponse
	if err := c.doJSON(ctx, http.MethodPost, "/runtime/v1/quiesce", nil, request, &response); err != nil {
		return QuiesceResponse{}, err
	}
	if response.PendingOperationIDs == nil || !validQuiesceStatus(response.Status) {
		return QuiesceResponse{}, invalidResponse()
	}
	switch response.Status {
	case QuiesceDrained:
		if len(response.PendingOperationIDs) != 0 || !receiptMatchesQuiesce(response.DrainedReceipt, request) {
			return QuiesceResponse{}, invalidResponse()
		}
	case QuiesceDraining, QuiesceFailed:
		if response.DrainedReceipt != nil {
			return QuiesceResponse{}, invalidResponse()
		}
	}
	return response, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, requestBody, responseBody any) error {
	return c.doJSONStatus(ctx, method, path, query, requestBody, responseBody, http.StatusOK)
}

func (c *Client) doJSONStatus(ctx context.Context, method, path string, query url.Values, requestBody, responseBody any, successStatus int) error {
	if ctx == nil {
		return invalidRequest()
	}
	var body []byte
	if requestBody != nil {
		var err error
		body, err = json.Marshal(requestBody)
		if err != nil || len(body) > maxRequestBodyBytes {
			return invalidRequest()
		}
	}
	requestURL := c.baseURL + path
	if len(query) > 0 {
		requestURL += "?" + query.Encode()
	}
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, method, requestURL, bytes.NewReader(body))
	if err != nil {
		return invalidRequest()
	}
	request.Header.Set(ProtocolHeader, ProtocolVersion)
	request.Header.Set("Authorization", "Bearer "+c.credential)
	request.Header.Set("Accept", "application/json")
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		if errors.Is(requestCtx.Err(), context.Canceled) {
			return requestCtx.Err()
		}
		if errors.Is(requestCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			return &Error{Code: ErrorTransportTimeout, StatusCode: http.StatusGatewayTimeout}
		}
		return &Error{Code: ErrorTransport, StatusCode: http.StatusBadGateway}
	}
	defer func() { _ = response.Body.Close() }()
	data, err := readBounded(response.Body, maxResponseBodyBytes)
	if err != nil {
		if errors.Is(err, errResponseTooLarge) {
			return &Error{Code: ErrorResponseTooLarge, StatusCode: response.StatusCode}
		}
		return invalidResponse()
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return parseRuntimeError(response.StatusCode, data)
	}
	if response.StatusCode != successStatus {
		return invalidResponse()
	}
	var envelope struct {
		ProtocolVersion string `json:"protocol_version"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.ProtocolVersion != ProtocolVersion {
		return incompatibleProtocol()
	}
	if err := json.Unmarshal(data, responseBody); err != nil {
		return invalidResponse()
	}
	return nil
}

var errResponseTooLarge = errors.New("response too large")

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errResponseTooLarge
	}
	return data, nil
}

func parseRuntimeError(status int, body []byte) error {
	var response contract.ErrorResponse
	if err := json.Unmarshal(body, &response); err != nil || response.ProtocolVersion != ProtocolVersion {
		return incompatibleProtocol()
	}
	expectedStatus, ok := statusForCode(response.Code)
	if !ok || expectedStatus != status {
		return invalidResponse()
	}
	return &Error{Code: response.Code, StatusCode: status}
}

func statusForCode(code string) (int, bool) {
	switch code {
	case ErrorInvalidRequest:
		return http.StatusBadRequest, true
	case ErrorAuthenticationFailed:
		return http.StatusUnauthorized, true
	case ErrorFenceRejected, ErrorOperationConflict, ErrorProtocolIncompatible, ErrorCapabilityUnavailable:
		return http.StatusConflict, true
	case ErrorOperationNotFound:
		return http.StatusNotFound, true
	case ErrorResultTooLarge:
		return http.StatusRequestEntityTooLarge, true
	case ErrorRuntimeStateUnavailable:
		return http.StatusServiceUnavailable, true
	default:
		return 0, false
	}
}

func validateHandshake(request CapabilitiesRequest) error {
	if !validHostID(request.HostID) || request.RuntimeGeneration < 1 || request.ControlVersion < 0 {
		return invalidRequest()
	}
	return nil
}

func validateHostFence(fence HostFence) error {
	if !validHostID(fence.HostID) || fence.RuntimeGeneration < 1 || fence.RuntimeInstanceID == "" || fence.ControlVersion < 0 {
		return invalidRequest()
	}
	return nil
}

func validateExecute(request ExecuteRequest) error {
	if validateHostFence(request.HostFence) != nil || !validTaskID(request.TaskID) || !validOperationID(request.OperationID) || request.LeaseVersion < 1 || request.Capability == "" || request.Action == "" || len(request.Arguments) == 0 || !json.Valid(request.Arguments) || !isJSONObject(request.Arguments) {
		return invalidRequest()
	}
	if contract.WorkspaceScopedCapability(request.Capability) {
		if !validWorkspaceID(request.WorkspaceID) || request.BindingVersion < 1 || !validTaskWorkspaceViewID(request.WorkspaceViewID) || request.WorkspaceCheckpointRevision < 0 {
			return invalidRequest()
		}
	} else if request.WorkspaceID != "" || request.BindingVersion != 0 || request.WorkspaceViewID != "" || request.WorkspaceCheckpointRevision != 0 {
		return invalidRequest()
	}
	return nil
}

func isJSONObject(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

func operationMatchesExecute(operation Operation, request ExecuteRequest) bool {
	return validOperation(operation) && operation.OperationID == request.OperationID && operation.TaskID == request.TaskID && operation.LeaseVersion == request.LeaseVersion && operation.HostID == request.HostID &&
		operation.WorkspaceID == request.WorkspaceID && operation.BindingVersion == request.BindingVersion &&
		operation.WorkspaceViewID == request.WorkspaceViewID && operation.WorkspaceCheckpointRevision == request.WorkspaceCheckpointRevision
}

func validWorkspaceID(value string) bool { return validID(value, 'w') }

func validTaskWorkspaceViewID(value string) bool {
	if len(value) != 65 || value[0] != 'v' {
		return false
	}
	decoded, err := hex.DecodeString(value[1:])
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == value[1:]
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func validWorkspaceBindings(bindings []WorkspaceBinding) bool {
	if bindings == nil || len(bindings) > 256 {
		return false
	}
	ids := make(map[string]struct{}, len(bindings))
	directories := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		if !validWorkspaceID(binding.WorkspaceID) || binding.BindingVersion < 1 || binding.Directory != binding.WorkspaceID {
			return false
		}
		if _, exists := ids[binding.WorkspaceID]; exists {
			return false
		}
		if _, exists := directories[binding.Directory]; exists {
			return false
		}
		ids[binding.WorkspaceID] = struct{}{}
		directories[binding.Directory] = struct{}{}
	}
	return true
}

func sameWorkspaceBindings(left, right []WorkspaceBinding) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func operationMatchesQuery(operation Operation, operationID string, query OperationQuery) bool {
	return validOperation(operation) && operation.OperationID == operationID && operation.TaskID == query.TaskID && operation.LeaseVersion == query.LeaseVersion && operation.HostID == query.HostID
}

func validOperation(operation Operation) bool {
	if !validHostID(operation.HostID) || operation.RuntimeGeneration < 1 || operation.RuntimeInstanceID == "" || operation.ControlVersion < 0 ||
		!validTaskID(operation.TaskID) || !validOperationID(operation.OperationID) || operation.LeaseVersion < 1 {
		return false
	}
	if operation.WorkspaceID == "" {
		return operation.BindingVersion == 0 && operation.WorkspaceViewID == "" && operation.WorkspaceCheckpointRevision == 0
	}
	return validWorkspaceID(operation.WorkspaceID) && operation.BindingVersion >= 1 &&
		validTaskWorkspaceViewID(operation.WorkspaceViewID) && operation.WorkspaceCheckpointRevision >= 0
}

func receiptMatchesFence(receipt *DrainedReceipt, fence HostFence) bool {
	return receipt != nil && receipt.ReceiptID != "" && receipt.HostID == fence.HostID && receipt.RuntimeGeneration == fence.RuntimeGeneration && receipt.RuntimeInstanceID == fence.RuntimeInstanceID && isUTC(receipt.IssuedAt)
}

func isUTC(value time.Time) bool {
	if value.IsZero() {
		return false
	}
	_, offset := value.Zone()
	return offset == 0
}

func receiptMatchesQuiesce(receipt *DrainedReceipt, request QuiesceRequest) bool {
	return receiptMatchesFence(receipt, request.HostFence) && receipt.BlockedControlVersion == request.ControlVersion && receipt.TargetControlVersion == request.TargetControlVersion && sameStringSet(receipt.TaskIDs, request.TaskIDs)
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[string]int, len(left))
	for _, value := range left {
		seen[value]++
	}
	for _, value := range right {
		if seen[value] == 0 {
			return false
		}
		seen[value]--
	}
	return true
}

func validMode(mode Mode) bool {
	return mode == ModeBlocked || mode == ModeAI || mode == ModeHuman
}

func validOperationStatus(status OperationStatus) bool {
	switch status {
	case OperationAccepted, OperationRunning, OperationSucceeded, OperationFailed, OperationCancelling, OperationCancelled, OperationUnknown:
		return true
	default:
		return false
	}
}

func validQuiesceStatus(status QuiesceStatus) bool {
	return status == QuiesceDraining || status == QuiesceDrained || status == QuiesceFailed
}

func validHostID(value string) bool      { return validID(value, 'h') }
func validTaskID(value string) bool      { return validID(value, 'q') }
func validOperationID(value string) bool { return validID(value, 'o') }

func validID(value string, prefix byte) bool {
	if len(value) != 8 || value[0] != prefix {
		return false
	}
	for i := 1; i < len(value); i++ {
		if (value[i] < 'a' || value[i] > 'z') && (value[i] < '0' || value[i] > '9') {
			return false
		}
	}
	return true
}

func validTaskIDs(ids []string) bool {
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if !validTaskID(id) {
			return false
		}
		if _, ok := seen[id]; ok {
			return false
		}
		seen[id] = struct{}{}
	}
	return true
}

func invalidRequest() error {
	return &Error{Code: ErrorInvalidRequest, StatusCode: http.StatusBadRequest}
}

func invalidResponse() error {
	return &Error{Code: ErrorInvalidResponse, StatusCode: http.StatusBadGateway}
}

func incompatibleProtocol() error {
	return &Error{Code: ErrorProtocolIncompatible, StatusCode: http.StatusConflict}
}
