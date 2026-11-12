package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"morde-sync/client"
)

const (
	maxRequestLine       = 1 << 20
	maxScopeEntries      = 1000
	cancelTimeout        = 15 * time.Second
	localRecoveryTimeout = 30 * time.Second
)

var (
	workspaceIDRE     = regexp.MustCompile(`^w[a-z0-9]{7}$`)
	syncFolderIDRE    = regexp.MustCompile(`^morde-[a-f0-9]{32}$`)
	sourceBindingIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	sessionIDRE       = regexp.MustCompile(`^p[a-z0-9]{7}$`)
	taskIDRE          = regexp.MustCompile(`^q[a-z0-9]{7}$`)
	taskOperationRE   = regexp.MustCompile(`^agent:[a-f0-9]{64}$`)
	idempotencyRE     = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)
	preservedIDRE     = regexp.MustCompile(`^[a-f0-9]{32}$`)
	folderIDRE        = regexp.MustCompile(`^morde-[a-f0-9]{32}$`)
)

type stdioServer struct {
	in     io.Reader
	out    io.Writer
	errOut io.Writer
	ctx    context.Context

	outMu  sync.Mutex
	mu     sync.Mutex
	active *activeSync
	wg     sync.WaitGroup
}

type activeSync struct {
	id                  json.RawMessage
	idKey               string
	client              *client.Client
	workspace           string
	directory           string
	live                bool
	allowTerminalCancel bool
	cancel              context.CancelFunc
	done                chan struct{}

	mu              sync.Mutex
	result          client.Result
	cancelRequested bool
	liveIdle        bool
	sessionActive   bool
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type progressNotification struct {
	JSONRPC string         `json:"jsonrpc"`
	Method  string         `json:"method"`
	Params  progressParams `json:"params"`
}

type progressParams struct {
	RequestID         json.RawMessage `json:"request_id"`
	SessionID         string          `json:"session_id"`
	State             string          `json:"state"`
	Stage             string          `json:"stage"`
	PlannedBytes      int64           `json:"planned_bytes"`
	ConfirmedBytes    int64           `json:"confirmed_bytes"`
	FileCount         int64           `json:"file_count"`
	FilesProcessed    int64           `json:"files_processed"`
	TotalBytes        int64           `json:"total_bytes"`
	BytesProcessed    int64           `json:"bytes_processed"`
	CurrentPath       string          `json:"current_path"`
	ConfirmedRevision *int64          `json:"confirmed_revision,omitempty"`
}

type syncParams struct {
	BaseURL               string   `json:"base_url"`
	BearerToken           string   `json:"bearer_token"`
	WorkspaceID           string   `json:"workspace_id"`
	FolderID              string   `json:"folder_id,omitempty"`
	SourceBindingID       string   `json:"source_binding_id,omitempty"`
	Directory             string   `json:"directory"`
	Direction             string   `json:"direction"`
	Policy                string   `json:"policy,omitempty"`
	Scope                 []string `json:"scope,omitempty"`
	BaseRevision          *int64   `json:"base_revision,omitempty"`
	SessionID             string   `json:"session_id,omitempty"`
	TaskID                string   `json:"task_id,omitempty"`
	TaskHubOperationKey   string   `json:"task_hub_operation_key,omitempty"`
	IdempotencyKey        string   `json:"idempotency_key,omitempty"`
	RetryCommitAfterQuota bool     `json:"retry_commit_after_quota,omitempty"`
}

type cancelParams struct {
	RequestID json.RawMessage `json:"request_id"`
}

type liveSyncParams struct {
	BaseURL       string   `json:"base_url"`
	BearerToken   string   `json:"bearer_token"`
	WorkspaceID   string   `json:"workspace_id"`
	Directory     string   `json:"directory"`
	Scope         []string `json:"scope,omitempty"`
	RescanSeconds int      `json:"rescan_seconds,omitempty"`
}

type handoffCheckpointParams struct {
	BaseURL        string   `json:"base_url"`
	BearerToken    string   `json:"bearer_token"`
	WorkspaceID    string   `json:"workspace_id"`
	Directory      string   `json:"directory"`
	Scope          []string `json:"scope,omitempty"`
	IdempotencyKey string   `json:"idempotency_key"`
}

type preservedListParams struct {
	WorkspaceID string `json:"workspace_id"`
	Directory   string `json:"directory"`
	FolderID    string `json:"folder_id,omitempty"`
}

type preservedRestoreParams struct {
	WorkspaceID string `json:"workspace_id"`
	Directory   string `json:"directory"`
	ID          string `json:"id"`
	FolderID    string `json:"folder_id,omitempty"`
}

type hubRootParams struct {
	Directory string `json:"directory"`
}

type hubFilesParams struct {
	Directory string `json:"directory"`
	Identity  string `json:"identity"`
	Path      string `json:"path,omitempty"`
}

type hubFileGetParams struct {
	Directory string `json:"directory"`
	Identity  string `json:"identity"`
	Path      string `json:"path"`
	MaxBytes  int    `json:"max_bytes"`
}

type hubFileSnapshotParams struct {
	Directory string `json:"directory"`
	Identity  string `json:"identity"`
	Path      string `json:"path"`
}

type hubFileChunkParams struct {
	Directory string `json:"directory"`
	Identity  string `json:"identity"`
	Path      string `json:"path"`
	Version   string `json:"version"`
	Offset    int64  `json:"offset"`
	MaxBytes  int    `json:"max_bytes"`
}

type hubCommandParams struct {
	Directory string `json:"directory"`
	Identity  string `json:"identity"`
	Command   string `json:"command"`
	TimeoutMS int    `json:"timeout_ms"`
}

type hubCommandProbeResult struct {
	Available bool `json:"available"`
}

type hubCommandResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	TimedOut bool   `json:"timed_out"`
	Stopped  bool   `json:"stopped"`
}

type hubFilesResult struct {
	Entries []client.HubFileEntry `json:"entries"`
}

type preservedRestoreResult struct {
	Restored bool `json:"restored"`
}

type preservedDeleteResult struct {
	Deleted bool `json:"deleted"`
}

type syncResult struct {
	Session   client.SyncSession `json:"session"`
	Conflicts []client.Conflict  `json:"conflicts"`
}

type cancelResult struct {
	Cancelled bool               `json:"cancelled"`
	Session   client.SyncSession `json:"session"`
}

type handoffCheckpointResult struct {
	Revision int64 `json:"revision"`
}

func run(in io.Reader, out, errOut io.Writer) error {
	return runContext(context.Background(), in, out, errOut)
}

func runContext(ctx context.Context, in io.Reader, out, errOut io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s := &stdioServer{in: in, out: out, errOut: errOut, ctx: ctx}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), maxRequestLine)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		s.handleLine(line)
	}
	if err := scanner.Err(); err != nil {
		_ = s.writeError(json.RawMessage("null"), -32700, "request line exceeds the size limit")
		fmt.Fprintln(s.errOut, "morde-sync: invalid or oversized input line")
	}
	s.stopActiveAtInputEnd()
	s.wg.Wait()
	return scanner.Err()
}

func (s *stdioServer) stopActiveAtInputEnd() {
	s.mu.Lock()
	operation := s.active
	ownedCancellation := false
	if operation != nil {
		operation.mu.Lock()
		if !operation.cancelRequested {
			operation.cancelRequested = true
			operation.cancel()
			ownedCancellation = true
		}
		operation.mu.Unlock()
	}
	s.mu.Unlock()
	if operation == nil || !ownedCancellation {
		return
	}
	<-operation.done
	operation.mu.Lock()
	result := operation.result
	liveIdle := operation.liveIdle
	sessionActive := operation.sessionActive
	operation.mu.Unlock()
	if operation.live && liveIdle && !sessionActive {
		return
	}
	if result.Session.ID == "" {
		fmt.Fprintln(s.errOut, "morde-sync: stopped locally; server session state is unknown")
		return
	}
	if terminalSessionState(result.Session.State) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cancelTimeout)
	defer cancel()
	remoteSession, err := operation.client.Cancel(ctx, operation.workspace, result.Session.ID, operation.directory)
	if err != nil || remoteSession.State != "cancelled" {
		fmt.Fprintln(s.errOut, "morde-sync: server cancellation could not be confirmed; session state is unknown")
	}
}

func (s *stdioServer) handleLine(line []byte) {
	var request rpcRequest
	if err := decodeStrict(line, &request); err != nil {
		_ = s.writeError(json.RawMessage("null"), -32700, "invalid JSON-RPC request")
		return
	}
	idKey, ok := requestIDKey(request.ID)
	if !ok || request.JSONRPC != "2.0" || request.Method == "" || len(request.Params) == 0 || bytes.Equal(bytes.TrimSpace(request.Params), []byte("null")) {
		_ = s.writeError(validIDOrNull(request.ID), -32600, "invalid JSON-RPC request")
		return
	}
	switch request.Method {
	case "sync":
		s.startSync(request.ID, idKey, request.Params)
	case "live_sync":
		s.startLiveSync(request.ID, idKey, request.Params)
	case "handoff_checkpoint":
		s.startHandoffCheckpoint(request.ID, idKey, request.Params)
	case "list_preserved":
		s.listPreserved(request.ID, request.Params)
	case "restore_preserved":
		s.restorePreserved(request.ID, request.Params)
	case "delete_preserved":
		s.deletePreserved(request.ID, request.Params)
	case "hub_root":
		s.hubRoot(request.ID, request.Params)
	case "hub_files":
		s.hubFiles(request.ID, request.Params)
	case "hub_file_get":
		s.hubFileGet(request.ID, request.Params)
	case "hub_file_snapshot":
		s.hubFileSnapshot(request.ID, request.Params)
	case "hub_file_chunk":
		s.hubFileChunk(request.ID, request.Params)
	case "hub_command":
		s.hubCommand(request.ID, request.Params)
	case "hub_command_probe":
		s.hubCommandProbe(request.ID, request.Params)
	case "cancel":
		s.startCancel(request.ID, request.Params)
	default:
		_ = s.writeError(request.ID, -32601, "method not found")
	}
}

func (s *stdioServer) hubRoot(id json.RawMessage, rawParams json.RawMessage) {
	var params hubRootParams
	if err := decodeStrict(rawParams, &params); err != nil || params.Directory == "" || len(params.Directory) > 4096 {
		_ = s.writeError(id, -32602, "invalid selected directory parameters")
		return
	}
	selection, err := client.SelectHubRoot(params.Directory)
	if err != nil {
		_ = s.writeError(id, -32030, "selected directory could not be verified")
		return
	}
	_ = s.writeResult(id, selection)
}

func (s *stdioServer) hubFiles(id json.RawMessage, rawParams json.RawMessage) {
	var params hubFilesParams
	if err := decodeStrict(rawParams, &params); err != nil || params.Directory == "" || len(params.Directory) > 4096 || len(params.Identity) != 64 || len(params.Path) > 1024 {
		_ = s.writeError(id, -32602, "invalid selected directory parameters")
		return
	}
	entries, err := client.ListHubFiles(params.Directory, params.Identity, params.Path)
	if err != nil {
		_ = s.writeError(id, -32030, "selected directory could not be verified")
		return
	}
	_ = s.writeResult(id, hubFilesResult{Entries: entries})
}

func (s *stdioServer) hubFileGet(id json.RawMessage, rawParams json.RawMessage) {
	var params hubFileGetParams
	if err := decodeStrict(rawParams, &params); err != nil || params.Directory == "" || len(params.Directory) > 4096 || len(params.Identity) != 64 ||
		params.Path == "" || len(params.Path) > 1024 || params.MaxBytes < 1 || params.MaxBytes > client.HubInspectMaxBytes {
		_ = s.writeError(id, -32602, "invalid selected file parameters")
		return
	}
	content, err := client.ReadHubFile(params.Directory, params.Identity, params.Path, params.MaxBytes)
	if err != nil {
		_ = s.writeError(id, -32030, "selected file could not be verified")
		return
	}
	_ = s.writeResult(id, content)
}

func (s *stdioServer) hubFileSnapshot(id json.RawMessage, rawParams json.RawMessage) {
	var params hubFileSnapshotParams
	if err := decodeStrict(rawParams, &params); err != nil || params.Directory == "" || len(params.Directory) > 4096 || len(params.Identity) != 64 ||
		params.Path == "" || len(params.Path) > 1024 {
		_ = s.writeError(id, -32602, "invalid selected file parameters")
		return
	}
	snapshot, err := client.SnapshotHubFile(params.Directory, params.Identity, params.Path)
	if err != nil {
		_ = s.writeError(id, -32030, "selected file could not be verified")
		return
	}
	_ = s.writeResult(id, snapshot)
}

func (s *stdioServer) hubFileChunk(id json.RawMessage, rawParams json.RawMessage) {
	var params hubFileChunkParams
	if err := decodeStrict(rawParams, &params); err != nil || params.Directory == "" || len(params.Directory) > 4096 || len(params.Identity) != 64 ||
		params.Path == "" || len(params.Path) > 1024 || len(params.Version) != 64 || params.Offset < 0 ||
		params.MaxBytes < 1 || params.MaxBytes > client.HubInspectMaxChunkBytes {
		_ = s.writeError(id, -32602, "invalid selected file parameters")
		return
	}
	chunk, err := client.ReadHubFileChunk(params.Directory, params.Identity, params.Path, params.Version, params.Offset, params.MaxBytes)
	if err != nil {
		if errors.Is(err, client.ErrHubFileChanged) {
			_ = s.writeError(id, -32032, "selected file changed after snapshot")
			return
		}
		_ = s.writeError(id, -32030, "selected file could not be verified")
		return
	}
	_ = s.writeResult(id, chunk)
}

func (s *stdioServer) hubCommand(id json.RawMessage, rawParams json.RawMessage) {
	var params hubCommandParams
	if err := decodeStrict(rawParams, &params); err != nil || params.Directory == "" || len(params.Directory) > 4096 ||
		len(params.Identity) != 64 || !utf8.ValidString(params.Command) || strings.ContainsRune(params.Command, '\x00') ||
		len(params.Command) < 1 || len(params.Command) > 8<<10 || params.TimeoutMS < 1000 || params.TimeoutMS > 300000 {
		_ = s.writeError(id, -32602, "invalid command parameters")
		return
	}
	selection, err := client.SelectHubRoot(params.Directory)
	if err != nil || selection.Directory != params.Directory || selection.Identity != params.Identity {
		_ = s.writeError(id, -32030, "command workspace could not be verified")
		return
	}
	result, err := runHubCommand(s.ctx, selection.Directory, selection.Identity, params.Command, params.TimeoutMS)
	if err != nil {
		_ = s.writeError(id, -32031, "command execution was not confirmed")
		return
	}
	_ = s.writeResult(id, result)
}

func (s *stdioServer) hubCommandProbe(id json.RawMessage, rawParams json.RawMessage) {
	var params struct{}
	if err := decodeStrict(rawParams, &params); err != nil {
		_ = s.writeError(id, -32602, "invalid command probe parameters")
		return
	}
	_ = s.writeResult(id, hubCommandProbeResult{Available: probeHubCommand(s.ctx)})
}

func (s *stdioServer) listPreserved(id json.RawMessage, rawParams json.RawMessage) {
	var params preservedListParams
	if err := decodeStrict(rawParams, &params); err != nil || validatePreservedListParams(params) != nil {
		_ = s.writeError(id, -32602, "invalid local recovery parameters")
		return
	}
	if s.hasActiveSync() {
		_ = s.writeError(id, -32001, "another sync is active")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), localRecoveryTimeout)
	defer cancel()
	items, err := client.ListPreserved(ctx, params.Directory, params.WorkspaceID)
	if err != nil {
		_ = s.writeError(id, -32020, "could not list local recovery copies")
		return
	}
	if params.FolderID != "" {
		nodeItems, err := client.ListSyncthingPreserved(ctx, params.Directory, params.WorkspaceID, params.FolderID)
		if err != nil {
			_ = s.writeError(id, -32020, "could not list local recovery copies")
			return
		}
		for _, nodeItem := range nodeItems {
			for _, item := range items {
				if item.ID == nodeItem.ID {
					_ = s.writeError(id, -32020, "local recovery copy IDs conflict")
					return
				}
			}
			items = append(items, nodeItem)
		}
	}
	if items == nil {
		items = []client.PreservedFile{}
	}
	_ = s.writeResult(id, items)
}

func (s *stdioServer) restorePreserved(id json.RawMessage, rawParams json.RawMessage) {
	var params preservedRestoreParams
	if err := decodeStrict(rawParams, &params); err != nil || validatePreservedRestoreParams(params) != nil {
		_ = s.writeError(id, -32602, "invalid local recovery parameters")
		return
	}
	if s.hasActiveSync() {
		_ = s.writeError(id, -32001, "another sync is active")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), localRecoveryTimeout)
	defer cancel()
	err := error(nil)
	if params.FolderID != "" {
		err = client.RestoreSyncthingPreserved(ctx, params.Directory, params.WorkspaceID, params.FolderID, params.ID)
	}
	if params.FolderID == "" || errors.Is(err, fs.ErrNotExist) {
		err = client.RestorePreserved(ctx, params.Directory, params.WorkspaceID, params.ID)
	}
	if err != nil {
		_ = s.writeError(id, -32021, "could not restore local recovery copy")
		return
	}
	_ = s.writeResult(id, preservedRestoreResult{Restored: true})
}

func (s *stdioServer) deletePreserved(id json.RawMessage, rawParams json.RawMessage) {
	var params preservedRestoreParams
	if err := decodeStrict(rawParams, &params); err != nil || validatePreservedRestoreParams(params) != nil {
		_ = s.writeError(id, -32602, "invalid local recovery parameters")
		return
	}
	if s.hasActiveSync() {
		_ = s.writeError(id, -32001, "another sync is active")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), localRecoveryTimeout)
	defer cancel()
	err := error(nil)
	if params.FolderID != "" {
		err = client.DeleteSyncthingPreserved(ctx, params.Directory, params.WorkspaceID, params.FolderID, params.ID)
	}
	if params.FolderID == "" || errors.Is(err, fs.ErrNotExist) {
		err = client.DeletePreserved(ctx, params.Directory, params.WorkspaceID, params.ID)
	}
	if err != nil {
		_ = s.writeError(id, -32022, "could not delete local recovery copy")
		return
	}
	_ = s.writeResult(id, preservedDeleteResult{Deleted: true})
}

func (s *stdioServer) hasActiveSync() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active != nil
}

func (s *stdioServer) startHandoffCheckpoint(id json.RawMessage, idKey string, rawParams json.RawMessage) {
	var params handoffCheckpointParams
	if err := decodeStrict(rawParams, &params); err != nil || validateHandoffCheckpointParams(params) != nil {
		_ = s.writeError(id, -32602, "invalid handoff checkpoint parameters")
		return
	}
	apiClient, err := client.New(params.BaseURL, params.BearerToken, nil)
	if err != nil {
		_ = s.writeError(id, -32602, "invalid handoff checkpoint parameters")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	operation := &activeSync{id: append(json.RawMessage(nil), id...), idKey: idKey, client: apiClient,
		workspace: params.WorkspaceID, directory: params.Directory, allowTerminalCancel: true, cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	if s.active != nil {
		s.mu.Unlock()
		cancel()
		_ = s.writeError(id, -32001, "another sync is active")
		return
	}
	s.active = operation
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		progress := func(value client.Progress) {
			operation.mu.Lock()
			if value.SessionID != "" {
				operation.result.Session.ID = value.SessionID
				operation.result.Session.WorkspaceID = params.WorkspaceID
				operation.result.Session.State = value.State
				operation.sessionActive = !terminalSessionState(value.State)
			}
			operation.mu.Unlock()
			_ = s.writeNotification(progressNotification{JSONRPC: "2.0", Method: "progress",
				Params: progressParams{RequestID: operation.id, State: "running", Stage: "verify",
					PlannedBytes: value.PlannedBytes, ConfirmedBytes: value.ConfirmedBytes}})
		}
		progress(client.Progress{Stage: "verify"})
		revision, checkpointErr := apiClient.HandoffCheckpoint(ctx, client.LiveSyncOptions{
			WorkspaceID: params.WorkspaceID, Directory: params.Directory, Scope: params.Scope,
			IdempotencyKey: params.IdempotencyKey, Progress: progress,
			OnResult: func(result client.Result) {
				operation.mu.Lock()
				operation.result = result
				operation.sessionActive = result.Session.ID != "" && !terminalSessionState(result.Session.State)
				operation.mu.Unlock()
			},
		})
		operation.mu.Lock()
		close(operation.done)
		operation.mu.Unlock()
		if checkpointErr != nil {
			message := "handoff checkpoint failed"
			if errors.Is(checkpointErr, context.Canceled) {
				message = "handoff checkpoint cancelled"
			}
			_ = s.writeError(id, -32010, message)
		} else {
			_ = s.writeNotification(progressNotification{JSONRPC: "2.0", Method: "progress",
				Params: progressParams{RequestID: operation.id, State: "completed", Stage: "complete"}})
			_ = s.writeResult(id, handoffCheckpointResult{Revision: revision})
		}
		s.clearActiveUnlessCancelling(operation)
	}()
}

func (s *stdioServer) startLiveSync(id json.RawMessage, idKey string, rawParams json.RawMessage) {
	var params liveSyncParams
	if err := decodeStrict(rawParams, &params); err != nil {
		_ = s.writeError(id, -32602, "invalid live sync parameters")
		return
	}
	if err := validateLiveSyncParams(params); err != nil {
		_ = s.writeError(id, -32602, "invalid live sync parameters")
		return
	}
	apiClient, err := client.New(params.BaseURL, params.BearerToken, nil)
	if err != nil {
		_ = s.writeError(id, -32602, "invalid live sync parameters")
		return
	}
	interval := 30 * time.Second
	if params.RescanSeconds != 0 {
		interval = time.Duration(params.RescanSeconds) * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	operation := &activeSync{id: append(json.RawMessage(nil), id...), idKey: idKey, client: apiClient,
		workspace: params.WorkspaceID, directory: params.Directory, live: true, cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	if s.active != nil {
		s.mu.Unlock()
		cancel()
		_ = s.writeError(id, -32001, "another sync is active")
		return
	}
	s.active = operation
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		liveErr := apiClient.LiveSync(ctx, client.LiveSyncOptions{WorkspaceID: params.WorkspaceID, Directory: params.Directory,
			Scope: params.Scope, RescanInterval: interval,
			Progress: func(value client.Progress) {
				operation.mu.Lock()
				if value.SessionID != "" {
					operation.result.Session.ID = value.SessionID
					operation.result.Session.WorkspaceID = params.WorkspaceID
					operation.result.Session.State = value.State
					operation.sessionActive = value.State != "completed" && value.State != "conflict" && value.State != "cancelled" && value.State != "failed" && value.State != "expired"
					operation.liveIdle = false
				} else if value.Stage == "watch" || value.Stage == "changes" || value.Stage == "rescan_fallback" {
					operation.liveIdle = true
					operation.sessionActive = false
				} else if value.Stage == "sync_planning" || value.Stage == "sync" {
					operation.liveIdle = false
				}
				operation.mu.Unlock()
				_ = s.writeNotification(progressNotification{JSONRPC: "2.0", Method: "progress",
					Params: progressParams{RequestID: operation.id, SessionID: value.SessionID, State: value.State, Stage: normalizeLiveProgressStage(value.State, value.Stage),
						PlannedBytes: value.PlannedBytes, ConfirmedBytes: value.ConfirmedBytes, CurrentPath: value.CurrentPath, ConfirmedRevision: value.ConfirmedRevision}})
			},
			OnResult: func(result client.Result) {
				operation.mu.Lock()
				operation.result = result
				operation.sessionActive = result.Session.ID != "" && result.Session.State != "completed" && result.Session.State != "conflict" && result.Session.State != "cancelled" && result.Session.State != "failed" && result.Session.State != "expired"
				operation.mu.Unlock()
			},
		})
		operation.mu.Lock()
		close(operation.done)
		operation.mu.Unlock()
		var conflict *client.LiveConflictError
		if errors.As(liveErr, &conflict) {
			_ = s.writeResult(id, syncResult{Session: conflict.Session, Conflicts: nonNilConflicts(conflict.Conflicts)})
		} else if liveErr != nil {
			message := sanitizedError(liveErr, params.BearerToken, params.Directory)
			if errors.Is(liveErr, context.Canceled) {
				message = "live sync cancelled"
			}
			_ = s.writeError(id, -32010, message)
		} else {
			_ = s.writeError(id, -32010, "live sync stopped unexpectedly")
		}
		s.clearActiveUnlessCancelling(operation)
	}()
}

func normalizeLiveProgressStage(state, stage string) string {
	if state == "conflict" {
		return "conflict"
	}
	if state == "reconnecting" || stage == "retry" || stage == "rescan_fallback" {
		return "reconnect"
	}
	switch stage {
	case "watch", "watching", "changes":
		return "watching"
	case "transfer", "verify", "apply", "complete":
		return "transfer"
	case "sync_planning", "planning", "scan", "compare":
		return "scan"
	default:
		return "scan"
	}
}

func (s *stdioServer) startSync(id json.RawMessage, idKey string, rawParams json.RawMessage) {
	var params syncParams
	if err := decodeStrict(rawParams, &params); err != nil {
		_ = s.writeError(id, -32602, "invalid sync parameters")
		return
	}
	if err := validateSyncParams(params); err != nil {
		_ = s.writeError(id, -32602, "invalid sync parameters")
		return
	}
	apiClient, err := client.New(params.BaseURL, params.BearerToken, nil)
	if err != nil {
		_ = s.writeError(id, -32602, "invalid sync parameters")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	operation := &activeSync{
		id: append(json.RawMessage(nil), id...), idKey: idKey, client: apiClient,
		workspace: params.WorkspaceID, directory: params.Directory, cancel: cancel, done: make(chan struct{}),
	}
	s.mu.Lock()
	if s.active != nil {
		s.mu.Unlock()
		cancel()
		_ = s.writeError(id, -32001, "another sync is active")
		return
	}
	s.active = operation
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		result, syncErr := apiClient.Sync(ctx, client.SyncOptions{
			WorkspaceID: params.WorkspaceID, FolderID: params.FolderID, SourceBindingID: params.SourceBindingID, Directory: params.Directory, Direction: params.Direction,
			Policy: params.Policy,
			Scope:  params.Scope, BaseRevision: params.BaseRevision, SessionID: params.SessionID,
			TaskID: params.TaskID, TaskHubOperationKey: params.TaskHubOperationKey,
			IdempotencyKey: params.IdempotencyKey, RetryCommitAfterQuota: params.RetryCommitAfterQuota,
			Progress: func(value client.Progress) {
				_ = s.writeNotification(progressNotification{
					JSONRPC: "2.0", Method: "progress",
					Params: progressParams{RequestID: operation.id, SessionID: value.SessionID, State: value.State,
						Stage: value.Stage, PlannedBytes: value.PlannedBytes, ConfirmedBytes: value.ConfirmedBytes,
						FileCount: value.FileCount, FilesProcessed: value.FilesProcessed, TotalBytes: value.TotalBytes,
						BytesProcessed: value.BytesProcessed, CurrentPath: value.CurrentPath},
				})
			},
		})
		operation.mu.Lock()
		operation.result = result
		close(operation.done)
		operation.mu.Unlock()

		if syncErr != nil {
			message := sanitizedError(syncErr, params.BearerToken, params.Directory)
			if errors.Is(syncErr, context.Canceled) {
				message = "sync cancelled"
			}
			_ = s.writeError(id, -32010, message)
		} else {
			_ = s.writeResult(id, syncResult{Session: result.Session, Conflicts: nonNilConflicts(result.Conflicts)})
		}
		s.clearActiveUnlessCancelling(operation)
	}()
}

func (s *stdioServer) startCancel(id, rawParams json.RawMessage) {
	var params cancelParams
	if err := decodeStrict(rawParams, &params); err != nil {
		_ = s.writeError(id, -32602, "invalid cancel parameters")
		return
	}
	requestIDKey, ok := requestIDKey(params.RequestID)
	if !ok {
		_ = s.writeError(id, -32602, "invalid cancel parameters")
		return
	}
	s.mu.Lock()
	operation := s.active
	if operation == nil || operation.idKey != requestIDKey {
		s.mu.Unlock()
		_ = s.writeError(id, -32004, "no matching sync is active")
		return
	}
	operation.mu.Lock()
	if operation.cancelRequested {
		operation.mu.Unlock()
		s.mu.Unlock()
		_ = s.writeError(id, -32004, "sync cancellation is already in progress")
		return
	}
	operation.cancelRequested = true
	operation.cancel()
	operation.mu.Unlock()
	s.wg.Add(1)
	s.mu.Unlock()

	go s.finishCancel(id, operation)
}

func (s *stdioServer) finishCancel(id json.RawMessage, operation *activeSync) {
	defer s.wg.Done()
	<-operation.done
	operation.mu.Lock()
	result := operation.result
	liveIdle := operation.liveIdle
	sessionActive := operation.sessionActive
	operation.mu.Unlock()
	if operation.live && liveIdle && !sessionActive {
		_ = s.writeResult(id, cancelResult{Cancelled: true, Session: result.Session})
		s.clearActive(operation)
		return
	}
	if result.Session.ID == "" {
		_ = s.writeError(id, -32010, "local sync stopped, but server cancellation could not be confirmed; session state is unknown")
		s.clearActive(operation)
		return
	}
	if (operation.live || operation.allowTerminalCancel) && !sessionActive && terminalSessionState(result.Session.State) {
		_ = s.writeResult(id, cancelResult{Cancelled: true, Session: result.Session})
		s.clearActive(operation)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cancelTimeout)
	defer cancel()
	remoteSession, err := operation.client.Cancel(ctx, operation.workspace, result.Session.ID, operation.directory)
	if err != nil || (remoteSession.State != "cancelled" && !((operation.live || operation.allowTerminalCancel) && terminalSessionState(remoteSession.State))) {
		_ = s.writeError(id, -32010, "local sync stopped, but server cancellation could not be confirmed; session state is unknown")
		s.clearActive(operation)
		return
	}
	_ = s.writeResult(id, cancelResult{Cancelled: true, Session: remoteSession})
	s.clearActive(operation)
}

func terminalSessionState(state string) bool {
	switch state {
	case "completed", "conflict", "cancelled", "failed", "expired":
		return true
	default:
		return false
	}
}

func validateSyncParams(value syncParams) error {
	if value.BaseURL == "" || len(value.BaseURL) > 4096 || value.BearerToken == "" || len(value.BearerToken) > 8192 ||
		value.WorkspaceID == "" || !workspaceIDRE.MatchString(value.WorkspaceID) || value.Directory == "" || len(value.Directory) > 4096 {
		return errors.New("required field is invalid")
	}
	if value.Direction != "upload" && value.Direction != "download" && value.Direction != "bidirectional" {
		return errors.New("direction is invalid")
	}
	if value.Policy != "" && value.Policy != "fill_missing" && value.Policy != "compare_merge" && value.Policy != "live" {
		return errors.New("policy is invalid")
	}
	if value.BaseRevision != nil && *value.BaseRevision < 0 {
		return errors.New("base_revision is invalid")
	}
	if value.FolderID != "" && (!syncFolderIDRE.MatchString(value.FolderID) || !sourceBindingIDRE.MatchString(value.SourceBindingID) || value.Direction != "upload" || value.Policy != "compare_merge" ||
		value.BaseRevision == nil || *value.BaseRevision != 0 || len(value.Scope) != 0 || value.TaskID != "" || value.TaskHubOperationKey != "") {
		return errors.New("project folder sync requires a source binding and unscoped compare-merge upload from revision zero")
	}
	if value.FolderID == "" && value.SourceBindingID != "" {
		return errors.New("source binding ID requires a project folder")
	}
	if value.SessionID != "" && !sessionIDRE.MatchString(value.SessionID) {
		return errors.New("session_id is invalid")
	}
	if value.RetryCommitAfterQuota && value.SessionID == "" {
		return errors.New("retry_commit_after_quota requires session_id")
	}
	if value.IdempotencyKey != "" && !idempotencyRE.MatchString(value.IdempotencyKey) {
		return errors.New("idempotency_key is invalid")
	}
	if (value.TaskID == "") != (value.TaskHubOperationKey == "") || value.TaskID != "" &&
		(!taskIDRE.MatchString(value.TaskID) || !taskOperationRE.MatchString(value.TaskHubOperationKey) || value.Direction != "upload" ||
			value.SessionID == "" && (value.Policy != "compare_merge" || value.BaseRevision == nil || len(value.Scope) != 1) ||
			value.SessionID != "" && (value.Policy != "" && value.Policy != "compare_merge" || value.Scope != nil && len(value.Scope) != 1)) {
		return errors.New("Task sync association is invalid")
	}
	if len(value.Scope) > maxScopeEntries {
		return errors.New("scope has too many entries")
	}
	seen := make(map[string]struct{}, len(value.Scope))
	for _, scope := range value.Scope {
		if !validRelativePath(scope) {
			return errors.New("scope contains an invalid path")
		}
		key := strings.ToLower(scope)
		if _, exists := seen[key]; exists {
			return errors.New("scope contains a duplicate path")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateLiveSyncParams(value liveSyncParams) error {
	if value.BaseURL == "" || len(value.BaseURL) > 4096 || value.BearerToken == "" || len(value.BearerToken) > 8192 ||
		value.WorkspaceID == "" || !workspaceIDRE.MatchString(value.WorkspaceID) || value.Directory == "" || len(value.Directory) > 4096 {
		return errors.New("required field is invalid")
	}
	if value.RescanSeconds != 0 && (value.RescanSeconds < 5 || value.RescanSeconds > 3600) {
		return errors.New("rescan_seconds is invalid")
	}
	if len(value.Scope) > maxScopeEntries {
		return errors.New("scope has too many entries")
	}
	seen := make(map[string]struct{}, len(value.Scope))
	for _, scope := range value.Scope {
		if !validRelativePath(scope) {
			return errors.New("scope contains an invalid path")
		}
		key := strings.ToLower(scope)
		if _, exists := seen[key]; exists {
			return errors.New("scope contains a duplicate path")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateHandoffCheckpointParams(value handoffCheckpointParams) error {
	if value.BaseURL == "" || len(value.BaseURL) > 4096 || value.BearerToken == "" || len(value.BearerToken) > 8192 ||
		value.WorkspaceID == "" || !workspaceIDRE.MatchString(value.WorkspaceID) || value.Directory == "" || len(value.Directory) > 4096 ||
		value.IdempotencyKey == "" || len(value.IdempotencyKey) > 63 || !idempotencyRE.MatchString(value.IdempotencyKey) {
		return errors.New("required field is invalid")
	}
	if len(value.Scope) > maxScopeEntries {
		return errors.New("scope has too many entries")
	}
	seen := make(map[string]struct{}, len(value.Scope))
	for _, scope := range value.Scope {
		if !validRelativePath(scope) {
			return errors.New("scope contains an invalid path")
		}
		key := strings.ToLower(scope)
		if _, exists := seen[key]; exists {
			return errors.New("scope contains a duplicate path")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validatePreservedListParams(value preservedListParams) error {
	if value.WorkspaceID == "" || !workspaceIDRE.MatchString(value.WorkspaceID) ||
		value.Directory == "" || len(value.Directory) > 4096 || value.FolderID != "" && !folderIDRE.MatchString(value.FolderID) {
		return errors.New("required field is invalid")
	}
	return nil
}

func validatePreservedRestoreParams(value preservedRestoreParams) error {
	if value.WorkspaceID == "" || !workspaceIDRE.MatchString(value.WorkspaceID) ||
		value.Directory == "" || len(value.Directory) > 4096 || !preservedIDRE.MatchString(value.ID) ||
		value.FolderID != "" && !folderIDRE.MatchString(value.FolderID) {
		return errors.New("required field is invalid")
	}
	return nil
}

func validRelativePath(value string) bool {
	if value == "" || strings.ContainsRune(value, '\\') || strings.HasPrefix(value, "/") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func decodeStrict(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func requestIDKey(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", false
	}
	switch typed := value.(type) {
	case string:
		if typed == "" {
			return "", false
		}
		return "s:" + typed, true
	case json.Number:
		if strings.ContainsAny(typed.String(), ".eE") {
			return "", false
		}
		return "n:" + typed.String(), true
	default:
		return "", false
	}
}

func validIDOrNull(raw json.RawMessage) json.RawMessage {
	if _, ok := requestIDKey(raw); ok {
		return raw
	}
	return json.RawMessage("null")
}

func (s *stdioServer) writeResult(id json.RawMessage, result any) error {
	return s.writeJSON(rpcResponse{JSONRPC: "2.0", ID: validIDOrNull(id), Result: result})
}

func (s *stdioServer) writeError(id json.RawMessage, code int, message string) error {
	if strings.TrimSpace(message) == "" {
		message = "request failed"
	}
	return s.writeJSON(rpcResponse{JSONRPC: "2.0", ID: validIDOrNull(id), Error: &rpcError{Code: code, Message: message}})
}

func (s *stdioServer) writeNotification(value progressNotification) error {
	return s.writeJSON(value)
}

func (s *stdioServer) writeJSON(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if _, err := s.out.Write(append(encoded, '\n')); err != nil {
		return err
	}
	return nil
}

func (s *stdioServer) clearActive(operation *activeSync) {
	s.mu.Lock()
	if s.active == operation {
		s.active = nil
	}
	s.mu.Unlock()
}

func (s *stdioServer) clearActiveUnlessCancelling(operation *activeSync) {
	s.mu.Lock()
	operation.mu.Lock()
	if s.active == operation && !operation.cancelRequested {
		s.active = nil
	}
	operation.mu.Unlock()
	s.mu.Unlock()
}

func sanitizedError(err error, secrets ...string) string {
	message := err.Error()
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	if strings.TrimSpace(message) == "" {
		return "sync failed"
	}
	return message
}

func nonNilConflicts(values []client.Conflict) []client.Conflict {
	if values == nil {
		return []client.Conflict{}
	}
	return values
}
