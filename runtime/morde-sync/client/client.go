// Package client implements resumable HTTP transfers for the morde-sync core.
package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	mordesync "morde-sync"
)

const (
	manifestPageSize  = 500
	maxJSONBytes      = 16 << 20
	maxErrorJSONBytes = 16 << 10
	maxManifestFiles  = 100_000
)

var (
	workspaceIDPattern  = regexp.MustCompile(`^w[a-z0-9]{7}$`)
	syncFolderIDPattern = regexp.MustCompile(`^morde-[a-f0-9]{32}$`)
	sessionIDPattern    = regexp.MustCompile(`^p[a-z0-9]{7}$`)
	taskIDPattern       = regexp.MustCompile(`^q[a-z0-9]{7}$`)
	taskOperationKey    = regexp.MustCompile(`^agent:[a-f0-9]{64}$`)
	digestPattern       = regexp.MustCompile(`^[a-f0-9]{64}$`)
	idempotencyPattern  = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)
)

// Client uses a workspace-scoped bearer credential. The token is never placed
// in a URL or an error value.
type Client struct {
	baseURL *url.URL
	token   string
	http    *http.Client
}

// New validates the API origin and constructs a client. A supplied HTTP client
// is used as-is; the default client does not follow redirects with credentials.
func New(baseURL, bearerToken string, transport *http.Client) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("Morde API URL must be an absolute HTTP(S) URL without query or fragment")
	}
	if bearerToken == "" || strings.ContainsAny(bearerToken, "\r\n") {
		return nil, errors.New("workspace bearer credential is required")
	}
	if transport == nil {
		transport = &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return &Client{baseURL: parsed, token: bearerToken, http: transport}, nil
}

// SyncOptions describes one resumable sync run. fill_missing remains additive;
// live is used by LiveSync and requires an explicit common baseline before it
// may replace or remove an existing local file.
type SyncOptions struct {
	WorkspaceID           string
	FolderID              string // Optional registered project folder for a manual upload.
	Directory             string
	Direction             string // upload, download, or bidirectional; defaults to bidirectional.
	Policy                string // fill_missing, compare_merge, or live; defaults to fill_missing.
	BaseRevision          *int64
	Scope                 []string
	DeletedPaths          []string
	DeletedDirectories    []string
	AckCursor             *string
	Baseline              mordesync.Manifest // Common state used only by the live policy.
	SessionID             string             // Set to resume an existing session.
	TaskID                string             // Optional explicit Task write fence for a device upload.
	TaskHubOperationKey   string             // Paired with TaskID to settle one durable Hub write.
	IdempotencyKey        string             // Optional stable create key for recovery after an ambiguous create response.
	RetryCommitAfterQuota bool               // Explicitly starts a new commit attempt after a saved quota rejection.
	// IdempotencyNamespace derives one stable server create key per distinct
	// request. Handoff uses this because a checkpoint may require several
	// sync sessions; reusing one key for different requests would conflict.
	IdempotencyNamespace string
	Progress             func(Progress)
}

// Progress is a transfer snapshot suitable for a progress bar or status label.
type Progress struct {
	SessionID         string
	State             string
	Stage             string
	ConfirmedRevision *int64
	PlannedBytes      int64
	ConfirmedBytes    int64
	FileCount         int64
	FilesProcessed    int64
	TotalBytes        int64
	BytesProcessed    int64
	CurrentPath       string
}

// Result preserves the latest session known to the client even when a transfer
// fails, so the caller can resume it by passing Session.ID in SyncOptions.
type Result struct {
	Session   SyncSession
	Conflicts []Conflict
}

// SyncSession is the transport representation of the Portal API session.
type SyncSession struct {
	ID              string           `json:"id"`
	WorkspaceID     string           `json:"workspace_id"`
	FolderID        string           `json:"folder_id,omitempty"`
	Direction       string           `json:"direction"`
	Policy          string           `json:"policy"`
	State           string           `json:"state"`
	BaseRevision    int64            `json:"base_revision"`
	TargetRevision  int64            `json:"target_revision"`
	Stage           string           `json:"stage"`
	PlannedBytes    int64            `json:"planned_bytes"`
	ConfirmedBytes  int64            `json:"confirmed_bytes"`
	TransferBlocks  []TransferBlock  `json:"transfer_blocks"`
	DownloadActions []DownloadAction `json:"download_actions"`
	Conflicts       []Conflict       `json:"conflicts"`
}

// DownloadAction is a path-level operation fixed by the server for the session.
type DownloadAction struct {
	Path      string  `json:"path"`
	Action    string  `json:"action"`
	SHA256    *string `json:"sha256"`
	SizeBytes int64   `json:"size_bytes"`
	Applied   bool    `json:"applied"`
}

type TransferBlock struct {
	Chunk    Chunk  `json:"chunk"`
	Role     string `json:"role"`
	Received bool   `json:"received"`
}

type Chunk struct {
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

type Conflict struct {
	Path         string  `json:"path"`
	Reason       string  `json:"reason"`
	LocalSHA256  *string `json:"local_sha256"`
	RemoteSHA256 *string `json:"remote_sha256"`
}

// HTTPError contains the method, status, and stable API error code. Response
// bodies can contain private server details and are intentionally not retained.
type HTTPError struct {
	Method string
	Status int
	Code   string
}

func (e *HTTPError) Error() string {
	message := fmt.Sprintf("Morde API %s returned HTTP %d", e.Method, e.Status)
	if validAPIErrorCode(e.Code) {
		return message + " (" + e.Code + ")"
	}
	return message
}

// Sync creates or resumes a session, transfers required file blocks, applies
// remote file and empty-directory actions, acknowledges durable local changes,
// and commits the session. Failed runs return the known session in Result.
func (c *Client) Sync(ctx context.Context, options SyncOptions) (Result, error) {
	return c.sync(ctx, options, true, false)
}

func (c *Client) sync(ctx context.Context, options SyncOptions, lockDirectory, keepCompletedMetadata bool) (Result, error) {
	if err := c.validateOptions(ctx, options); err != nil {
		return Result{}, err
	}
	policy := options.Policy
	if policy == "" {
		policy = "fill_missing"
	}
	if policy != "fill_missing" && policy != "compare_merge" && policy != "live" {
		return Result{}, errors.New("sync policy must be fill_missing, compare_merge, or live")
	}
	direction := options.Direction
	if direction == "" {
		direction = "bidirectional"
	}
	if direction != "upload" && direction != "download" && direction != "bidirectional" {
		return Result{}, errors.New("sync direction must be upload, download, or bidirectional")
	}

	directory, err := canonicalDirectory(options.Directory)
	if err != nil {
		return Result{}, fmt.Errorf("resolve local workspace directory: %w", err)
	}
	if lockDirectory {
		releaseOperation, err := acquireOperationLock(ctx, directory)
		if err != nil {
			return Result{}, err
		}
		defer releaseOperation()
	}
	local, localFiles, err := c.localManifest(ctx, options, directory, options.Progress)
	if err != nil {
		return Result{}, err
	}
	if options.TaskID != "" && options.SessionID == "" {
		if err := validateTaskLinkedManifest(options, local); err != nil {
			return Result{}, err
		}
	}
	var session SyncSession
	var saved resumeMetadata
	if options.SessionID != "" {
		if !sessionIDPattern.MatchString(options.SessionID) {
			return Result{}, errors.New("sync session ID is invalid")
		}
		saved, err = readResumeMetadata(directory, options.WorkspaceID)
		if err == nil && (saved.SessionID != options.SessionID || saved.Directory != directory || saved.WorkspaceID != options.WorkspaceID || saved.FolderID != options.FolderID ||
			saved.TaskID != options.TaskID || saved.TaskHubOperationKey != options.TaskHubOperationKey) {
			err = errors.New("saved sync metadata does not match the requested workspace, directory, and session")
		}
		if err == nil && mordesync.ValidateManifest(saved.Baseline) != nil {
			err = errors.New("saved sync baseline is invalid")
		}
		if err == nil && mordesync.ValidateDirectoryPathList(saved.DeletedDirectories) != nil {
			err = errors.New("saved deleted-directory list is invalid")
		}
		if err == nil && options.Scope != nil && !equalStrings(options.Scope, saved.Scope) {
			err = errors.New("resume scope does not match the saved session scope")
		}
		if err == nil && options.BaseRevision != nil && *options.BaseRevision != saved.BaseRevision {
			err = errors.New("resume base revision does not match the saved session")
		}
		if err == nil && options.Direction != "" && options.Direction != saved.Direction {
			err = errors.New("resume direction does not match the saved session")
		}
		if err == nil && options.Policy != "" && options.Policy != effectiveSavedPolicy(saved) {
			err = errors.New("resume policy does not match the saved session")
		}
		if err != nil {
			return Result{}, err
		}
		if options.RetryCommitAfterQuota {
			if !saved.CommitQuotaRejected {
				return Result{}, errors.New("a new commit attempt requires a saved quota rejection")
			}
			previousKey := saved.CommitIdempotencyKey
			saved.CommitIdempotencyKey, err = randomIdempotencyKey()
			if err != nil {
				return Result{}, err
			}
			if saved.CommitIdempotencyKey == previousKey {
				return Result{}, errors.New("could not generate a fresh workspace commit retry key")
			}
			saved.CommitQuotaRejected = false
			if err := writeResumeMetadata(directory, saved); err != nil {
				return Result{}, fmt.Errorf("save new workspace commit retry key: %w", err)
			}
		}
		options.Scope = append([]string(nil), saved.Scope...)
		options.Policy = effectiveSavedPolicy(saved)
		options.DeletedPaths = append([]string(nil), saved.DeletedPaths...)
		options.DeletedDirectories = append([]string(nil), saved.DeletedDirectories...)
		options.AckCursor = cloneStringPointer(saved.AckCursor)
		options.Baseline = saved.Baseline
		local = saved.Manifest
		localFiles = manifestFileMap(local)
		if options.TaskID != "" {
			if err := validateTaskLinkedManifest(options, local); err != nil {
				return Result{}, err
			}
		}
		session, err = c.getSession(ctx, options.WorkspaceID, options.SessionID)
	} else {
		previous, previousErr := readResumeMetadata(directory, options.WorkspaceID)
		if previousErr != nil && !errors.Is(previousErr, os.ErrNotExist) {
			return Result{}, previousErr
		}
		if previousErr == nil {
			if previous.FolderID != options.FolderID {
				return Result{}, errors.New("a previous sync operation belongs to another project folder; resolve it before starting a new operation")
			}
			if previous.SessionID == "" && (previous.TaskID != options.TaskID || previous.TaskHubOperationKey != options.TaskHubOperationKey) {
				return Result{}, errors.New("a previous sync operation belongs to another Task write; resolve it before starting a new operation")
			}
			if previous.SessionID != "" && !isTerminal(previous.State) {
				return Result{}, fmt.Errorf("workspace has resumable sync session %s; pass its ID or cancel it first", previous.SessionID)
			}
			if previous.SessionID == "" && !previous.CreateQuotaRejected && (!equalStrings(options.Scope, previous.Scope) || previous.Direction != direction || effectiveSavedPolicy(previous) != policy || !equalStrings(options.DeletedPaths, previous.DeletedPaths) || !equalStrings(options.DeletedDirectories, previous.DeletedDirectories) || !equalStringPointers(options.AckCursor, previous.AckCursor) || !equalManifests(local, previous.Manifest) || !equalManifests(options.Baseline, previous.Baseline)) {
				return Result{}, errors.New("a previous create has an unknown outcome; retry with the same workspace contents and scope")
			}
			if previous.SessionID == "" && !previous.CreateQuotaRejected && options.IdempotencyKey != "" && options.IdempotencyKey != previous.IdempotencyKey {
				return Result{}, errors.New("a previous create has an unknown outcome; retry with its saved idempotency key")
			}
		}
		baseRevision := int64(0)
		if previousErr == nil && previous.SessionID == "" && !previous.CreateQuotaRejected {
			baseRevision = previous.BaseRevision
			if options.BaseRevision != nil && *options.BaseRevision != baseRevision {
				return Result{}, errors.New("a previous create has an unknown outcome; base revision must remain unchanged")
			}
		} else if options.BaseRevision != nil {
			baseRevision = *options.BaseRevision
		} else {
			baseRevision, err = c.latestRevision(ctx, options.WorkspaceID)
		}
		if err == nil {
			key := options.IdempotencyKey
			retryAfterQuotaRejection := previousErr == nil && previous.SessionID == "" && previous.CreateQuotaRejected
			if retryAfterQuotaRejection {
				if key == "" || key == previous.IdempotencyKey {
					key, err = randomIdempotencyKey()
					if err == nil && key == previous.IdempotencyKey {
						err = errors.New("could not generate a fresh workspace create retry key")
					}
				}
			} else if options.IdempotencyNamespace != "" {
				key, err = derivedCreateKey(options.IdempotencyNamespace, c.baseURL.String(), directory,
					options.WorkspaceID, options.FolderID, direction, policy, baseRevision, options.Scope, local, options.DeletedPaths, options.DeletedDirectories, options.AckCursor,
					options.TaskID, options.TaskHubOperationKey)
				if err == nil && previousErr == nil && previous.SessionID == "" && key != previous.IdempotencyKey {
					return Result{}, errors.New("a previous create has an unknown outcome for another handoff, Portal, or directory; resolve it before retrying")
				}
			}
			if previousErr == nil && previous.SessionID == "" && !previous.CreateQuotaRejected {
				key = previous.IdempotencyKey
			}
			if key == "" && err == nil {
				key, err = randomIdempotencyKey()
			}
			if err == nil {
				saved = resumeMetadata{Version: 1, WorkspaceID: options.WorkspaceID, FolderID: options.FolderID, Directory: directory,
					Direction: direction, Policy: policy, BaseRevision: baseRevision, Scope: append([]string(nil), options.Scope...),
					TaskID: options.TaskID, TaskHubOperationKey: options.TaskHubOperationKey,
					DeletedPaths: append([]string(nil), options.DeletedPaths...), DeletedDirectories: append([]string(nil), options.DeletedDirectories...), AckCursor: cloneStringPointer(options.AckCursor),
					Baseline: options.Baseline, IdempotencyKey: key, Manifest: local}
				err = writeResumeMetadata(directory, saved)
			}
			if err == nil {
				session, err = c.createSession(ctx, options.WorkspaceID, options.FolderID, direction, policy, baseRevision, options.Scope, local, options.DeletedPaths, options.DeletedDirectories, options.AckCursor,
					options.TaskID, options.TaskHubOperationKey, key)
				if session.ID != "" {
					saved.SessionID = session.ID
					saved.State = session.State
					if saveErr := writeResumeMetadata(directory, saved); err == nil {
						err = saveErr
					}
				} else if isDefiniteQuotaRejection(err) {
					// A durable server rejection makes this key terminal; retain only enough local state to rotate it.
					saved = resumeMetadata{Version: 1, WorkspaceID: options.WorkspaceID, Directory: directory,
						TaskID: saved.TaskID, TaskHubOperationKey: saved.TaskHubOperationKey,
						IdempotencyKey: saved.IdempotencyKey, CreateQuotaRejected: true}
					if saveErr := writeResumeMetadata(directory, saved); saveErr != nil {
						err = errors.Join(err, fmt.Errorf("save rejected workspace sync create state: %w", saveErr))
					}
				}
			}
		}
	}
	result := Result{Session: session, Conflicts: append([]Conflict(nil), session.Conflicts...)}
	if err != nil {
		return result, err
	}
	if session.ID == "" || session.WorkspaceID != options.WorkspaceID || session.FolderID != options.FolderID || !sessionIDPattern.MatchString(session.ID) {
		return result, errors.New("Morde API returned an invalid sync session")
	}
	if err := validateSession(session); err != nil {
		return result, err
	}
	if options.FolderID != "" {
		if len(session.DownloadActions) != 0 {
			return result, errors.New("project upload session unexpectedly contains cloud-to-local actions")
		}
		for _, block := range session.TransferBlocks {
			if block.Role != "upload" {
				return result, errors.New("project upload session unexpectedly contains a download block")
			}
		}
	}
	if options.SessionID != "" {
		if session.Direction != saved.Direction || session.BaseRevision != saved.BaseRevision || session.Policy != effectiveSavedPolicy(saved) {
			return result, errors.New("cannot resume a session that uses an unsupported sync policy")
		}
		direction = session.Direction
	} else if session.Direction != direction || session.Policy != policy {
		return result, errors.New("Morde API returned a session that does not match the sync request")
	}
	progress := progressReporter{callback: options.Progress, session: session, fileCount: local.FileCount, totalBytes: local.TotalBytes}
	progress.report("")
	if isTerminal(session.State) {
		if session.State != "completed" || !keepCompletedMetadata {
			_ = removeResumeMetadata(directory, options.WorkspaceID, session.ID)
		}
		return result, nil
	}
	progress.session.Stage = "transfer"

	blocks, err := transferBlockMap(session.TransferBlocks)
	if err != nil {
		return result, err
	}
	actionsByPath, err := downloadActionMap(session.DownloadActions, session.Policy)
	if err != nil {
		return result, err
	}
	if direction == "upload" || direction == "bidirectional" {
		if err := c.uploadBlocks(ctx, options.WorkspaceID, options.Directory, local, blocks, &progress); err != nil {
			result.Session = progress.session
			return result, err
		}
	}
	if direction == "download" || direction == "bidirectional" {
		conflicts, applyErr := c.downloadMissing(ctx, options, session, localFiles, local.Directories, local.EmptyDirectories, blocks, actionsByPath, &progress)
		result.Conflicts = append(result.Conflicts, conflicts...)
		if applyErr != nil {
			result.Session = progress.session
			return result, applyErr
		}
	}
	commitKey := saved.CommitIdempotencyKey
	if commitKey == "" {
		commitKey, err = randomIdempotencyKey()
		if err != nil {
			result.Session = progress.session
			return result, err
		}
		saved.CommitIdempotencyKey = commitKey
		if err := writeResumeMetadata(directory, saved); err != nil {
			result.Session = progress.session
			return result, fmt.Errorf("save workspace commit retry key: %w", err)
		}
	}
	committed, err := c.commit(ctx, options.WorkspaceID, session.ID, commitKey)
	if err != nil {
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && httpErr.Status == http.StatusConflict && httpErr.Code == "quota_exceeded" {
			saved.CommitQuotaRejected = true
			if saveErr := writeResumeMetadata(directory, saved); saveErr != nil {
				return result, fmt.Errorf("save quota rejection for workspace commit retry: %w", saveErr)
			}
		}
		result.Session = progress.session
		return result, err
	}
	result.Session = committed
	progress.session = committed
	progress.report("")
	result.Conflicts = appendUniqueConflicts(result.Conflicts, committed.Conflicts)
	if !keepCompletedMetadata {
		if err := removeResumeMetadata(directory, options.WorkspaceID, session.ID); err != nil {
			return result, fmt.Errorf("sync committed but local resume metadata could not be removed: %w", err)
		}
	}
	return result, nil
}

func isDefiniteQuotaRejection(err error) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && httpErr.Status == http.StatusConflict && httpErr.Code == "quota_exceeded"
}

func (c *Client) localManifest(ctx context.Context, options SyncOptions, directory string, report func(Progress)) (mordesync.Manifest, map[string]mordesync.File, error) {
	if options.SessionID != "" {
		metadata, err := readResumeMetadata(directory, options.WorkspaceID)
		if err != nil {
			return mordesync.Manifest{}, nil, err
		}
		if metadata.SessionID != options.SessionID || metadata.Directory != directory {
			return mordesync.Manifest{}, nil, errors.New("saved sync metadata does not match the requested session and directory")
		}
		if err := mordesync.ValidateManifest(metadata.Manifest); err != nil {
			return mordesync.Manifest{}, nil, fmt.Errorf("saved local manifest is invalid: %w", err)
		}
		if report != nil {
			report(Progress{Stage: "planning", FileCount: metadata.Manifest.FileCount, FilesProcessed: metadata.Manifest.FileCount,
				TotalBytes: metadata.Manifest.TotalBytes, BytesProcessed: metadata.Manifest.TotalBytes})
		}
		return metadata.Manifest, manifestFileMap(metadata.Manifest), nil
	}
	var lastScanReport time.Time
	lastScanPhase := mordesync.ScanPhase("")
	manifest, err := mordesync.Scan(ctx, directory, func(scan mordesync.ScanProgress) {
		if report != nil {
			now := time.Now()
			if scan.Phase == lastScanPhase && now.Sub(lastScanReport) < 100*time.Millisecond {
				return
			}
			lastScanPhase = scan.Phase
			lastScanReport = now
			report(Progress{Stage: string(scan.Phase), FileCount: scan.FilesFound, FilesProcessed: scan.FilesHashed,
				TotalBytes: scan.BytesFound, BytesProcessed: scan.BytesHashed, CurrentPath: scan.CurrentPath})
		}
	})
	if err != nil {
		return mordesync.Manifest{}, nil, fmt.Errorf("scan local workspace: %w", err)
	}
	manifest = filterManifest(manifest, options.Scope)
	if err := mordesync.ValidateManifest(manifest); err != nil {
		return mordesync.Manifest{}, nil, fmt.Errorf("validate local workspace manifest: %w", err)
	}
	if report != nil {
		report(Progress{Stage: "planning", FileCount: manifest.FileCount, FilesProcessed: manifest.FileCount,
			TotalBytes: manifest.TotalBytes, BytesProcessed: manifest.TotalBytes})
	}
	return manifest, manifestFileMap(manifest), nil
}

func manifestFileMap(manifest mordesync.Manifest) map[string]mordesync.File {
	files := make(map[string]mordesync.File, len(manifest.Files))
	for _, file := range manifest.Files {
		files[strings.ToLower(file.Path)] = file
	}
	return files
}

// Cancel stops future transfer work and preserves all blocks and files already
// acknowledged by the server or applied locally.
func (c *Client) Cancel(ctx context.Context, workspaceID, sessionID, directory string) (SyncSession, error) {
	if ctx == nil || !workspaceIDPattern.MatchString(workspaceID) || !sessionIDPattern.MatchString(sessionID) || directory == "" {
		return SyncSession{}, errors.New("workspace ID, session ID, and local directory are required")
	}
	key, err := randomIdempotencyKey()
	if err != nil {
		return SyncSession{}, err
	}
	var session SyncSession
	err = c.requestJSON(ctx, http.MethodPost, c.sessionPath(workspaceID, sessionID)+"/cancel", key, nil, http.StatusOK, &session)
	if err == nil {
		err = validateSession(session)
	}
	if err == nil {
		canonical, pathErr := canonicalDirectory(directory)
		if pathErr != nil {
			err = pathErr
		} else {
			err = removeResumeMetadata(canonical, workspaceID, sessionID)
		}
	}
	return session, err
}

func (c *Client) validateOptions(ctx context.Context, options SyncOptions) error {
	if ctx == nil || !workspaceIDPattern.MatchString(options.WorkspaceID) || options.Directory == "" {
		return errors.New("context, valid workspace ID, and local directory are required")
	}
	if options.BaseRevision != nil && *options.BaseRevision < 0 {
		return errors.New("base revision cannot be negative")
	}
	if options.FolderID != "" && (!syncFolderIDPattern.MatchString(options.FolderID) || options.Direction != "upload" || options.Policy != "compare_merge" ||
		options.BaseRevision == nil || *options.BaseRevision != 0 || len(options.Scope) != 0 || len(options.DeletedPaths) != 0 ||
		len(options.DeletedDirectories) != 0 || options.AckCursor != nil || options.TaskID != "" || options.TaskHubOperationKey != "") {
		return errors.New("project folder sync requires an unscoped compare-merge upload from revision zero")
	}
	if err := mordesync.ValidateManifest(options.Baseline); err != nil {
		return fmt.Errorf("sync baseline is invalid: %w", err)
	}
	if err := mordesync.ValidateDirectoryPathList(options.DeletedDirectories); err != nil {
		return fmt.Errorf("deleted workspace directories: %w", err)
	}
	if options.IdempotencyKey != "" && !idempotencyPattern.MatchString(options.IdempotencyKey) {
		return errors.New("sync idempotency key is invalid")
	}
	if options.RetryCommitAfterQuota && options.SessionID == "" {
		return errors.New("quota commit retry requires an existing session")
	}
	if options.IdempotencyNamespace != "" && (len(options.IdempotencyNamespace) > 63 || !idempotencyPattern.MatchString(options.IdempotencyNamespace) || options.IdempotencyKey != "") {
		return errors.New("sync idempotency namespace is invalid")
	}
	if (options.TaskID == "") != (options.TaskHubOperationKey == "") {
		return errors.New("Task sync requires both task_id and task_hub_operation_key")
	}
	if options.TaskID != "" {
		if !taskIDPattern.MatchString(options.TaskID) || !taskOperationKey.MatchString(options.TaskHubOperationKey) {
			return errors.New("Task sync association is invalid")
		}
		if options.SessionID == "" && (options.BaseRevision == nil || options.Direction != "upload" || options.Policy != "compare_merge" ||
			len(options.Scope) != 1 || mordesync.ValidateDirectoryPathList(options.Scope) != nil ||
			len(options.DeletedPaths) != 0 || len(options.DeletedDirectories) != 0 || options.AckCursor != nil) {
			return errors.New("Task sync requires an upload of one scoped file from an explicit base revision")
		}
		if options.SessionID != "" && (options.Direction != "" && options.Direction != "upload" ||
			options.Policy != "" && options.Policy != "compare_merge" || options.Scope != nil && len(options.Scope) != 1) {
			return errors.New("Task sync resume options do not match a single-file upload")
		}
	}
	return nil
}

func validateTaskLinkedManifest(options SyncOptions, manifest mordesync.Manifest) error {
	if options.TaskID != "" && (len(manifest.Files) != 1 || len(manifest.EmptyDirectories) != 0 ||
		len(options.Scope) != 1 || manifest.Files[0].Path != options.Scope[0]) {
		return errors.New("Task sync manifest must contain only its exact scoped file")
	}
	for _, directory := range manifest.Directories {
		if options.TaskID != "" && !strings.HasPrefix(strings.ToLower(options.Scope[0]), strings.ToLower(directory.Path)+"/") {
			return errors.New("Task sync directory metadata must be limited to parent directories of its file")
		}
	}
	return nil
}

func (c *Client) latestRevision(ctx context.Context, workspaceID string) (int64, error) {
	page, err := c.manifestPage(ctx, workspaceID, 0, "", 1)
	if err != nil {
		return 0, err
	}
	return page.Revision, nil
}

func (c *Client) createSession(ctx context.Context, workspaceID, folderID, direction, policy string, baseRevision int64, scope []string, manifest mordesync.Manifest, deletedPaths, deletedDirectories []string, ackCursor *string, taskID, taskOperationKey, key string) (SyncSession, error) {
	data, err := createSessionPayload(folderID, direction, policy, baseRevision, scope, manifest, deletedPaths, deletedDirectories, ackCursor, taskID, taskOperationKey)
	if err != nil {
		return SyncSession{}, err
	}
	var session SyncSession
	path := c.workspacePath(workspaceID) + "/sync-sessions"
	err = c.requestJSON(ctx, http.MethodPost, path, key, data, http.StatusCreated, &session)
	return session, err
}

func createSessionPayload(folderID, direction, policy string, baseRevision int64, scope []string, manifest mordesync.Manifest, deletedPaths, deletedDirectories []string, ackCursor *string, taskID, taskOperationKey string) ([]byte, error) {
	files := make([]workspaceFile, 0, len(manifest.Files))
	for _, file := range manifest.Files {
		files = append(files, toWorkspaceFile(file))
	}
	request := createSessionRequest{FolderID: folderID, Direction: direction, Policy: policy, BaseRevision: baseRevision, Scope: scope, Files: files,
		EmptyDirectories: append([]string{}, manifest.EmptyDirectories...), Directories: make([]workspaceDirectory, 0, len(manifest.Directories)), DeletedPaths: append([]string(nil), deletedPaths...),
		DeletedDirectories: append([]string(nil), deletedDirectories...), AckCursor: cloneStringPointer(ackCursor),
		TaskID: taskID, TaskHubOperationKey: taskOperationKey}
	for _, directory := range manifest.Directories {
		request.Directories = append(request.Directories, toWorkspaceDirectory(directory))
	}
	data, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode sync manifest: %w", err)
	}
	if len(data) > maxJSONBytes {
		return nil, errors.New("sync manifest exceeds the 16 MiB API request limit")
	}
	return data, nil
}

func derivedCreateKey(namespace, portalURL, directory, workspaceID, folderID, direction, policy string, baseRevision int64, scope []string, manifest mordesync.Manifest, deletedPaths, deletedDirectories []string, ackCursor *string, taskID, taskOperationKey string) (string, error) {
	request, err := createSessionPayload(folderID, direction, policy, baseRevision, scope, manifest, deletedPaths, deletedDirectories, ackCursor, taskID, taskOperationKey)
	if err != nil {
		return "", err
	}
	// Portal and canonical local directory are part of the local operation
	// identity. Neither path is sent to the server; only this digest is.
	digest := sha256.Sum256(append([]byte(portalURL+"\x00"+directory+"\x00"+workspaceID+"\x00"), request...))
	return namespace + "." + hex.EncodeToString(digest[:]), nil
}

func (c *Client) getSession(ctx context.Context, workspaceID, sessionID string) (SyncSession, error) {
	var session SyncSession
	err := c.requestJSON(ctx, http.MethodGet, c.sessionPath(workspaceID, sessionID), "", nil, http.StatusOK, &session)
	return session, err
}

func (c *Client) commit(ctx context.Context, workspaceID, sessionID, key string) (SyncSession, error) {
	var session SyncSession
	err := c.requestJSON(ctx, http.MethodPost, c.sessionPath(workspaceID, sessionID)+"/commit", key, nil, http.StatusOK, &session)
	return session, err
}

func (c *Client) uploadBlocks(ctx context.Context, workspaceID, directory string, local mordesync.Manifest, blocks map[string]TransferBlock, progress *progressReporter) error {
	refs := make([]mordesync.BlockRef, 0)
	for _, transfer := range blocks {
		if transfer.Role != "upload" || transfer.Received {
			continue
		}
		digest := transfer.Chunk.SHA256
		found := false
		for _, file := range local.Files {
			for index, block := range file.Blocks {
				if block.SHA256 == digest {
					if block.Size != transfer.Chunk.SizeBytes {
						return fmt.Errorf("upload block %s size does not match local manifest", digest)
					}
					ref := mordesync.BlockRef{File: file, Index: index}
					refs = append(refs, ref)
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			return fmt.Errorf("session requires upload block %s that is absent from the local workspace", digest)
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		left := refs[i].File.Blocks[refs[i].Index].SHA256
		right := refs[j].File.Blocks[refs[j].Index].SHA256
		return left < right
	})
	return mordesync.ReadBlocks(ctx, directory, refs, func(file mordesync.File, _ int, block mordesync.Block, data []byte) error {
		progress.setPath(file.Path)
		transfer, required := blocks[blockKey("upload", block.SHA256)]
		if !required || int64(len(data)) != transfer.Chunk.SizeBytes {
			return errors.New("local upload block does not match the server transfer plan")
		}
		if err := c.rawRequest(ctx, http.MethodPut, c.blockPath(workspaceID, progress.session.ID, block.SHA256), bytes.NewReader(data), "application/octet-stream", http.StatusNoContent); err != nil {
			return err
		}
		progress.add(block.Size)
		return nil
	})
}

func (c *Client) downloadMissing(ctx context.Context, options SyncOptions, session SyncSession, initialLocal map[string]mordesync.File,
	initialLocalMetadata []mordesync.Directory, initialLocalDirectories []string, blocks map[string]TransferBlock, actions map[string]DownloadAction, progress *progressReporter) ([]Conflict, error) {
	remote, err := c.manifest(ctx, options.WorkspaceID, session.TargetRevision)
	if err != nil {
		return nil, err
	}
	remoteByPath := manifestFileMap(remote)
	remoteDirectoryPaths := manifestDirectoryPathSet(remote)
	remoteDirectoryMetadata := mordesync.DirectoryMetadataMap(remote.Directories)
	remoteEmptyDirectories := emptyDirectoryMap(remote)
	filtered := mordesync.Manifest{Files: make([]mordesync.File, 0, len(actions)), EmptyDirectories: make([]string, 0, len(actions)), Directories: make([]mordesync.Directory, 0, len(actions))}
	baselineByPath := manifestFileMap(options.Baseline)
	baselineDirectories := emptyDirectoryMap(options.Baseline)
	initialDirectorySet := make(map[string]struct{}, len(initialLocalDirectories))
	for _, directory := range initialLocalDirectories {
		initialDirectorySet[strings.ToLower(directory)] = struct{}{}
	}
	initialDirectoryMetadata := mordesync.DirectoryMetadataMap(initialLocalMetadata)
	for key := range initialDirectoryMetadata {
		initialDirectorySet[key] = struct{}{}
	}
	changedDirectories := make(map[string]struct{})
	createdDirectories := make(map[string]struct{})
	emptyDirectoryReady := make(map[string]struct{}, len(filtered.EmptyDirectories))
	var conflicts []Conflict
	var pathErrors []error
	pathError := func(path string, err error) {
		pathErrors = append(pathErrors, fmt.Errorf("%s: %w", path, err))
	}
	returnErrors := func(err error) error {
		return errors.Join(errors.Join(pathErrors...), err)
	}
	for _, action := range session.DownloadActions {
		key := strings.ToLower(action.Path)
		if action.Action == "delete" {
			if _, exists := remoteByPath[key]; exists {
				return conflicts, returnErrors(fmt.Errorf("delete action for %q conflicts with the pinned workspace manifest", action.Path))
			}
			if len(options.Scope) > 0 && !inScope(action.Path, options.Scope) {
				return conflicts, returnErrors(fmt.Errorf("delete action for %q is outside the requested scope", action.Path))
			}
			if action.Applied {
				continue
			}
			baseline, ok := baselineByPath[key]
			if session.Policy != "live" || !ok {
				return conflicts, returnErrors(fmt.Errorf("delete action for %q has no verified live baseline", action.Path))
			}
			progress.setPath(action.Path)
			actualHash, removed, removeErr := removeIfHash(ctx, options.Directory, options.WorkspaceID, baseline)
			if removeErr != nil {
				conflicts = append(conflicts, Conflict{Path: action.Path, Reason: "destination_exists", LocalSHA256: optionalHash(actualHash)})
				if ctx.Err() != nil {
					return conflicts, returnErrors(fmt.Errorf("apply remote deletion for %q: %w", action.Path, removeErr))
				}
				pathError(action.Path, fmt.Errorf("apply remote deletion: %w", removeErr))
				continue
			}
			if !removed {
				conflicts = append(conflicts, Conflict{Path: action.Path, Reason: "destination_exists", LocalSHA256: optionalHash(actualHash)})
				pathError(action.Path, fmt.Errorf("local file changed after the live plan; it was preserved: %w", mordesync.ErrDestinationChange))
				continue
			}
			for _, parent := range parentDirectoryPaths(action.Path) {
				changedDirectories[parent] = struct{}{}
			}
			if err := c.ackDownloadAction(ctx, options.WorkspaceID, session.ID, action); err != nil {
				return conflicts, returnErrors(err)
			}
			continue
		}
		if action.Action == "directory_delete" {
			if _, exists := remoteByPath[key]; exists {
				return conflicts, returnErrors(fmt.Errorf("directory delete action for %q conflicts with the pinned workspace manifest", action.Path))
			}
			if _, exists := remoteDirectoryPaths[key]; exists {
				return conflicts, returnErrors(fmt.Errorf("directory delete action for %q conflicts with the pinned workspace manifest", action.Path))
			}
			if len(options.Scope) > 0 && !inScope(action.Path, options.Scope) {
				return conflicts, returnErrors(fmt.Errorf("directory delete action for %q is outside the requested scope", action.Path))
			}
			baseline, ok := baselineDirectories[key]
			if session.Policy != "live" || !ok || baseline != action.Path {
				return conflicts, returnErrors(fmt.Errorf("directory delete action for %q has no verified live baseline", action.Path))
			}
			status, statusErr := localEmptyDirectoryStatus(ctx, options.Directory, action.Path)
			if statusErr != nil {
				return conflicts, returnErrors(statusErr)
			}
			if action.Applied {
				if status != mordesync.ApplyMissingStatus {
					conflicts = append(conflicts, Conflict{Path: action.Path, Reason: "destination_exists"})
					pathError(action.Path, errors.New("directory changed after its deletion was acknowledged; it was preserved"))
					continue
				}
			} else {
				progress.setPath(action.Path)
				removed, removeErr := mordesync.RemoveEmptyDirectory(ctx, options.Directory, action.Path)
				if removeErr != nil {
					if ctx.Err() != nil {
						return conflicts, returnErrors(fmt.Errorf("remove remote empty directory %q: %w", action.Path, removeErr))
					}
					pathError(action.Path, fmt.Errorf("remove remote empty directory: %w", removeErr))
					continue
				}
				if removed.Status == mordesync.ApplyConflict {
					conflicts = append(conflicts, Conflict{Path: action.Path, Reason: "destination_exists"})
					pathError(action.Path, errors.New("local directory is no longer empty; it was preserved"))
					continue
				}
			}
			if err := c.ackDownloadAction(ctx, options.WorkspaceID, session.ID, action); err != nil {
				return conflicts, returnErrors(err)
			}
			for _, parent := range parentDirectoryPaths(action.Path) {
				changedDirectories[parent] = struct{}{}
			}
			continue
		}
		if action.Action == "directory_upsert" {
			if _, exists := remoteDirectoryPaths[key]; !exists || !mordesync.InDirectoryScope(action.Path, options.Scope) {
				return conflicts, returnErrors(fmt.Errorf("directory upsert action for %q does not match the pinned workspace manifest and scope", action.Path))
			}
			if directory, exists := remoteEmptyDirectories[key]; exists {
				filtered.EmptyDirectories = append(filtered.EmptyDirectories, directory)
			}
			if directory, exists := remoteDirectoryMetadata[key]; exists {
				if directory.Path != action.Path {
					return conflicts, returnErrors(fmt.Errorf("directory metadata action for %q has a noncanonical path", action.Path))
				}
				filtered.Directories = append(filtered.Directories, directory)
			}
			continue
		}
		file, exists := remoteByPath[key]
		if !exists || file.Path != action.Path || action.SHA256 == nil || file.SHA256 != *action.SHA256 || file.Size != action.SizeBytes || (len(options.Scope) > 0 && !inScope(file.Path, options.Scope)) {
			return conflicts, returnErrors(fmt.Errorf("download action for %q does not match the pinned workspace manifest and scope", action.Path))
		}
		// An earlier acknowledgement does not prove the local path stayed intact while the session was interrupted.
		filtered.Files = append(filtered.Files, file)
		filtered.TotalBytes += file.Size
	}
	filtered.FileCount = int64(len(filtered.Files))
	if err := mordesync.ValidateManifestSubset(filtered); err != nil {
		return conflicts, returnErrors(fmt.Errorf("validate pinned download actions: %w", err))
	}
	plan, err := mordesync.PlanMissing(ctx, options.Directory, filtered)
	if err != nil {
		return conflicts, returnErrors(err)
	}
	statusByPath := make(map[string]mordesync.PlanEntry, len(plan.Entries))
	for _, entry := range plan.Entries {
		statusByPath[strings.ToLower(entry.Path)] = entry
	}
	directoryStatusByPath := make(map[string]mordesync.PlanEntry, len(plan.DirectoryEntries))
	for _, entry := range plan.DirectoryEntries {
		directoryStatusByPath[strings.ToLower(entry.Path)] = entry
	}
	for _, file := range filtered.Files {
		entry := statusByPath[strings.ToLower(file.Path)]
		key := strings.ToLower(file.Path)
		baseline, hasBaseline := baselineByPath[key]
		var metadataBaseline *mordesync.File
		if session.Policy == "live" && hasBaseline {
			metadataBaseline = &baseline
		}
		action := actions[key]
		fileChanged := false
		removedBaselineDirectory := false
		if entry.Status == mordesync.ApplyConflict {
			if baselineDirectory, hasBaselineDirectory := baselineDirectories[key]; hasBaselineDirectory {
				_, wasPresentWhenSessionStarted := initialDirectorySet[key]
				if session.Policy != "live" || baselineDirectory != file.Path || !wasPresentWhenSessionStarted || action.Applied {
					conflicts = append(conflicts, Conflict{Path: file.Path, Reason: "destination_exists", RemoteSHA256: optionalHash(file.SHA256)})
					pathError(file.Path, errors.New("destination directory differs from its live baseline; it was preserved"))
					continue
				}
				progress.setPath(file.Path)
				removed, removeErr := mordesync.RemoveEmptyDirectory(ctx, options.Directory, file.Path)
				if removeErr != nil {
					if mustStopDownload(ctx, removeErr) {
						return conflicts, returnErrors(fmt.Errorf("remove live baseline directory %q: %w", file.Path, removeErr))
					}
					pathError(file.Path, fmt.Errorf("remove live baseline directory: %w", removeErr))
					continue
				}
				if removed.Status != mordesync.ApplyWritten {
					conflicts = append(conflicts, Conflict{Path: file.Path, Reason: "destination_exists", RemoteSHA256: optionalHash(file.SHA256)})
					pathError(file.Path, errors.New("baseline directory disappeared before the live replacement; it was preserved"))
					continue
				}
				entry.Status = mordesync.ApplyMissingStatus
				removedBaselineDirectory = true
			} else {
				baseline, hasBaseline := baselineByPath[key]
				if session.Policy != "live" || !hasBaseline || entry.ExistingSHA256 != baseline.SHA256 {
					conflicts = append(conflicts, Conflict{Path: file.Path, Reason: "destination_exists", LocalSHA256: optionalHash(entry.ExistingSHA256), RemoteSHA256: optionalHash(file.SHA256)})
					pathError(file.Path, errors.New("destination differs from its live baseline; it was preserved"))
					continue
				}
				for _, chunk := range file.Blocks {
					transfer, ok := blocks[blockKey("download", chunk.SHA256)]
					if !ok || transfer.Chunk.SizeBytes != chunk.Size {
						return conflicts, returnErrors(fmt.Errorf("session is missing download block %s required by %q", chunk.SHA256, file.Path))
					}
				}
				progress.setPath(file.Path)
				reader := &remoteFileReader{client: c, ctx: ctx, workspaceID: options.WorkspaceID, sessionID: session.ID, blocks: file.Blocks}
				actualHash, replaceErr := replaceIfHash(ctx, options.Directory, options.WorkspaceID, baseline, file, reader)
				if replaceErr != nil {
					if mustStopDownload(ctx, replaceErr) {
						return conflicts, returnErrors(fmt.Errorf("apply remote update for %q: %w", file.Path, replaceErr))
					}
					if errors.Is(replaceErr, mordesync.ErrDestinationChange) {
						conflicts = append(conflicts, Conflict{Path: file.Path, Reason: "destination_exists", LocalSHA256: optionalHash(actualHash), RemoteSHA256: optionalHash(file.SHA256)})
					}
					pathError(file.Path, fmt.Errorf("apply remote update: %w", replaceErr))
					continue
				}
				fileChanged = true
				for _, parent := range parentDirectoryPaths(file.Path) {
					changedDirectories[parent] = struct{}{}
				}
				if err := c.ackFileBlocks(ctx, options.WorkspaceID, session.ID, file, blocks, progress); err != nil {
					return conflicts, returnErrors(err)
				}
				if err := c.ackDownloadAction(ctx, options.WorkspaceID, session.ID, action); err != nil {
					return conflicts, returnErrors(err)
				}
				continue
			}
		}
		if entry.Status == mordesync.ApplyPresent {
			if err := syncVerifiedExisting(ctx, options.Directory, file, metadataBaseline); err != nil {
				if mustStopDownload(ctx, err) {
					return conflicts, returnErrors(fmt.Errorf("verify and sync existing destination %q: %w", file.Path, err))
				}
				pathError(file.Path, fmt.Errorf("verify and sync existing destination: %w", err))
				continue
			}
		} else if entry.Status == mordesync.ApplyMissingStatus {
			if _, wasPresentWhenSessionStarted := initialLocal[strings.ToLower(file.Path)]; wasPresentWhenSessionStarted {
				pathError(file.Path, errors.New("destination disappeared after the session was planned; session remains resumable"))
				continue
			}
			if _, wasPresentWhenSessionStarted := initialDirectorySet[key]; wasPresentWhenSessionStarted && !removedBaselineDirectory {
				pathError(file.Path, errors.New("destination directory disappeared after the session was planned; session remains resumable"))
				continue
			}
			for _, chunk := range file.Blocks {
				transfer, ok := blocks[blockKey("download", chunk.SHA256)]
				if !ok || transfer.Chunk.SizeBytes != chunk.Size {
					return conflicts, returnErrors(fmt.Errorf("session is missing download block %s required by %q", chunk.SHA256, file.Path))
				}
			}
			progress.setPath(file.Path)
			reader := &remoteFileReader{client: c, ctx: ctx, workspaceID: options.WorkspaceID, sessionID: session.ID, blocks: file.Blocks}
			applied, err := mordesync.ApplyMissing(ctx, options.Directory, file, reader)
			if err != nil {
				if mustStopDownload(ctx, err) {
					return conflicts, returnErrors(fmt.Errorf("apply remote file %q: %w", file.Path, err))
				}
				if errors.Is(err, mordesync.ErrDestinationChange) {
					conflicts = append(conflicts, Conflict{Path: file.Path, Reason: "destination_exists", RemoteSHA256: optionalHash(file.SHA256)})
				}
				pathError(file.Path, fmt.Errorf("apply remote file: %w", err))
				continue
			}
			if applied.Status == mordesync.ApplyConflict {
				conflicts = append(conflicts, Conflict{Path: file.Path, Reason: "destination_exists", LocalSHA256: optionalHash(applied.ExistingSHA256), RemoteSHA256: optionalHash(file.SHA256)})
				pathError(file.Path, errors.New("destination changed while applying; session remains resumable"))
				continue
			}
			if applied.Status == mordesync.ApplyPresent {
				if err := syncVerifiedExisting(ctx, options.Directory, file, metadataBaseline); err != nil {
					if mustStopDownload(ctx, err) {
						return conflicts, returnErrors(fmt.Errorf("verify and sync existing destination %q: %w", file.Path, err))
					}
					pathError(file.Path, fmt.Errorf("verify and sync existing destination: %w", err))
					continue
				}
			} else if applied.Status != mordesync.ApplyWritten {
				return conflicts, returnErrors(fmt.Errorf("destination %q has an unsupported apply result %q", file.Path, applied.Status))
			} else {
				fileChanged = true
			}
		} else {
			return conflicts, returnErrors(fmt.Errorf("destination %q has an unsupported local state %q", file.Path, entry.Status))
		}
		if err := c.ackFileBlocks(ctx, options.WorkspaceID, session.ID, file, blocks, progress); err != nil {
			return conflicts, returnErrors(err)
		}
		if fileChanged {
			for _, parent := range parentDirectoryPaths(file.Path) {
				changedDirectories[parent] = struct{}{}
				if _, existed := initialDirectorySet[parent]; !existed && entry.Status == mordesync.ApplyMissingStatus {
					createdDirectories[parent] = struct{}{}
				}
			}
		}
		if err := c.ackDownloadAction(ctx, options.WorkspaceID, session.ID, action); err != nil {
			return conflicts, returnErrors(err)
		}
	}
	for _, directory := range filtered.EmptyDirectories {
		key := strings.ToLower(directory)
		entry := directoryStatusByPath[key]
		action := actions[key]
		if action.Action != "directory_upsert" || action.Path != directory {
			return conflicts, returnErrors(fmt.Errorf("directory %q has no matching server upsert action", directory))
		}
		if entry.Status == mordesync.ApplyConflict {
			baseline, hasBaseline := baselineByPath[key]
			_, wasPresentWhenSessionStarted := initialLocal[key]
			if session.Policy != "live" || !hasBaseline || baseline.Path != directory || !wasPresentWhenSessionStarted || action.Applied {
				conflicts = append(conflicts, Conflict{Path: directory, Reason: "destination_exists"})
				pathError(directory, errors.New("destination is not an empty baseline file; it was preserved"))
				continue
			}
			progress.setPath(directory)
			actualHash, removed, removeErr := removeIfHash(ctx, options.Directory, options.WorkspaceID, baseline)
			if removeErr != nil || !removed {
				conflicts = append(conflicts, Conflict{Path: directory, Reason: "destination_exists", LocalSHA256: optionalHash(actualHash)})
				if mustStopDownload(ctx, removeErr) {
					return conflicts, returnErrors(fmt.Errorf("remove live baseline file %q for directory replacement: %w", directory, removeErr))
				}
				if removeErr != nil {
					pathError(directory, fmt.Errorf("remove live baseline file for directory replacement: %w", removeErr))
				} else {
					pathError(directory, errors.New("baseline file changed before directory replacement; it was preserved"))
				}
				continue
			}
			entry.Status = mordesync.ApplyMissingStatus
		} else if entry.Status == mordesync.ApplyMissingStatus {
			if baseline, hadBaselineFile := baselineByPath[key]; hadBaselineFile {
				if session.Policy != "live" || baseline.Path != directory {
					pathError(directory, errors.New("baseline file disappeared before directory replacement; session remains resumable"))
					continue
				}
				preserved, preserveErr := hasPreservedBaseline(ctx, options.Directory, options.WorkspaceID, baseline)
				if preserveErr != nil {
					return conflicts, returnErrors(fmt.Errorf("verify preserved file before directory replacement: %w", preserveErr))
				}
				if !preserved {
					pathError(directory, errors.New("baseline file disappeared without a verified recovery copy; directory replacement is paused"))
					continue
				}
			} else if _, wasPresent := initialLocal[key]; wasPresent {
				pathError(directory, errors.New("destination file disappeared after the session was planned; session remains resumable"))
				continue
			}
			if _, wasPresent := initialDirectorySet[key]; wasPresent {
				pathError(directory, errors.New("destination directory disappeared after the session was planned; session remains resumable"))
				continue
			}
			if _, hadBaselineDirectory := baselineDirectories[key]; hadBaselineDirectory {
				pathError(directory, errors.New("live baseline directory was already missing when the session was planned; it was preserved"))
				continue
			}
		} else if entry.Status != mordesync.ApplyPresent {
			return conflicts, returnErrors(fmt.Errorf("destination directory %q has an unsupported local state %q", directory, entry.Status))
		}
		progress.setPath(directory)
		applied, applyErr := mordesync.EnsureEmptyDirectory(ctx, options.Directory, directory)
		if applyErr != nil {
			if mustStopDownload(ctx, applyErr) {
				return conflicts, returnErrors(fmt.Errorf("create remote empty directory %q: %w", directory, applyErr))
			}
			pathError(directory, fmt.Errorf("create remote empty directory: %w", applyErr))
			continue
		}
		if applied.Status == mordesync.ApplyConflict {
			conflicts = append(conflicts, Conflict{Path: directory, Reason: "destination_exists"})
			pathError(directory, errors.New("destination changed while creating the empty directory; it was preserved"))
			continue
		}
		if applied.Status == mordesync.ApplyWritten {
			for _, parent := range parentDirectoryPaths(directory) {
				createdDirectories[parent] = struct{}{}
			}
			createdDirectories[strings.ToLower(directory)] = struct{}{}
		}
		emptyDirectoryReady[key] = struct{}{}
	}
	sort.Slice(filtered.Directories, func(i, j int) bool {
		if filtered.Directories[i].Path == "." {
			return false
		}
		if filtered.Directories[j].Path == "." {
			return true
		}
		depthI, depthJ := strings.Count(filtered.Directories[i].Path, "/"), strings.Count(filtered.Directories[j].Path, "/")
		if depthI != depthJ {
			return depthI > depthJ
		}
		return filtered.Directories[i].Path < filtered.Directories[j].Path
	})
	for _, desired := range filtered.Directories {
		key := strings.ToLower(desired.Path)
		if _, empty := remoteEmptyDirectories[key]; empty {
			if _, ready := emptyDirectoryReady[key]; !ready {
				continue
			}
		}
		action := actions[key]
		if action.Action != "directory_upsert" || action.Path != desired.Path {
			return conflicts, returnErrors(fmt.Errorf("directory metadata %q has no matching server action", desired.Path))
		}
		expected, hadExpected := initialDirectoryMetadata[key]
		if _, changed := changedDirectories[key]; changed {
			expected.Path = desired.Path
			expected.ModTimeKnown = false
			hadExpected = true
		}
		if action.Applied {
			expected, hadExpected = desired, true
		}
		var expectedPtr *mordesync.Directory
		if hadExpected {
			expectedPtr = &expected
		}
		progress.setPath(desired.Path)
		result, applyErr := mordesync.ApplyDirectoryMetadata(ctx, options.Directory, expectedPtr, desired, hasString(createdDirectories, key))
		if applyErr != nil {
			pathError(desired.Path, fmt.Errorf("apply directory metadata: %w", applyErr))
			continue
		}
		if result.Status == mordesync.ApplyConflict {
			conflicts = append(conflicts, Conflict{Path: desired.Path, Reason: "destination_exists"})
			pathError(desired.Path, errors.New("local directory changed since the session was planned; it was preserved"))
			continue
		}
		if err := c.ackDownloadAction(ctx, options.WorkspaceID, session.ID, action); err != nil {
			return conflicts, returnErrors(err)
		}
	}
	for key := range changedDirectories {
		if _, actioned := actions[key]; actioned {
			continue
		}
		desired, exists := remoteDirectoryMetadata[key]
		if !exists || !desired.ModTimeKnown {
			continue
		}
		expected, hadExpected := initialDirectoryMetadata[key]
		if hadExpected {
			expected.ModTimeKnown = false
		}
		var expectedPtr *mordesync.Directory
		if hadExpected {
			expectedPtr = &expected
		}
		progress.setPath(desired.Path)
		result, applyErr := mordesync.ApplyDirectoryMetadata(ctx, options.Directory, expectedPtr, desired, hasString(createdDirectories, key))
		if applyErr != nil {
			pathError(desired.Path, fmt.Errorf("restore directory metadata after file changes: %w", applyErr))
		} else if result.Status == mordesync.ApplyConflict {
			conflicts = append(conflicts, Conflict{Path: desired.Path, Reason: "destination_exists"})
			pathError(desired.Path, errors.New("local directory changed since the session was planned; it was preserved"))
		}
	}
	for _, directory := range filtered.EmptyDirectories {
		key := strings.ToLower(directory)
		if _, ready := emptyDirectoryReady[key]; !ready {
			continue
		}
		if _, hasMetadata := remoteDirectoryMetadata[key]; hasMetadata {
			continue
		}
		action := actions[key]
		if err := c.ackDownloadAction(ctx, options.WorkspaceID, session.ID, action); err != nil {
			return conflicts, returnErrors(err)
		}
	}
	return conflicts, errors.Join(pathErrors...)
}

func hasString(values map[string]struct{}, value string) bool {
	_, exists := values[strings.ToLower(value)]
	return exists
}

func parentDirectoryPaths(value string) []string {
	parts := strings.Split(value, "/")
	result := make([]string, 1, len(parts))
	result[0] = "."
	prefix := ""
	for index, part := range parts[:len(parts)-1] {
		if index > 0 {
			prefix += "/"
		}
		prefix += part
		result = append(result, strings.ToLower(prefix))
	}
	return result
}

func localEmptyDirectoryStatus(ctx context.Context, directory, relative string) (mordesync.ApplyStatus, error) {
	plan, err := mordesync.PlanMissing(ctx, directory, mordesync.Manifest{EmptyDirectories: []string{relative}})
	if err != nil {
		return "", err
	}
	if len(plan.DirectoryEntries) != 1 || plan.DirectoryEntries[0].Path != relative {
		return "", errors.New("could not classify local empty directory")
	}
	return plan.DirectoryEntries[0].Status, nil
}

func mustStopDownload(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, mordesync.ErrContentMismatch) {
		return true
	}
	var remoteErr *remoteDownloadError
	return errors.As(err, &remoteErr)
}

func (c *Client) ackFileBlocks(ctx context.Context, workspaceID, sessionID string, file mordesync.File, blocks map[string]TransferBlock, progress *progressReporter) error {
	seen := make(map[string]struct{}, len(file.Blocks))
	for _, chunk := range file.Blocks {
		if _, duplicate := seen[chunk.SHA256]; duplicate {
			continue
		}
		seen[chunk.SHA256] = struct{}{}
		key := blockKey("download", chunk.SHA256)
		transfer, required := blocks[key]
		if !required || transfer.Chunk.SizeBytes != chunk.Size || transfer.Received {
			continue
		}
		if err := c.ackDownload(ctx, workspaceID, sessionID, chunk.SHA256); err != nil {
			return err
		}
		transfer.Received = true
		blocks[key] = transfer
		progress.add(chunk.Size)
	}
	return nil
}

func (c *Client) ackDownload(ctx context.Context, workspaceID, sessionID, digest string) error {
	return c.rawRequest(ctx, http.MethodPost, c.blockPath(workspaceID, sessionID, digest)+"/ack", nil, "", http.StatusNoContent)
}

func (c *Client) ackDownloadAction(ctx context.Context, workspaceID, sessionID string, action DownloadAction) error {
	body, err := json.Marshal(struct {
		Path      string  `json:"path"`
		Action    string  `json:"action"`
		SHA256    *string `json:"sha256"`
		SizeBytes int64   `json:"size_bytes"`
	}{Path: action.Path, Action: action.Action, SHA256: action.SHA256, SizeBytes: action.SizeBytes})
	if err != nil {
		return fmt.Errorf("encode download action acknowledgement: %w", err)
	}
	return c.rawRequest(ctx, http.MethodPost, c.sessionPath(workspaceID, sessionID)+"/download-actions/ack", bytes.NewReader(body), "application/json", http.StatusNoContent)
}

type remoteFileReader struct {
	client      *Client
	ctx         context.Context
	workspaceID string
	sessionID   string
	blocks      []mordesync.Block
	index       int
	current     *bytes.Reader
}

type remoteDownloadError struct{ err error }

func (e *remoteDownloadError) Error() string { return e.err.Error() }
func (e *remoteDownloadError) Unwrap() error { return e.err }

func (r *remoteFileReader) Read(buffer []byte) (int, error) {
	for {
		if r.current != nil {
			n, err := r.current.Read(buffer)
			if !errors.Is(err, io.EOF) {
				return n, err
			}
			r.current = nil
			if n > 0 {
				return n, nil
			}
		}
		if r.index >= len(r.blocks) {
			return 0, io.EOF
		}
		block := r.blocks[r.index]
		r.index++
		if !digestPattern.MatchString(block.SHA256) || block.Size <= 0 || block.Size > mordesync.BlockSize {
			return 0, &remoteDownloadError{err: errors.New("remote manifest contains an invalid block")}
		}
		data, err := r.client.getBlock(r.ctx, r.workspaceID, r.sessionID, block.SHA256, block.Size)
		if err != nil {
			return 0, &remoteDownloadError{err: err}
		}
		r.current = bytes.NewReader(data)
	}
}

func (c *Client) getBlock(ctx context.Context, workspaceID, sessionID, digest string, size int64) ([]byte, error) {
	resp, err := c.request(ctx, http.MethodGet, c.blockPath(workspaceID, sessionID, digest), nil, "", "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpError(resp, http.MethodGet)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, size+1))
	if err != nil {
		return nil, fmt.Errorf("read downloaded workspace block: %w", err)
	}
	if int64(len(data)) != size || sumSHA256(data) != digest {
		return nil, errors.New("downloaded workspace block failed size or SHA-256 verification")
	}
	return data, nil
}

func (c *Client) manifest(ctx context.Context, workspaceID string, revision int64) (mordesync.Manifest, error) {
	if revision == 0 {
		return mordesync.Manifest{Files: []mordesync.File{}, EmptyDirectories: []string{}}, nil
	}
	cursor := ""
	seenCursors := make(map[string]struct{})
	files := make([]mordesync.File, 0)
	emptyDirectories := make([]string, 0)
	directories := make([]mordesync.Directory, 0)
	directoryByPath := make(map[string]mordesync.Directory)
	var totalBytes int64
	var expectedRevision int64
	for {
		page, err := c.manifestPage(ctx, workspaceID, revision, cursor, manifestPageSize)
		if err != nil {
			return mordesync.Manifest{}, err
		}
		if expectedRevision == 0 {
			expectedRevision = page.Revision
		}
		if page.Revision != expectedRevision || page.Revision != revision {
			return mordesync.Manifest{}, errors.New("manifest pagination changed revision")
		}
		for _, file := range page.Files {
			coreFile, err := toCoreFile(file)
			if err != nil {
				return mordesync.Manifest{}, err
			}
			if coreFile.Size > int64(^uint64(0)>>1)-totalBytes {
				return mordesync.Manifest{}, errors.New("workspace manifest byte count overflows")
			}
			totalBytes += coreFile.Size
			files = append(files, coreFile)
			if len(files) > maxManifestFiles {
				return mordesync.Manifest{}, errors.New("workspace manifest exceeds the file limit")
			}
		}
		for _, directory := range page.EmptyDirectories {
			emptyDirectories = append(emptyDirectories, directory)
			if len(files)+len(emptyDirectories) > mordesync.MaxManifestEntries {
				return mordesync.Manifest{}, errors.New("workspace manifest exceeds the item limit")
			}
		}
		for _, directory := range page.Directories {
			coreDirectory, err := toCoreDirectory(directory)
			if err != nil {
				return mordesync.Manifest{}, err
			}
			key := strings.ToLower(coreDirectory.Path)
			if prior, exists := directoryByPath[key]; exists {
				if prior.Path != coreDirectory.Path || prior.ModeKnown && coreDirectory.ModeKnown && prior.Mode != coreDirectory.Mode ||
					prior.ModTimeKnown && coreDirectory.ModTimeKnown && prior.ModTimeNS != coreDirectory.ModTimeNS {
					return mordesync.Manifest{}, errors.New("manifest pagination returned conflicting directory metadata")
				}
				continue
			}
			directories = append(directories, coreDirectory)
			directoryByPath[key] = coreDirectory
			if len(directories) > mordesync.MaxManifestEntries {
				return mordesync.Manifest{}, errors.New("workspace manifest exceeds directory limit")
			}
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		cursor = *page.NextCursor
		if _, duplicate := seenCursors[cursor]; duplicate {
			return mordesync.Manifest{}, errors.New("manifest pagination cursor repeated")
		}
		seenCursors[cursor] = struct{}{}
	}
	manifest := mordesync.Manifest{FileCount: int64(len(files)), TotalBytes: totalBytes, Files: files, EmptyDirectories: emptyDirectories, Directories: directories}
	if err := mordesync.ValidateManifest(manifest); err != nil {
		return mordesync.Manifest{}, fmt.Errorf("validate server workspace manifest: %w", err)
	}
	return manifest, nil
}

func (c *Client) manifestPage(ctx context.Context, workspaceID string, revision int64, cursor string, limit int) (workspaceManifest, error) {
	endpoint := c.workspacePath(workspaceID) + "/manifest"
	query := url.Values{"limit": []string{fmt.Sprint(limit)}}
	if revision > 0 {
		query.Set("revision", fmt.Sprint(revision))
	}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	var page workspaceManifest
	err := c.requestJSON(ctx, http.MethodGet, endpoint+"?"+query.Encode(), "", nil, http.StatusOK, &page)
	if err == nil && (page.Revision < 0 || page.WorkspaceID != workspaceID || len(page.Files)+len(page.EmptyDirectories) > limit || (page.NextCursor != nil && len(*page.NextCursor) > 512)) {
		err = errors.New("Morde API returned an invalid manifest page")
	}
	return page, err
}

func (c *Client) requestJSON(ctx context.Context, method, endpoint, idempotencyKey string, body []byte, expected int, destination any) error {
	contentType := "application/json"
	if method == http.MethodGet {
		contentType = ""
	}
	resp, err := c.request(ctx, method, endpoint, bytes.NewReader(body), contentType, idempotencyKey)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != expected {
		return httpError(resp, method)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes+1))
	if err != nil {
		return fmt.Errorf("read Morde API response: %w", err)
	}
	if len(data) > maxJSONBytes {
		return errors.New("Morde API JSON response exceeds the 16 MiB limit")
	}
	if err := json.Unmarshal(data, destination); err != nil {
		return fmt.Errorf("decode Morde API response: %w", err)
	}
	return nil
}

func (c *Client) rawRequest(ctx context.Context, method, endpoint string, body io.Reader, contentType string, expected int) error {
	resp, err := c.request(ctx, method, endpoint, body, contentType, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != expected {
		return httpError(resp, method)
	}
	return nil
}

func httpError(resp *http.Response, method string) *HTTPError {
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorJSONBytes+1))
	if err != nil || len(data) > maxErrorJSONBytes {
		return &HTTPError{Method: method, Status: resp.StatusCode}
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &payload) != nil || !validAPIErrorCode(payload.Error.Code) {
		return &HTTPError{Method: method, Status: resp.StatusCode}
	}
	return &HTTPError{Method: method, Status: resp.StatusCode, Code: payload.Error.Code}
}

func validAPIErrorCode(code string) bool {
	if len(code) == 0 || len(code) > 64 || code[0] < 'a' || code[0] > 'z' {
		return false
	}
	for _, char := range code[1:] {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func (c *Client) request(ctx context.Context, method, endpoint string, body io.Reader, contentType, idempotencyKey string) (*http.Response, error) {
	relative, err := url.Parse(endpoint)
	if err != nil || relative.IsAbs() || relative.Host != "" {
		return nil, errors.New("invalid Morde API endpoint")
	}
	target := *c.baseURL
	target.Path = path.Join(target.Path, relative.Path)
	target.RawPath = ""
	target.RawQuery = relative.RawQuery
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, fmt.Errorf("build Morde API request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json, application/octet-stream")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Morde API request: %w", err)
	}
	return resp, nil
}

func (c *Client) workspacePath(workspaceID string) string {
	return c.endpoint("api", "workspaces", workspaceID)
}

func (c *Client) sessionPath(workspaceID, sessionID string) string {
	return c.workspacePath(workspaceID) + "/sync-sessions/" + sessionID
}

func (c *Client) blockPath(workspaceID, sessionID, digest string) string {
	return c.sessionPath(workspaceID, sessionID) + "/blocks/" + digest
}

func (c *Client) endpoint(parts ...string) string {
	return "/" + path.Join(parts...)
}

type progressReporter struct {
	mu          sync.Mutex
	callback    func(Progress)
	session     SyncSession
	fileCount   int64
	totalBytes  int64
	currentPath string
}

func (p *progressReporter) report(currentPath string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.currentPath = currentPath
	if p.callback != nil {
		p.callback(Progress{SessionID: p.session.ID, State: p.session.State, Stage: p.session.Stage,
			PlannedBytes: p.session.PlannedBytes, ConfirmedBytes: p.session.ConfirmedBytes, FileCount: p.fileCount,
			FilesProcessed: p.fileCount, TotalBytes: p.totalBytes, BytesProcessed: p.totalBytes, CurrentPath: p.currentPath})
	}
}

func (p *progressReporter) setPath(value string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.currentPath = value
	if p.callback != nil {
		p.callback(Progress{SessionID: p.session.ID, State: p.session.State, Stage: p.session.Stage,
			PlannedBytes: p.session.PlannedBytes, ConfirmedBytes: p.session.ConfirmedBytes, FileCount: p.fileCount,
			FilesProcessed: p.fileCount, TotalBytes: p.totalBytes, BytesProcessed: p.totalBytes, CurrentPath: p.currentPath})
	}
}

func (p *progressReporter) add(bytes int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.session.ConfirmedBytes += bytes
	if p.session.ConfirmedBytes > p.session.PlannedBytes {
		p.session.ConfirmedBytes = p.session.PlannedBytes
	}
	if p.callback != nil {
		p.callback(Progress{SessionID: p.session.ID, State: p.session.State, Stage: p.session.Stage,
			PlannedBytes: p.session.PlannedBytes, ConfirmedBytes: p.session.ConfirmedBytes, FileCount: p.fileCount,
			FilesProcessed: p.fileCount, TotalBytes: p.totalBytes, BytesProcessed: p.totalBytes, CurrentPath: p.currentPath})
	}
}

type workspaceChunk struct {
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

type workspaceFile struct {
	Path          string                        `json:"path"`
	Kind          string                        `json:"kind,omitempty"`
	SymlinkTarget string                        `json:"symlink_target,omitempty"`
	HardlinkTo    string                        `json:"hardlink_to,omitempty"`
	Xattrs        []mordesync.ExtendedAttribute `json:"xattrs"`
	ACL           *mordesync.NativeACL          `json:"acl,omitempty"`
	MetadataKnown bool                          `json:"metadata_known,omitempty"`
	SizeBytes     int64                         `json:"size_bytes"`
	SHA256        string                        `json:"sha256"`
	Mode          uint32                        `json:"mode"`
	ModTimeNS     *int64                        `json:"mod_time_ns,omitempty"`
	Chunks        []workspaceChunk              `json:"chunks"`
}

type workspaceDirectory struct {
	Path          string                        `json:"path"`
	Mode          *uint32                       `json:"mode,omitempty"`
	ModTimeNS     *int64                        `json:"mod_time_ns,omitempty"`
	Xattrs        []mordesync.ExtendedAttribute `json:"xattrs"`
	ACL           *mordesync.NativeACL          `json:"acl,omitempty"`
	MetadataKnown bool                          `json:"metadata_known,omitempty"`
}

type workspaceManifest struct {
	WorkspaceID      string               `json:"workspace_id"`
	Revision         int64                `json:"revision"`
	Files            []workspaceFile      `json:"files"`
	EmptyDirectories []string             `json:"empty_directories"`
	Directories      []workspaceDirectory `json:"directories"`
	NextCursor       *string              `json:"next_cursor"`
}

type createSessionRequest struct {
	FolderID            string               `json:"folder_id,omitempty"`
	Direction           string               `json:"direction"`
	Policy              string               `json:"policy"`
	BaseRevision        int64                `json:"base_revision"`
	Scope               []string             `json:"scope,omitempty"`
	Files               []workspaceFile      `json:"files"`
	EmptyDirectories    []string             `json:"empty_directories"`
	Directories         []workspaceDirectory `json:"directories,omitempty"`
	DeletedPaths        []string             `json:"deleted_paths,omitempty"`
	DeletedDirectories  []string             `json:"deleted_directories,omitempty"`
	AckCursor           *string              `json:"ack_cursor,omitempty"`
	TaskID              string               `json:"task_id,omitempty"`
	TaskHubOperationKey string               `json:"task_hub_operation_key,omitempty"`
}

func toWorkspaceFile(file mordesync.File) workspaceFile {
	chunks := make([]workspaceChunk, 0, len(file.Blocks))
	for _, chunk := range file.Blocks {
		chunks = append(chunks, workspaceChunk{SHA256: chunk.SHA256, SizeBytes: chunk.Size})
	}
	result := workspaceFile{Path: file.Path, Kind: manifestFileKind(file), SymlinkTarget: file.SymlinkTarget, HardlinkTo: file.HardlinkTo,
		Xattrs: file.Xattrs, ACL: file.ACL, MetadataKnown: file.MetadataKnown,
		SizeBytes: file.Size, SHA256: file.SHA256, Mode: file.Mode, Chunks: chunks}
	if file.ModTimeKnown {
		modified := file.ModTimeNS
		result.ModTimeNS = &modified
	}
	return result
}

func toWorkspaceDirectory(directory mordesync.Directory) workspaceDirectory {
	result := workspaceDirectory{Path: directory.Path, Xattrs: directory.Xattrs, ACL: directory.ACL, MetadataKnown: directory.MetadataKnown}
	if directory.ModeKnown {
		mode := directory.Mode
		result.Mode = &mode
	}
	if directory.ModTimeKnown {
		modified := directory.ModTimeNS
		result.ModTimeNS = &modified
	}
	return result
}

func toCoreDirectory(directory workspaceDirectory) (mordesync.Directory, error) {
	if directory.Path != "." && mordesync.ValidateRelativePath(directory.Path) != nil {
		return mordesync.Directory{}, errors.New("server manifest contains an invalid directory path")
	}
	result := mordesync.Directory{Path: directory.Path, Xattrs: directory.Xattrs, ACL: directory.ACL, MetadataKnown: directory.MetadataKnown}
	if directory.Mode != nil {
		if *directory.Mode > 0o777 {
			return mordesync.Directory{}, errors.New("server manifest contains invalid directory permissions")
		}
		result.Mode, result.ModeKnown = *directory.Mode, true
	}
	if directory.ModTimeNS != nil {
		result.ModTimeNS, result.ModTimeKnown = *directory.ModTimeNS, true
	}
	return result, nil
}

func toCoreFile(file workspaceFile) (mordesync.File, error) {
	blocks := make([]mordesync.Block, 0, len(file.Chunks))
	var offset int64
	for _, chunk := range file.Chunks {
		if !digestPattern.MatchString(chunk.SHA256) || chunk.SizeBytes <= 0 || chunk.SizeBytes > mordesync.BlockSize {
			return mordesync.File{}, errors.New("server manifest contains an invalid chunk")
		}
		blocks = append(blocks, mordesync.Block{Offset: offset, Size: chunk.SizeBytes, SHA256: chunk.SHA256})
		offset += chunk.SizeBytes
	}
	coreFile := mordesync.File{Path: file.Path, Kind: file.Kind, SymlinkTarget: file.SymlinkTarget, HardlinkTo: file.HardlinkTo,
		Xattrs: file.Xattrs, ACL: file.ACL, MetadataKnown: file.MetadataKnown,
		Size: file.SizeBytes, SHA256: file.SHA256, Mode: file.Mode, Blocks: blocks}
	if file.ModTimeNS != nil {
		coreFile.ModTimeNS = *file.ModTimeNS
		coreFile.ModTimeKnown = true
	}
	if err := mordesync.ValidateFile(coreFile); err != nil {
		return mordesync.File{}, fmt.Errorf("invalid server manifest file %q: %w", file.Path, err)
	}
	return coreFile, nil
}

func inScope(filePath string, scopes []string) bool {
	for _, candidate := range scopes {
		candidate = strings.TrimSuffix(candidate, "/")
		if strings.EqualFold(filePath, candidate) || strings.HasPrefix(strings.ToLower(filePath), strings.ToLower(candidate)+"/") {
			return true
		}
	}
	return false
}

func transferBlockMap(values []TransferBlock) (map[string]TransferBlock, error) {
	result := make(map[string]TransferBlock, len(values))
	for _, block := range values {
		if !digestPattern.MatchString(block.Chunk.SHA256) || block.Chunk.SizeBytes <= 0 || block.Chunk.SizeBytes > mordesync.BlockSize || (block.Role != "upload" && block.Role != "download") {
			return nil, errors.New("Morde API returned an invalid transfer block")
		}
		key := blockKey(block.Role, block.Chunk.SHA256)
		if _, duplicate := result[key]; duplicate {
			return nil, errors.New("Morde API returned a duplicate transfer block")
		}
		result[key] = block
	}
	return result, nil
}

func downloadActionMap(values []DownloadAction, policy string) (map[string]DownloadAction, error) {
	if values == nil {
		return nil, errors.New("Morde API omitted the session download action list")
	}
	result := make(map[string]DownloadAction, len(values))
	for _, action := range values {
		if action.Path == "" || !fs.ValidPath(action.Path) || path.IsAbs(action.Path) || path.Clean(action.Path) != action.Path || strings.ContainsRune(action.Path, '\\') {
			return nil, errors.New("Morde API returned an unsafe download action path")
		}
		key := strings.ToLower(action.Path)
		if _, duplicate := result[key]; duplicate {
			return nil, errors.New("Morde API returned duplicate download actions")
		}
		if action.Action != "upsert" && action.Action != "delete" && action.Action != "directory_upsert" && action.Action != "directory_delete" {
			return nil, errors.New("Morde API returned an invalid download action")
		}
		if action.Action == "delete" || action.Action == "directory_delete" {
			if policy != "live" {
				return nil, errors.New("fill_missing sync session unexpectedly contains a delete action")
			}
			if action.SHA256 != nil || action.SizeBytes != 0 {
				return nil, errors.New("Morde API returned an invalid delete action for this sync policy")
			}
			if action.Action == "directory_delete" {
				if err := mordesync.ValidateDirectoryPathList([]string{action.Path}); err != nil {
					return nil, errors.New("Morde API returned an invalid directory delete action path")
				}
			}
			result[key] = action
			continue
		}
		if action.Action == "directory_upsert" {
			if action.SHA256 != nil || action.SizeBytes != 0 {
				return nil, errors.New("Morde API returned an invalid directory upsert action")
			}
			if action.Path != "." {
				if err := mordesync.ValidateDirectoryPathList([]string{action.Path}); err != nil {
					return nil, errors.New("Morde API returned an invalid directory upsert action path")
				}
			}
			result[key] = action
			continue
		}
		if action.SHA256 == nil || !digestPattern.MatchString(*action.SHA256) || action.SizeBytes < 0 {
			return nil, errors.New("Morde API returned an invalid upsert download action")
		}
		result[key] = action
	}
	return result, nil
}

func syncVerifiedExisting(ctx context.Context, directory string, expected mordesync.File, baseline *mordesync.File) error {
	if expected.Kind == mordesync.FileKindSymlink {
		return mordesync.VerifyExistingSymlink(ctx, directory, expected)
	}
	if ctx == nil || !fs.ValidPath(expected.Path) || path.Clean(expected.Path) != expected.Path || strings.ContainsRune(expected.Path, '\\') {
		return errors.New("invalid existing destination path")
	}
	root, err := openClientWorkspaceRoot(directory)
	if err != nil {
		return fmt.Errorf("open destination root: %w", err)
	}
	defer root.Close()
	parent, name, closeParent, err := openClientParent(root, expected.Path)
	if err != nil {
		return err
	}
	defer closeParent()
	info, err := parent.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("existing destination is not a regular file")
	}
	opened, err := parent.Open(name)
	if err != nil {
		return fmt.Errorf("open existing destination: %w", err)
	}
	defer opened.Close()
	openedInfo, err := opened.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return errors.New("existing destination changed while opening")
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, clientContextReader{ctx: ctx, reader: opened}); err != nil {
		return fmt.Errorf("verify existing destination: %w", err)
	}
	if hex.EncodeToString(hasher.Sum(nil)) != expected.SHA256 {
		return errors.New("existing destination content differs from the pinned manifest")
	}
	after, statErr := opened.Stat()
	current, pathErr := parent.Lstat(name)
	if statErr != nil || pathErr != nil || !os.SameFile(info, after) || !os.SameFile(info, current) ||
		after.Size() != expected.Size || current.Size() != expected.Size || after.ModTime() != info.ModTime() || current.ModTime() != info.ModTime() {
		return errors.New("existing destination changed during verification")
	}
	if baseline != nil && (uint32(info.Mode().Perm()) != expected.Mode || expected.ModTimeKnown && info.ModTime().UnixNano() != expected.ModTimeNS) {
		if manifestFileKind(*baseline) != manifestFileKind(expected) || baseline.Size != expected.Size || baseline.SHA256 != expected.SHA256 ||
			uint32(info.Mode().Perm()) != baseline.Mode ||
			(baseline.ModTimeKnown && info.ModTime().UnixNano() != baseline.ModTimeNS) || expected.ModTimeKnown && !baseline.ModTimeKnown {
			return fmt.Errorf("existing destination metadata changed after the live plan: %w", mordesync.ErrDestinationChange)
		}
		if uint32(info.Mode().Perm()) != expected.Mode {
			if err := opened.Chmod(os.FileMode(expected.Mode)); err != nil {
				return fmt.Errorf("set existing destination mode: %w", err)
			}
		}
		if expected.ModTimeKnown && info.ModTime().UnixNano() != expected.ModTimeNS {
			modified := time.Unix(0, expected.ModTimeNS)
			if err := parent.Chtimes(name, modified, modified); err != nil {
				return fmt.Errorf("set existing destination modification time: %w", err)
			}
		}
		updated, statErr := opened.Stat()
		current, pathErr = parent.Lstat(name)
		if statErr != nil || pathErr != nil || !os.SameFile(info, updated) || !os.SameFile(info, current) ||
			uint32(updated.Mode().Perm()) != expected.Mode || expected.ModTimeKnown && updated.ModTime().UnixNano() != expected.ModTimeNS {
			return fmt.Errorf("existing destination metadata changed during update: %w", mordesync.ErrDestinationChange)
		}
		if _, err := opened.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("verify existing destination after metadata update: %w", err)
		}
		hasher.Reset()
		if _, err := io.Copy(hasher, clientContextReader{ctx: ctx, reader: opened}); err != nil || hex.EncodeToString(hasher.Sum(nil)) != expected.SHA256 {
			return errors.Join(mordesync.ErrDestinationChange, err)
		}
	}
	if err := opened.Sync(); err != nil {
		return fmt.Errorf("sync existing destination: %w", err)
	}
	if err := syncClientDirectory(parent); err != nil {
		return fmt.Errorf("sync existing destination directory: %w", err)
	}
	return nil
}

// removeIfHash applies a remote delete only while the local destination still
// has the content recorded in the common live baseline. It is idempotent when
// the destination is already absent.
func removeIfHash(ctx context.Context, directory, workspaceID string, expected mordesync.File) (string, bool, error) {
	return removeIfHashAtBoundary(ctx, directory, workspaceID, expected, nil)
}

func removeIfHashAtBoundary(ctx context.Context, directory, workspaceID string, expected mordesync.File, afterPreserve func()) (string, bool, error) {
	if ctx == nil || !workspaceIDPattern.MatchString(workspaceID) || expected.Path == "" || !digestPattern.MatchString(expected.SHA256) || expected.Size < 0 {
		return "", false, errors.New("invalid live delete baseline")
	}
	root, err := openClientWorkspaceRoot(directory)
	if err != nil {
		return "", false, fmt.Errorf("open workspace root: %w", err)
	}
	defer root.Close()
	parent, name, closeParent, err := openClientParent(root, expected.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", true, nil
	}
	if err != nil {
		return "", false, err
	}
	defer closeParent()
	info, digest, target, exists, err := inspectClientFile(ctx, parent, name, expected.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", true, nil
	}
	if err != nil {
		return "", false, err
	}
	if !exists {
		return "", true, nil
	}
	if !sameClientFile(info, digest, target, expected) {
		return digest, false, nil
	}
	current, err := parent.Lstat(name)
	if err != nil || !os.SameFile(info, current) || current.Size() != info.Size() || current.ModTime() != info.ModTime() {
		return digest, false, mordesync.ErrDestinationChange
	}
	if err := ctx.Err(); err != nil {
		return digest, false, err
	}
	_, archivePath, err := preserveClientFile(ctx, root, parent, directory, workspaceID, name, info, expected)
	if err != nil {
		return digest, false, err
	}
	if afterPreserve != nil {
		afterPreserve()
	}
	if _, err := parent.Lstat(name); err == nil {
		return digest, false, fmt.Errorf("destination changed during remote deletion; recovery copy remains at %q: %w", archivePath, mordesync.ErrDestinationChange)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return digest, false, err
	}
	return digest, true, nil
}

// replaceIfHash atomically installs a remote update only if the destination
// still matches the exact common baseline. The new file is fully staged and
// verified before the existing path is replaced.
func replaceIfHash(ctx context.Context, directory, workspaceID string, expected, remote mordesync.File, source io.Reader) (string, error) {
	return replaceIfHashAtBoundary(ctx, directory, workspaceID, expected, remote, source, nil)
}

func replaceIfHashAtBoundary(ctx context.Context, directory, workspaceID string, expected, remote mordesync.File, source io.Reader, afterPreserve func()) (string, error) {
	if ctx == nil || source == nil && manifestFileKind(remote) != mordesync.FileKindSymlink || !workspaceIDPattern.MatchString(workspaceID) || expected.Path != remote.Path || expected.Path == "" ||
		mordesync.ValidateManifest(mordesync.Manifest{FileCount: 1, TotalBytes: expected.Size, Files: []mordesync.File{expected}}) != nil ||
		mordesync.ValidateManifest(mordesync.Manifest{FileCount: 1, TotalBytes: remote.Size, Files: []mordesync.File{remote}}) != nil ||
		!digestPattern.MatchString(expected.SHA256) || !digestPattern.MatchString(remote.SHA256) {
		return "", errors.New("invalid live replacement plan")
	}
	if manifestFileKind(expected) == mordesync.FileKindSymlink || manifestFileKind(remote) == mordesync.FileKindSymlink {
		return replaceLinkAware(ctx, directory, workspaceID, expected, remote, source, afterPreserve)
	}
	root, err := openClientWorkspaceRoot(directory)
	if err != nil {
		return "", fmt.Errorf("open workspace root: %w", err)
	}
	defer root.Close()
	parent, name, closeParent, err := openClientParent(root, expected.Path)
	if err != nil {
		return "", err
	}
	defer closeParent()
	initialInfo, initialHash, exists, err := hashClientPath(ctx, parent, name)
	if err != nil {
		return initialHash, err
	}
	if !exists || initialHash != expected.SHA256 || initialInfo.Size() != expected.Size || uint32(initialInfo.Mode().Perm()) != expected.Mode {
		return initialHash, fmt.Errorf("destination no longer matches the common live baseline: %w", mordesync.ErrDestinationChange)
	}
	tempName, err := clientTemporaryName()
	if err != nil {
		return initialHash, err
	}
	temporary, err := parent.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return initialHash, fmt.Errorf("create staged live update: %w", err)
	}
	tempExists := true
	defer func() {
		_ = temporary.Close()
		if tempExists {
			_ = parent.Remove(tempName)
		}
	}()
	hasher := sha256.New()
	limited := io.LimitReader(clientContextReader{ctx: ctx, reader: source}, remote.Size+1)
	written, err := io.Copy(io.MultiWriter(temporary, hasher), limited)
	if err != nil {
		return initialHash, fmt.Errorf("stage live update: %w", err)
	}
	if written != remote.Size || hex.EncodeToString(hasher.Sum(nil)) != remote.SHA256 {
		return initialHash, fmt.Errorf("staged live update failed size or SHA-256 verification: %w", mordesync.ErrContentMismatch)
	}
	if err := temporary.Chmod(os.FileMode(remote.Mode)); err != nil {
		return initialHash, fmt.Errorf("set staged live update mode: %w", err)
	}
	if remote.ModTimeKnown {
		modified := time.Unix(0, remote.ModTimeNS)
		if err := parent.Chtimes(tempName, modified, modified); err != nil {
			return initialHash, fmt.Errorf("set staged live update modification time: %w", err)
		}
	}
	if err := temporary.Sync(); err != nil {
		return initialHash, fmt.Errorf("sync staged live update: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return initialHash, fmt.Errorf("close staged live update: %w", err)
	}
	latestInfo, latestHash, exists, err := hashClientPath(ctx, parent, name)
	if err != nil {
		return latestHash, err
	}
	if !exists || latestHash != expected.SHA256 || !os.SameFile(initialInfo, latestInfo) || latestInfo.Size() != initialInfo.Size() || latestInfo.ModTime() != initialInfo.ModTime() || uint32(latestInfo.Mode().Perm()) != expected.Mode {
		return latestHash, fmt.Errorf("destination changed after the live plan; local content was preserved: %w", mordesync.ErrDestinationChange)
	}
	if err := ctx.Err(); err != nil {
		return latestHash, err
	}
	_, archivePath, err := preserveClientFile(ctx, root, parent, directory, workspaceID, name, initialInfo, expected)
	if err != nil {
		return latestHash, err
	}
	if afterPreserve != nil {
		afterPreserve()
	}
	if err := parent.Link(tempName, name); err != nil {
		if _, statErr := parent.Lstat(name); statErr == nil {
			return latestHash, fmt.Errorf("install live update without replacing a concurrent local file; recovery copy remains at %q: %w", archivePath, mordesync.ErrDestinationChange)
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return latestHash, fmt.Errorf("inspect destination after live update failed: %w", statErr)
		} else {
			if restoreErr := restorePreservedLink(root, directory, workspaceID, archivePath, expected.Path); restoreErr != nil {
				return latestHash, fmt.Errorf("install live update failed; local content remains recoverable at %q: %v: %w", archivePath, restoreErr, mordesync.ErrDestinationChange)
			}
			return latestHash, fmt.Errorf("install live update: %w", err)
		}
	}
	if err := syncClientDirectory(parent); err != nil {
		return latestHash, fmt.Errorf("sync live update directory; recovery copy remains at %q: %w", archivePath, err)
	}
	return latestHash, nil
}

func replaceLinkAware(ctx context.Context, directory, workspaceID string, expected, remote mordesync.File, source io.Reader, afterPreserve func()) (string, error) {
	root, err := openClientWorkspaceRoot(directory)
	if err != nil {
		return "", fmt.Errorf("open workspace root: %w", err)
	}
	defer root.Close()
	parent, name, closeParent, err := openClientParent(root, expected.Path)
	if err != nil {
		return "", err
	}
	defer closeParent()
	initialInfo, initialHash, initialTarget, exists, err := inspectClientFile(ctx, parent, name, expected.Path)
	if err != nil {
		return initialHash, err
	}
	if !exists || !sameClientFile(initialInfo, initialHash, initialTarget, expected) {
		return initialHash, fmt.Errorf("destination no longer matches the common live baseline: %w", mordesync.ErrDestinationChange)
	}
	tempName, err := clientTemporaryName()
	if err != nil {
		return initialHash, err
	}
	if manifestFileKind(remote) == mordesync.FileKindSymlink {
		if err := parent.Symlink(remote.SymlinkTarget, tempName); err != nil {
			return initialHash, fmt.Errorf("stage remote Workspace symlink: %w", err)
		}
		if remote.ModTimeKnown {
			tempPath := path.Join(path.Dir(remote.Path), tempName)
			tempFile := remote
			tempFile.Path = tempPath
			if err := mordesync.VerifyExistingSymlink(ctx, directory, tempFile); err != nil {
				_ = parent.Remove(tempName)
				return initialHash, fmt.Errorf("set staged Workspace symlink metadata: %w", err)
			}
		}
	} else {
		temporary, err := parent.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return initialHash, fmt.Errorf("create staged live update: %w", err)
		}
		hasher := sha256.New()
		limited := io.LimitReader(clientContextReader{ctx: ctx, reader: source}, remote.Size+1)
		written, stageErr := io.Copy(io.MultiWriter(temporary, hasher), limited)
		if stageErr == nil && (written != remote.Size || hex.EncodeToString(hasher.Sum(nil)) != remote.SHA256) {
			stageErr = fmt.Errorf("staged live update failed size or SHA-256 verification: %w", mordesync.ErrContentMismatch)
		}
		if stageErr == nil {
			stageErr = temporary.Chmod(os.FileMode(remote.Mode))
		}
		if stageErr == nil && remote.ModTimeKnown {
			modified := time.Unix(0, remote.ModTimeNS)
			stageErr = parent.Chtimes(tempName, modified, modified)
		}
		if stageErr == nil {
			stageErr = temporary.Sync()
		}
		closeErr := temporary.Close()
		if stageErr == nil {
			stageErr = closeErr
		}
		if stageErr != nil {
			_ = parent.Remove(tempName)
			return initialHash, fmt.Errorf("stage live update: %w", stageErr)
		}
	}
	tempInfo, err := parent.Lstat(tempName)
	if err != nil || manifestFileKind(remote) == mordesync.FileKindSymlink && tempInfo.Mode()&os.ModeSymlink == 0 ||
		manifestFileKind(remote) != mordesync.FileKindSymlink && !tempInfo.Mode().IsRegular() {
		_ = parent.Remove(tempName)
		return initialHash, errors.Join(errors.New("staged Workspace entry changed"), err)
	}
	defer func() {
		if current, statErr := parent.Lstat(tempName); statErr == nil && os.SameFile(tempInfo, current) {
			_ = parent.Remove(tempName)
		}
	}()
	latestInfo, latestHash, latestTarget, exists, err := inspectClientFile(ctx, parent, name, expected.Path)
	if err != nil {
		return latestHash, err
	}
	if !exists || !sameClientFile(latestInfo, latestHash, latestTarget, expected) || !os.SameFile(initialInfo, latestInfo) ||
		latestInfo.Size() != initialInfo.Size() || latestInfo.ModTime() != initialInfo.ModTime() {
		return latestHash, fmt.Errorf("destination changed after the live plan: %w", mordesync.ErrDestinationChange)
	}
	if err := ctx.Err(); err != nil {
		return latestHash, err
	}
	_, archivePath, err := preserveClientFile(ctx, root, parent, directory, workspaceID, name, initialInfo, expected)
	if err != nil {
		return latestHash, err
	}
	if afterPreserve != nil {
		afterPreserve()
	}
	if err := parent.Link(tempName, name); err != nil {
		if _, statErr := parent.Lstat(name); statErr == nil {
			return latestHash, fmt.Errorf("install live update without replacing a concurrent local path; recovery copy remains at %q: %w", archivePath, mordesync.ErrDestinationChange)
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return latestHash, fmt.Errorf("inspect destination after live update failed: %w", statErr)
		}
		if restoreErr := restorePreservedLink(root, directory, workspaceID, archivePath, expected.Path); restoreErr != nil {
			return latestHash, fmt.Errorf("install live update failed; local content remains recoverable at %q: %v: %w", archivePath, restoreErr, mordesync.ErrDestinationChange)
		}
		return latestHash, fmt.Errorf("install live update: %w", err)
	}
	installedInfo, installedHash, installedTarget, installed, err := inspectClientFile(ctx, parent, name, remote.Path)
	if err != nil || !installed || !os.SameFile(tempInfo, installedInfo) || !sameClientFile(installedInfo, installedHash, installedTarget, remote) {
		return latestHash, errors.Join(mordesync.ErrDestinationChange, err)
	}
	if err := syncClientDirectory(parent); err != nil {
		return latestHash, fmt.Errorf("sync directory after live update: %w", err)
	}
	return latestHash, nil
}

func removeLiveBaseline(ctx context.Context, parent *os.Root, name string, expectedInfo os.FileInfo, expected mordesync.File) (string, bool, error) {
	return removeLiveBaselineAtBoundary(ctx, parent, name, expectedInfo, expected, nil)
}

func removeLiveBaselineAtBoundary(ctx context.Context, parent *os.Root, name string, expectedInfo os.FileInfo, expected mordesync.File, afterDisplace func()) (string, bool, error) {
	preservedName, placeholderInfo, err := clientPreservedName(parent)
	if err != nil {
		return "", false, err
	}
	if err := displaceClientPath(parent, name, preservedName, placeholderInfo); err != nil {
		return "", false, fmt.Errorf("destination changed during remote deletion: %w", mordesync.ErrDestinationChange)
	}
	info, digest, exists, err := hashClientPath(ctx, parent, preservedName)
	if err != nil || !exists || !sameClientBaseline(info, digest, expectedInfo, expected) {
		cause := err
		if cause == nil {
			cause = mordesync.ErrDestinationChange
		}
		if restoreErr := restoreDisplacedClientPath(parent, preservedName, name); restoreErr != nil {
			return digest, false, fmt.Errorf("local file changed during remote deletion and is preserved as %q: %v: %w", preservedName, cause, mordesync.ErrDestinationChange)
		}
		return digest, false, fmt.Errorf("local file changed during remote deletion: %w", mordesync.ErrDestinationChange)
	}
	if err := ctx.Err(); err != nil {
		if restoreErr := restoreDisplacedClientPath(parent, preservedName, name); restoreErr != nil {
			return digest, false, fmt.Errorf("cancelled deletion left the local file preserved as %q: %v", preservedName, restoreErr)
		}
		return digest, false, err
	}
	if afterDisplace != nil {
		afterDisplace()
	}
	if err := removePreservedClientPath(parent, preservedName); err != nil {
		if restoreErr := restoreDisplacedClientPath(parent, preservedName, name); restoreErr != nil {
			return digest, false, fmt.Errorf("remove verified live destination: %v; local file preserved as %q: %w", err, preservedName, restoreErr)
		}
		return digest, false, fmt.Errorf("remove verified live destination: %w", err)
	}
	if err := syncClientDirectory(parent); err != nil {
		return digest, false, fmt.Errorf("sync directory after live delete: %w", err)
	}
	if _, err := parent.Lstat(name); err == nil {
		return digest, false, fmt.Errorf("destination changed during remote deletion; local content was preserved: %w", mordesync.ErrDestinationChange)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return digest, false, err
	}
	return digest, true, nil
}

func installLiveReplacement(ctx context.Context, parent *os.Root, name, stagedName string, expectedInfo os.FileInfo, expected mordesync.File) (string, error) {
	return installLiveReplacementAtBoundary(ctx, parent, name, stagedName, expectedInfo, expected, nil)
}

func installLiveReplacementAtBoundary(ctx context.Context, parent *os.Root, name, stagedName string, expectedInfo os.FileInfo, expected mordesync.File, afterDisplace func()) (string, error) {
	preservedName, placeholderInfo, err := clientPreservedName(parent)
	if err != nil {
		return "", err
	}
	if err := displaceClientPath(parent, name, preservedName, placeholderInfo); err != nil {
		return "", fmt.Errorf("destination changed during live update: %w", mordesync.ErrDestinationChange)
	}
	info, digest, exists, err := hashClientPath(ctx, parent, preservedName)
	if err != nil || !exists || !sameClientBaseline(info, digest, expectedInfo, expected) {
		cause := err
		if cause == nil {
			cause = mordesync.ErrDestinationChange
		}
		if restoreErr := restoreDisplacedClientPath(parent, preservedName, name); restoreErr != nil {
			return digest, fmt.Errorf("destination changed during live update; local content is preserved as %q: %v: %w", preservedName, cause, mordesync.ErrDestinationChange)
		}
		return digest, fmt.Errorf("destination changed during the live plan; local content was preserved: %w", mordesync.ErrDestinationChange)
	}
	if err := ctx.Err(); err != nil {
		if restoreErr := restoreDisplacedClientPath(parent, preservedName, name); restoreErr != nil {
			return digest, fmt.Errorf("cancelled live update left local content preserved as %q: %v", preservedName, restoreErr)
		}
		return digest, err
	}
	if afterDisplace != nil {
		afterDisplace()
	}
	// Link is an atomic no-clobber install: if another writer recreated name, it fails instead of replacing that file.
	if err := parent.Link(stagedName, name); err != nil {
		if _, statErr := parent.Lstat(name); statErr == nil {
			if backupInfo, backupHash, backupExists, backupErr := hashClientPath(ctx, parent, preservedName); backupErr == nil && backupExists && sameClientBaseline(backupInfo, backupHash, expectedInfo, expected) {
				if removeErr := removePreservedClientPath(parent, preservedName); removeErr != nil {
					return digest, fmt.Errorf("destination changed during live update; prior file remains preserved as %q: %w", preservedName, removeErr)
				}
			} else {
				return digest, fmt.Errorf("destination changed during live update; local content is preserved as %q: %v: %w", preservedName, backupErr, mordesync.ErrDestinationChange)
			}
			return digest, fmt.Errorf("destination changed during live update; local content was preserved: %w", mordesync.ErrDestinationChange)
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return digest, fmt.Errorf("install live update without overwriting destination: %w", err)
		}
		if restoreErr := restoreDisplacedClientPath(parent, preservedName, name); restoreErr != nil {
			return digest, fmt.Errorf("install live update without overwriting destination: %v; local content is preserved as %q: %w", err, preservedName, restoreErr)
		}
		return digest, fmt.Errorf("install live update without overwriting destination: %w", err)
	}
	backupInfo, backupHash, backupExists, backupErr := hashClientPath(ctx, parent, preservedName)
	if backupErr != nil || !backupExists || !sameClientBaseline(backupInfo, backupHash, expectedInfo, expected) {
		cause := backupErr
		if cause == nil {
			cause = mordesync.ErrDestinationChange
		}
		return digest, fmt.Errorf("live update installed, but the previous local file changed and is preserved as %q: %v: %w", preservedName, cause, mordesync.ErrDestinationChange)
	}
	if err := removePreservedClientPath(parent, preservedName); err != nil {
		return digest, fmt.Errorf("live update installed, but previous file cleanup failed; it is preserved as %q: %w", preservedName, err)
	}
	if err := syncClientDirectory(parent); err != nil {
		return digest, fmt.Errorf("sync directory after live update: %w", err)
	}
	return digest, nil
}

func sameClientBaseline(info os.FileInfo, digest string, expectedInfo os.FileInfo, expected mordesync.File) bool {
	return info != nil && expectedInfo != nil && os.SameFile(info, expectedInfo) && digest == expected.SHA256 &&
		info.Size() == expected.Size && uint32(info.Mode().Perm()) == expected.Mode
}

func clientPreservedName(parent *os.Root) (string, os.FileInfo, error) {
	for attempt := 0; attempt < 3; attempt++ {
		key, err := randomIdempotencyKey()
		if err != nil {
			return "", nil, err
		}
		name := ".morde-sync-tmp-preserved-" + key
		placeholder, err := parent.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return "", nil, fmt.Errorf("reserve live preservation path: %w", err)
		}
		info, statErr := placeholder.Stat()
		closeErr := placeholder.Close()
		if statErr != nil || closeErr != nil {
			if current, err := parent.Lstat(name); err == nil && info != nil && os.SameFile(info, current) {
				_ = parent.Remove(name)
			}
			if statErr != nil {
				return "", nil, fmt.Errorf("stat reserved live preservation path: %w", statErr)
			}
			return "", nil, fmt.Errorf("close reserved live preservation path: %w", closeErr)
		}
		return name, info, nil
	}
	return "", nil, errors.New("could not reserve a unique live preservation path")
}

func removePreservedClientPath(parent *os.Root, preservedName string) error {
	return parent.Remove(preservedName)
}

func displaceClientPath(parent *os.Root, name, preservedName string, placeholderInfo os.FileInfo) error {
	current, err := parent.Lstat(preservedName)
	if err != nil || !os.SameFile(placeholderInfo, current) || current.Size() != 0 {
		if err == nil {
			err = mordesync.ErrDestinationChange
		}
		return err
	}
	if err := parent.Rename(name, preservedName); err != nil {
		if reserved, statErr := parent.Lstat(preservedName); statErr == nil && os.SameFile(placeholderInfo, reserved) {
			_ = parent.Remove(preservedName)
		}
		return err
	}
	return nil
}

func restoreDisplacedClientPath(parent *os.Root, preservedName, name string) error {
	linkInfo, err := parent.Lstat(preservedName)
	if err != nil {
		return fmt.Errorf("cannot inspect preserved content at %q: %w", preservedName, err)
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		target, err := parent.Readlink(preservedName)
		latest, statErr := parent.Lstat(preservedName)
		if err != nil || statErr != nil || !os.SameFile(linkInfo, latest) || latest.Mode()&os.ModeSymlink == 0 {
			return errors.Join(fmt.Errorf("cannot verify preserved symlink at %q", preservedName), err, statErr)
		}
		if err := parent.Link(preservedName, name); err != nil {
			return fmt.Errorf("destination was recreated; symlink remains at %q: %w", preservedName, err)
		}
		restored, readErr := parent.Readlink(name)
		restoredInfo, restoredErr := parent.Lstat(name)
		if readErr != nil || restoredErr != nil || !os.SameFile(linkInfo, restoredInfo) || restored != target {
			return fmt.Errorf("restored symlink changed; original remains at %q: %v", preservedName, errors.Join(readErr, restoredErr))
		}
		if err := removePreservedClientPath(parent, preservedName); err != nil {
			return err
		}
		return syncClientDirectory(parent)
	}
	info, digest, exists, err := hashClientPath(context.Background(), parent, preservedName)
	if err != nil {
		return fmt.Errorf("cannot verify preserved content at %q: %w", preservedName, err)
	}
	if !exists {
		return fmt.Errorf("preserved content is missing at %q", preservedName)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("cannot safely restore non-regular path; content remains at %q", preservedName)
	}
	if err := parent.Link(preservedName, name); err == nil {
		restoredInfo, restoredDigest, restored, verifyErr := hashClientPath(context.Background(), parent, name)
		if verifyErr != nil {
			return fmt.Errorf("restored link changed during rollback; original remains at %q: %w", preservedName, verifyErr)
		}
		if !restored || !os.SameFile(info, restoredInfo) || restoredDigest != digest {
			return fmt.Errorf("restored link changed during rollback; original remains at %q", preservedName)
		}
		if err := removePreservedClientPath(parent, preservedName); err != nil {
			return err
		}
		return syncClientDirectory(parent)
	}
	source, err := parent.Open(preservedName)
	if err != nil {
		return err
	}
	defer source.Close()
	destination, err := parent.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("destination was recreated; content remains at %q: %w", preservedName, err)
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(destination, hasher), source)
	if copyErr == nil {
		copyErr = destination.Chmod(info.Mode().Perm())
	}
	if copyErr == nil {
		copyErr = destination.Sync()
	}
	destinationInfo, statErr := destination.Stat()
	closeErr := destination.Close()
	sourceInfo, sourceErr := source.Stat()
	currentInfo, currentDigest, currentExists, currentErr := hashClientPath(context.Background(), parent, name)
	preservedInfo, preservedDigest, preservedExists, preservedErr := hashClientPath(context.Background(), parent, preservedName)
	if copyErr != nil || statErr != nil || closeErr != nil || sourceErr != nil || currentErr != nil || preservedErr != nil ||
		written != info.Size() || hex.EncodeToString(hasher.Sum(nil)) != digest || !currentExists || !preservedExists ||
		!os.SameFile(destinationInfo, currentInfo) || !os.SameFile(info, sourceInfo) || !os.SameFile(info, preservedInfo) ||
		info.Size() != sourceInfo.Size() || info.ModTime() != sourceInfo.ModTime() || currentDigest != digest || preservedDigest != digest {
		return fmt.Errorf("restored content is incomplete or changed; original remains at %q", preservedName)
	}
	if err := removePreservedClientPath(parent, preservedName); err != nil {
		return err
	}
	return syncClientDirectory(parent)
}

func hashClientPath(ctx context.Context, parent *os.Root, name string) (os.FileInfo, string, bool, error) {
	info, err := parent.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, "", false, errors.New("live destination is not a regular file")
	}
	opened, err := parent.Open(name)
	if err != nil {
		return nil, "", false, err
	}
	defer opened.Close()
	openedInfo, err := opened.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, "", false, mordesync.ErrDestinationChange
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, clientContextReader{ctx: ctx, reader: opened}); err != nil {
		return nil, "", false, err
	}
	after, statErr := opened.Stat()
	current, pathErr := parent.Lstat(name)
	if statErr != nil || pathErr != nil || !os.SameFile(info, after) || !os.SameFile(info, current) ||
		after.Size() != info.Size() || current.Size() != info.Size() || after.ModTime() != info.ModTime() || current.ModTime() != info.ModTime() {
		return nil, "", false, mordesync.ErrDestinationChange
	}
	return info, hex.EncodeToString(hasher.Sum(nil)), true, nil
}

func inspectClientFile(ctx context.Context, parent *os.Root, name, relative string) (os.FileInfo, string, string, bool, error) {
	info, err := parent.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", "", false, nil
	}
	if err != nil {
		return nil, "", "", false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := parent.Readlink(name)
		current, statErr := parent.Lstat(name)
		if err != nil || statErr != nil || !os.SameFile(info, current) || current.Mode()&os.ModeSymlink == 0 ||
			current.Size() != info.Size() || current.ModTime() != info.ModTime() || current.Size() != int64(len([]byte(target))) {
			return nil, "", "", false, errors.Join(mordesync.ErrDestinationChange, err, statErr)
		}
		if err := mordesync.ValidateSymlinkTarget(relative, target); err != nil {
			return nil, "", "", false, err
		}
		return current, sumSHA256([]byte(target)), target, true, nil
	}
	current, digest, exists, err := hashClientPath(ctx, parent, name)
	return current, digest, "", exists, err
}

func sameClientFile(info os.FileInfo, digest, target string, expected mordesync.File) bool {
	if info == nil || digest != expected.SHA256 || info.Size() != expected.Size {
		return false
	}
	if manifestFileKind(expected) == mordesync.FileKindSymlink {
		return expected.Mode == 0 && info.Mode()&os.ModeSymlink != 0 && target == expected.SymlinkTarget
	}
	return info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && uint32(info.Mode().Perm()) == expected.Mode
}

func openClientWorkspaceRoot(directory string) (*os.Root, error) {
	abs, err := filepath.Abs(directory)
	if err != nil {
		return nil, errors.New("resolve workspace root")
	}
	abs = filepath.Clean(abs)
	if filepath.Dir(abs) == abs {
		return nil, errors.New("filesystem roots cannot be selected as a Workspace")
	}
	for current := abs; ; current = filepath.Dir(current) {
		if filepath.Base(current) == preservedDirectory {
			return nil, errors.New("recovery copies cannot be selected as a Workspace")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("workspace root must be a real directory")
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}
	openedInfo, err := root.Stat(".")
	if err != nil || !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
		_ = root.Close()
		return nil, errors.New("workspace root changed while opening")
	}
	return root, nil
}

func clientTemporaryName() (string, error) {
	key, err := randomIdempotencyKey()
	if err != nil {
		return "", err
	}
	return ".morde-sync-tmp-live-" + key, nil
}

type clientContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r clientContextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func openClientParent(root *os.Root, relative string) (*os.Root, string, func(), error) {
	parts := strings.Split(relative, "/")
	if len(parts) == 0 || parts[len(parts)-1] == "" {
		return nil, "", func() {}, errors.New("invalid destination path")
	}
	current := root
	owned := make([]*os.Root, 0, len(parts)-1)
	closeOwned := func() {
		for index := len(owned) - 1; index >= 0; index-- {
			_ = owned[index].Close()
		}
	}
	for _, part := range parts[:len(parts)-1] {
		info, err := current.Lstat(part)
		if err != nil {
			closeOwned()
			return nil, "", func() {}, fmt.Errorf("inspect destination parent: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			closeOwned()
			return nil, "", func() {}, errors.New("destination parent is unavailable or unsafe")
		}
		child, err := current.OpenRoot(part)
		if err != nil {
			closeOwned()
			return nil, "", func() {}, errors.New("open destination parent")
		}
		childInfo, err := child.Stat(".")
		if err != nil || !childInfo.IsDir() || !os.SameFile(info, childInfo) {
			_ = child.Close()
			closeOwned()
			return nil, "", func() {}, errors.New("destination parent changed while opening")
		}
		owned = append(owned, child)
		current = child
	}
	return current, parts[len(parts)-1], closeOwned, nil
}

func syncClientDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func blockKey(role, digest string) string { return role + ":" + digest }

func validateSession(session SyncSession) error {
	if !sessionIDPattern.MatchString(session.ID) || !workspaceIDPattern.MatchString(session.WorkspaceID) ||
		session.FolderID != "" && !syncFolderIDPattern.MatchString(session.FolderID) || session.BaseRevision < 0 || session.TargetRevision < 0 || session.PlannedBytes < 0 || session.ConfirmedBytes < 0 || session.ConfirmedBytes > session.PlannedBytes {
		return errors.New("Morde API returned an invalid sync session")
	}
	if session.Direction != "upload" && session.Direction != "download" && session.Direction != "bidirectional" {
		return errors.New("Morde API returned an invalid sync direction")
	}
	if session.Policy != "fill_missing" && session.Policy != "compare_merge" && session.Policy != "live" {
		return errors.New("Morde API returned an invalid sync policy")
	}
	switch session.State {
	case "planned", "transferring", "verifying", "applying", "completed", "conflict", "cancelled", "failed", "expired":
	default:
		return errors.New("Morde API returned an invalid sync state")
	}
	_, err := transferBlockMap(session.TransferBlocks)
	if err != nil {
		return err
	}
	_, err = downloadActionMap(session.DownloadActions, session.Policy)
	return err
}

func isTerminal(state string) bool {
	return state == "completed" || state == "conflict" || state == "cancelled" || state == "failed" || state == "expired"
}

func optionalHash(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func appendUniqueConflicts(destination, values []Conflict) []Conflict {
	seen := make(map[string]struct{}, len(destination))
	for _, conflict := range destination {
		seen[strings.ToLower(conflict.Path)] = struct{}{}
	}
	for _, conflict := range values {
		key := strings.ToLower(conflict.Path)
		if _, exists := seen[key]; !exists {
			destination = append(destination, conflict)
			seen[key] = struct{}{}
		}
	}
	return destination
}

func sumSHA256(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func randomIdempotencyKey() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate sync idempotency key: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

type resumeMetadata struct {
	Version              int                `json:"version"`
	WorkspaceID          string             `json:"workspace_id"`
	FolderID             string             `json:"folder_id,omitempty"`
	Directory            string             `json:"directory"`
	SessionID            string             `json:"session_id,omitempty"`
	Direction            string             `json:"direction"`
	Policy               string             `json:"policy,omitempty"`
	BaseRevision         int64              `json:"base_revision"`
	Scope                []string           `json:"scope"`
	DeletedPaths         []string           `json:"deleted_paths,omitempty"`
	DeletedDirectories   []string           `json:"deleted_directories,omitempty"`
	AckCursor            *string            `json:"ack_cursor,omitempty"`
	TaskID               string             `json:"task_id,omitempty"`
	TaskHubOperationKey  string             `json:"task_hub_operation_key,omitempty"`
	Baseline             mordesync.Manifest `json:"baseline,omitempty"`
	IdempotencyKey       string             `json:"idempotency_key"`
	CommitIdempotencyKey string             `json:"commit_idempotency_key,omitempty"`
	CommitQuotaRejected  bool               `json:"commit_quota_rejected,omitempty"`
	CreateQuotaRejected  bool               `json:"create_quota_rejected,omitempty"`
	State                string             `json:"state,omitempty"`
	Manifest             mordesync.Manifest `json:"manifest"`
}

func effectiveSavedPolicy(metadata resumeMetadata) string {
	if metadata.Policy == "" {
		return "fill_missing"
	}
	return metadata.Policy
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func canonicalDirectory(directory string) (string, error) {
	abs, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("workspace root must be a real directory")
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err = os.Lstat(canonical)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("workspace root must be a real directory")
	}
	return canonical, nil
}

func resumeCacheRoot() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate user cache: %w", err)
	}
	base := filepath.Join(cache, "morde")
	root := filepath.Join(base, "sync")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create private sync cache: %w", err)
	}
	for _, directory := range []string{base, root} {
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("sync cache path must contain only real directories")
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return "", fmt.Errorf("secure sync cache permissions: %w", err)
		}
	}
	return root, nil
}

func resumeFilename(directory, workspaceID string) (string, error) {
	root, err := resumeCacheRoot()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(workspaceID + "\x00" + directory))
	return filepath.Join(root, hex.EncodeToString(digest[:])+".json"), nil
}

func readResumeMetadata(directory, workspaceID string) (resumeMetadata, error) {
	filename, err := resumeFilename(directory, workspaceID)
	if err != nil {
		return resumeMetadata{}, err
	}
	rootPath := filepath.Dir(filename)
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return resumeMetadata{}, fmt.Errorf("open sync metadata cache: %w", err)
	}
	defer root.Close()
	name := filepath.Base(filename)
	info, err := root.Lstat(name)
	if err != nil {
		return resumeMetadata{}, fmt.Errorf("read sync metadata: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return resumeMetadata{}, errors.New("sync metadata file is not a private regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return resumeMetadata{}, fmt.Errorf("open sync metadata: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxJSONBytes+1))
	if err != nil {
		return resumeMetadata{}, fmt.Errorf("read sync metadata: %w", err)
	}
	if len(data) > maxJSONBytes {
		return resumeMetadata{}, errors.New("sync metadata exceeds its size limit")
	}
	var metadata resumeMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return resumeMetadata{}, fmt.Errorf("decode sync metadata: %w", err)
	}
	if metadata.Version != 1 || metadata.WorkspaceID != workspaceID || metadata.Directory != directory ||
		metadata.FolderID != "" && !syncFolderIDPattern.MatchString(metadata.FolderID) || metadata.IdempotencyKey == "" ||
		(metadata.CommitIdempotencyKey != "" && !idempotencyPattern.MatchString(metadata.CommitIdempotencyKey)) {
		return resumeMetadata{}, errors.New("sync metadata does not match the requested workspace")
	}
	return metadata, nil
}

func writeResumeMetadata(directory string, metadata resumeMetadata) error {
	filename, err := resumeFilename(directory, metadata.WorkspaceID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode sync metadata: %w", err)
	}
	if len(data) > maxJSONBytes {
		return errors.New("sync metadata exceeds its size limit")
	}
	root, err := os.OpenRoot(filepath.Dir(filename))
	if err != nil {
		return fmt.Errorf("open sync metadata cache: %w", err)
	}
	defer root.Close()
	name := filepath.Base(filename)
	if info, statErr := root.Lstat(name); statErr == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("refusing to replace a non-regular sync metadata file")
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect sync metadata: %w", statErr)
	}
	key, err := randomIdempotencyKey()
	if err != nil {
		return err
	}
	temporaryName := "." + name + "." + key + ".tmp"
	temporary, err := root.OpenFile(temporaryName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary sync metadata: %w", err)
	}
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = root.Remove(temporaryName)
		}
	}()
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("write sync metadata: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync metadata file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close sync metadata: %w", err)
	}
	if err := root.Rename(temporaryName, name); err != nil {
		return fmt.Errorf("install sync metadata: %w", err)
	}
	removeTemporary = false
	directoryFile, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directoryFile.Close()
	return directoryFile.Sync()
}

func removeResumeMetadata(directory, workspaceID, sessionID string) error {
	metadata, err := readResumeMetadata(directory, workspaceID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if metadata.SessionID != sessionID {
		return nil
	}
	filename, err := resumeFilename(directory, workspaceID)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Dir(filename))
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Remove(filepath.Base(filename)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove sync metadata: %w", err)
	}
	directoryFile, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directoryFile.Close()
	return directoryFile.Sync()
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalStringPointers(left, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func equalManifests(left, right mordesync.Manifest) bool {
	left = normalizeManifestKinds(left)
	right = normalizeManifestKinds(right)
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func normalizeManifestKinds(manifest mordesync.Manifest) mordesync.Manifest {
	if manifest.Files == nil {
		return manifest
	}
	manifest.Files = append([]mordesync.File(nil), manifest.Files...)
	for index := range manifest.Files {
		if manifest.Files[index].Kind == "" {
			manifest.Files[index].Kind = mordesync.FileKindRegular
		}
	}
	return manifest
}
