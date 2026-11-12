package workspacereplica

import (
	"bytes"
	"context"
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"morde-runtime/contract"
	mordesync "morde-sync/client"
)

type bindingManager interface {
	Capabilities(contract.CapabilitiesRequest) (contract.CapabilitiesResponse, error)
	SetWorkspaceBindings(context.Context, contract.SetWorkspaceBindingsRequest) (contract.SetWorkspaceBindingsResponse, error)
	DeleteTaskWorkspaceView(context.Context, string, contract.DeleteTaskWorkspaceViewRequest) (contract.DeleteTaskWorkspaceViewResponse, error)
}

type Options struct {
	HostID              string
	Generation          int64
	PortalURL           string
	BootstrapCredential string
	WorkspaceRoot       string
	WorkspaceUID        uint32
	WorkspaceGID        uint32
	Manager             bindingManager
	PollInterval        time.Duration
	OnError             func(error)
}

type Controller struct {
	options      Options
	portal       *url.URL
	admin        *http.Client
	mu           sync.Mutex
	watchers     map[string]watcher
	paused       map[string]pausedSync
	known        map[string]int64
	lastFence    *contract.HostFence
	lastBindings []contract.WorkspaceBinding
}

type watcher struct {
	version int64
	cancel  context.CancelFunc
	done    chan struct{}
}

type pausedSync struct {
	version   int64
	conflicts []string
	reported  bool
}

type bindingSnapshot struct {
	RuntimeGeneration int64         `json:"runtime_generation"`
	Items             []replicaItem `json:"items"`
}

type replicaItem struct {
	WorkspaceID               string                     `json:"workspace_id"`
	SyncCredential            string                     `json:"sync_credential"`
	Directory                 string                     `json:"directory"`
	State                     string                     `json:"state"`
	SyncState                 string                     `json:"sync_state"`
	SyncConflicts             []string                   `json:"sync_conflicts"`
	BindingVersion            int64                      `json:"binding_version"`
	DesiredRevision           int64                      `json:"desired_revision"`
	AppliedRevision           int64                      `json:"applied_revision"`
	ProjectUploadApplications []projectUploadApplication `json:"project_upload_applications"`
}

type projectUploadApplication struct {
	SessionID       string `json:"session_id"`
	SourceBindingID string `json:"source_binding_id"`
	ProjectPath     string `json:"project_path"`
	TargetRevision  int64  `json:"target_revision"`
	ManifestSHA256  string `json:"manifest_sha256"`
}

type projectUploadAppliedRequest struct {
	SourceBindingID string                `json:"source_binding_id"`
	TargetRevision  int64                 `json:"target_revision"`
	ManifestSHA256  string                `json:"manifest_sha256"`
	Status          string                `json:"status"`
	AppliedRevision *int64                `json:"applied_revision"`
	SyncState       *string               `json:"sync_state"`
	Impacts         []projectUploadImpact `json:"impacts"`
}

type projectUploadImpact struct {
	Path   string `json:"path"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

func New(options Options) (*Controller, error) {
	if !validHostID(options.HostID) || options.Generation < 1 || options.BootstrapCredential == "" || options.Manager == nil || options.WorkspaceRoot == "" {
		return nil, errors.New("Host workspace replica configuration is invalid")
	}
	parsed, err := url.Parse(options.PortalURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("Portal URL must be an HTTP(S) origin")
	}
	if options.PollInterval == 0 {
		options.PollInterval = 5 * time.Second
	}
	if options.PollInterval < time.Second || options.PollInterval > time.Minute {
		return nil, errors.New("Host workspace poll interval is out of range")
	}
	root, err := filepath.Abs(options.WorkspaceRoot)
	if err != nil {
		return nil, errors.New("Host workspace root is invalid")
	}
	options.WorkspaceRoot = filepath.Clean(root)
	return &Controller{
		options: options, portal: parsed,
		admin:    &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		watchers: make(map[string]watcher), paused: make(map[string]pausedSync), known: make(map[string]int64),
	}, nil
}

func (c *Controller) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("Host workspace replica context is required")
	}
	defer c.stopAll()
	delay := c.options.PollInterval
	for {
		err := c.reconcile(ctx)
		if err != nil && c.options.OnError != nil {
			c.options.OnError(err)
		}
		if err == nil {
			delay = c.options.PollInterval
		} else if delay < time.Minute {
			delay *= 2
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Controller) reconcile(ctx context.Context) error {
	var snapshot bindingSnapshot
	if err := c.requestJSON(ctx, http.MethodGet, c.bindingsURL(), c.options.BootstrapCredential, nil, &snapshot); err != nil {
		return err
	}
	if snapshot.RuntimeGeneration != c.options.Generation || len(snapshot.Items) > 64 {
		return errors.New("Portal returned an invalid Host workspace snapshot")
	}
	items := make(map[string]replicaItem, len(snapshot.Items))
	seenProjectSessions := make(map[string]struct{})
	for _, item := range snapshot.Items {
		if !validWorkspaceID(item.WorkspaceID) || item.Directory != item.WorkspaceID || item.BindingVersion < 1 ||
			item.DesiredRevision < 0 || item.AppliedRevision < 0 ||
			(item.State != "preparing" && item.State != "active" && item.State != "unbinding" && item.State != "detached" && item.State != "failed") ||
			(item.SyncState != "healthy" && item.SyncState != "conflict") || (item.SyncState == "healthy" && len(item.SyncConflicts) != 0) {
			return errors.New("Portal returned an invalid Host workspace binding")
		}
		for _, conflictPath := range item.SyncConflicts {
			if !validSyncConflictPath(conflictPath) {
				return errors.New("Portal returned an invalid Host workspace sync conflict")
			}
		}
		for _, application := range item.ProjectUploadApplications {
			if !validProjectUploadApplication(application) {
				return errors.New("Portal returned an invalid project upload application")
			}
			if _, exists := seenProjectSessions[application.SessionID]; exists {
				return errors.New("Portal returned duplicate project upload applications")
			}
			seenProjectSessions[application.SessionID] = struct{}{}
		}
		if _, exists := items[item.WorkspaceID]; exists {
			return errors.New("Portal returned duplicate Host workspace bindings")
		}
		items[item.WorkspaceID] = item
	}
	var deletionSnapshot contract.TaskWorkspaceViewDeletionSnapshot
	deletionPollErr := c.requestJSON(ctx, http.MethodGet, c.taskWorkspaceViewDeletionsURL(), c.options.BootstrapCredential, nil, &deletionSnapshot)
	if deletionPollErr == nil && !contract.ValidTaskWorkspaceViewDeletionSnapshot(deletionSnapshot, c.options.Generation) {
		deletionPollErr = errors.New("Portal returned an invalid Task Workspace view deletion snapshot")
	}
	c.reconcilePaused(items)
	watchable := make(map[string]replicaItem)
	for id, item := range items {
		if item.State == "active" && item.SyncState != "conflict" {
			watchable[id] = item
		}
	}
	if err := c.stopUnneededWatchers(ctx, watchable); err != nil {
		return err
	}

	var syncErr error
	syncedThisCycle := make(map[string]struct{})
	verifiedBaselines := make(map[string]struct{})
	blockedInitial := make(map[string]struct{})
	for workspaceID, item := range items {
		knownVersion, known := c.known[workspaceID]
		known = known && knownVersion == item.BindingVersion
		if item.State == "active" && item.SyncState == "conflict" {
			continue
		}
		if item.State == "active" && !known {
			if err := c.ensureWorkspaceDirectory(item.WorkspaceID); err != nil {
				blockedInitial[workspaceID] = struct{}{}
				if syncErr == nil {
					syncErr = err
				}
				continue
			}
			syncClient, err := mordesync.New(c.portal.String(), item.SyncCredential, nil)
			if err == nil {
				err = syncClient.ValidateLiveBaseline(ctx, item.WorkspaceID, c.workspacePath(item.WorkspaceID), nil)
			}
			if err != nil {
				blockedInitial[workspaceID] = struct{}{}
				if syncErr == nil {
					syncErr = fmt.Errorf("active Host workspace cannot safely resume without its live baseline: %w", err)
				}
				continue
			}
			verifiedBaselines[workspaceID] = struct{}{}
		}
		needsInitialSync := item.State == "preparing" && (!known || item.AppliedRevision < item.DesiredRevision)
		if needsInitialSync {
			updated, err := c.downloadAndAcknowledge(ctx, item, 0)
			if err != nil {
				blockedInitial[workspaceID] = struct{}{}
				if syncErr == nil {
					syncErr = err
				}
				continue
			}
			items[workspaceID] = updated
			syncedThisCycle[workspaceID] = struct{}{}
		}
	}

	capabilities, err := c.options.Manager.Capabilities(contract.CapabilitiesRequest{
		ProtocolVersion: contract.ProtocolVersion, HostID: c.options.HostID, RuntimeGeneration: c.options.Generation,
	})
	if err != nil {
		return errors.New("read current Runtime binding fence")
	}
	fence := contract.HostFence{
		HostID: capabilities.HostID, RuntimeGeneration: capabilities.RuntimeGeneration,
		RuntimeInstanceID: capabilities.RuntimeInstanceID, ControlVersion: capabilities.ControlVersion,
	}
	bindings := make([]contract.WorkspaceBinding, 0, len(items))
	active := make(map[string]replicaItem)
	for _, item := range items {
		if _, blocked := blockedInitial[item.WorkspaceID]; blocked {
			continue
		}
		knownVersion, known := c.known[item.WorkspaceID]
		known = known && knownVersion == item.BindingVersion
		_, baselineVerified := verifiedBaselines[item.WorkspaceID]
		include := item.State == "active" && (known || baselineVerified || item.AppliedRevision == item.DesiredRevision || item.SyncState == "conflict") ||
			item.State == "preparing" && item.AppliedRevision == item.DesiredRevision
		if !include {
			continue
		}
		if err := c.ensureWorkspaceDirectory(item.WorkspaceID); err != nil {
			return err
		}
		_, synced := syncedThisCycle[item.WorkspaceID]
		if c.lastFence == nil && !known && !synced {
			if err := chownWorkspaceTree(c.workspacePath(item.WorkspaceID), c.options.WorkspaceUID, c.options.WorkspaceGID); err != nil {
				if syncErr == nil {
					syncErr = err
				}
				continue
			}
		}
		bindings = append(bindings, contract.WorkspaceBinding{
			WorkspaceID: item.WorkspaceID, BindingVersion: item.BindingVersion, Directory: item.Directory,
		})
		if item.State == "active" {
			if item.SyncCredential == "" {
				return errors.New("Portal omitted an active Host workspace credential")
			}
			active[item.WorkspaceID] = item
		}
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].WorkspaceID < bindings[j].WorkspaceID })

	if err := c.stopUnneededWatchers(ctx, active); err != nil {
		return err
	}
	if !sameFence(c.lastFence, fence) || !sameBindings(c.lastBindings, bindings) {
		_, err := c.options.Manager.SetWorkspaceBindings(ctx, contract.SetWorkspaceBindingsRequest{
			ProtocolVersion: contract.ProtocolVersion, HostFence: fence, Bindings: bindings,
		})
		if err != nil {
			return errors.New("install Host workspace bindings in Runtime")
		}
		c.lastFence = &fence
		c.lastBindings = append([]contract.WorkspaceBinding(nil), bindings...)
	}
	for _, item := range items {
		if item.State == "unbinding" {
			if err := c.finishUnbinding(ctx, item); err != nil {
				return err
			}
		}
	}
	for _, item := range items {
		if item.State == "preparing" && item.AppliedRevision == item.DesiredRevision {
			if _, blocked := blockedInitial[item.WorkspaceID]; blocked {
				continue
			}
			if err := c.activate(ctx, item); err != nil {
				return err
			}
		}
	}
	for workspaceID, item := range active {
		c.known[workspaceID] = item.BindingVersion
		if item.SyncState != "conflict" {
			c.startWatcher(ctx, item)
		}
	}
	for workspaceID, item := range items {
		if item.State == "preparing" && item.AppliedRevision == item.DesiredRevision {
			if _, blocked := blockedInitial[workspaceID]; blocked {
				continue
			}
			c.known[workspaceID] = item.BindingVersion
		}
	}
	if deletionPollErr == nil {
		if err := c.applyTaskWorkspaceViewDeletions(ctx, fence, deletionSnapshot.Items); err != nil {
			deletionPollErr = err
		}
	}
	projectUploadErr := c.applyProjectUploadApplications(ctx, active)
	return errors.Join(syncErr, deletionPollErr, projectUploadErr)
}

func (c *Controller) applyTaskWorkspaceViewDeletions(ctx context.Context, fence contract.HostFence, items []contract.PendingTaskWorkspaceViewDeletion) error {
	var deletionErr error
	for _, item := range items {
		request := contract.DeleteTaskWorkspaceViewRequest{
			ProtocolVersion: contract.ProtocolVersion, HostFence: fence, TaskID: item.TaskID, RequestID: item.RequestID,
		}
		response, err := c.options.Manager.DeleteTaskWorkspaceView(ctx, item.TaskID, request)
		if err != nil {
			if deletionErr == nil {
				deletionErr = fmt.Errorf("Task Workspace view deletion %s is pending: %w", item.RequestID, err)
			}
			continue
		}
		if response.ProtocolVersion != contract.ProtocolVersion || response.HostFence != fence || response.TaskID != item.TaskID ||
			response.RequestID != item.RequestID || !response.Deleted {
			if deletionErr == nil {
				deletionErr = fmt.Errorf("Runtime did not confirm Task Workspace view deletion %s", item.RequestID)
			}
			continue
		}
		ackPath := c.taskWorkspaceViewDeletionAppliedURL(item)
		if err := c.requestJSON(ctx, http.MethodPost, ackPath, c.options.BootstrapCredential, struct{}{}, nil); err != nil && deletionErr == nil {
			deletionErr = fmt.Errorf("Task Workspace view deletion %s acknowledgement is pending: %w", item.RequestID, err)
		}
	}
	return deletionErr
}

func (c *Controller) downloadAndAcknowledge(ctx context.Context, item replicaItem, baseRevision int64) (replicaItem, error) {
	if item.SyncCredential == "" {
		return item, errors.New("Portal omitted a Host workspace sync credential")
	}
	if err := c.ensureWorkspaceDirectory(item.WorkspaceID); err != nil {
		return item, err
	}
	syncClient, err := mordesync.New(c.portal.String(), item.SyncCredential, nil)
	if err != nil {
		return item, errors.New("create Host workspace sync client")
	}
	result, err := syncClient.Sync(ctx, mordesync.SyncOptions{
		WorkspaceID: item.WorkspaceID, Directory: c.workspacePath(item.WorkspaceID), Direction: "download",
		Policy: "fill_missing", BaseRevision: &baseRevision,
	})
	if err != nil {
		return item, err
	}
	if result.Session.State != "completed" || result.Session.TargetRevision < item.AppliedRevision || len(result.Conflicts) != 0 {
		return item, errors.New("Host workspace download did not complete cleanly")
	}
	paths := appliedPaths(result.Session.DownloadActions)
	if err := chownAppliedPaths(c.workspacePath(item.WorkspaceID), paths, c.options.WorkspaceUID, c.options.WorkspaceGID); err != nil {
		return item, err
	}
	if err := chownWorkspaceTree(c.workspacePath(item.WorkspaceID), c.options.WorkspaceUID, c.options.WorkspaceGID); err != nil {
		return item, err
	}
	if err := syncClient.ConfirmDownloadedLiveBaseline(ctx, item.WorkspaceID, c.workspacePath(item.WorkspaceID), result.Session.TargetRevision); err != nil {
		return item, err
	}
	if err := c.recordApplied(ctx, item, result.Session.ID, result.Session.TargetRevision); err != nil {
		return item, err
	}
	item.AppliedRevision = result.Session.TargetRevision
	return item, nil
}

func (c *Controller) startWatcher(parent context.Context, item replicaItem) {
	if item.SyncState == "conflict" {
		return
	}
	c.mu.Lock()
	if paused, exists := c.paused[item.WorkspaceID]; exists {
		if paused.version != item.BindingVersion {
			delete(c.paused, item.WorkspaceID)
		} else if !paused.reported {
			conflicts := append([]string(nil), paused.conflicts...)
			c.mu.Unlock()
			if err := c.reportSyncStatus(parent, item, "conflict", conflicts); err != nil {
				c.reportError(errors.New("Host workspace conflict status update failed"))
				return
			}
			c.mu.Lock()
			if current, ok := c.paused[item.WorkspaceID]; ok && current.version == item.BindingVersion {
				current.reported = true
				c.paused[item.WorkspaceID] = current
			}
			c.mu.Unlock()
			return
		} else {
			// A healthy snapshot after a reported conflict is the explicit resume.
			delete(c.paused, item.WorkspaceID)
		}
	}
	if current, exists := c.watchers[item.WorkspaceID]; exists {
		if current.version == item.BindingVersion {
			select {
			case <-current.done:
				delete(c.watchers, item.WorkspaceID)
			default:
				c.mu.Unlock()
				return
			}
		} else {
			delete(c.watchers, item.WorkspaceID)
			c.mu.Unlock()
			current.cancel()
			select {
			case <-current.done:
			case <-parent.Done():
				return
			}
			c.startWatcher(parent, item)
			return
		}
	}
	ctx, cancel := context.WithCancel(parent)
	entry := watcher{version: item.BindingVersion, cancel: cancel, done: make(chan struct{})}
	c.watchers[item.WorkspaceID] = entry
	c.mu.Unlock()
	go func() {
		defer close(entry.done)
		syncClient, err := mordesync.New(c.portal.String(), item.SyncCredential, nil)
		if err != nil {
			c.reportError(errors.New("create Host live sync client"))
			return
		}
		for delay := time.Second; ; {
			err := syncClient.LiveSync(ctx, mordesync.LiveSyncOptions{
				WorkspaceID: item.WorkspaceID, Directory: c.workspacePath(item.WorkspaceID),
				OnResult: func(result mordesync.Result) { c.acknowledgeLiveResult(ctx, item, result) },
			})
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				var conflict *mordesync.LiveConflictError
				if errors.As(err, &conflict) {
					conflicts := syncStatusConflicts(conflict.Conflicts)
					if c.pause(item, conflicts) {
						if statusErr := c.reportSyncStatus(ctx, item, "conflict", conflicts); statusErr == nil {
							c.markPausedReported(item.WorkspaceID, item.BindingVersion)
						} else {
							c.reportError(errors.New("Host workspace conflict status update failed"))
						}
					}
					return
				}
				c.reportError(errors.New("Host workspace live sync paused"))
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if delay < time.Minute {
				delay *= 2
			}
		}
	}()
}

func (c *Controller) acknowledgeLiveResult(ctx context.Context, item replicaItem, result mordesync.Result) {
	if result.Session.State != "completed" || len(result.Conflicts) != 0 || len(result.Session.Conflicts) != 0 {
		return
	}
	paths := appliedPaths(result.Session.DownloadActions)
	for reported := false; ; {
		if ctx.Err() != nil {
			return
		}
		var err error
		if len(paths) > 0 {
			err = chownAppliedPaths(c.workspacePath(item.WorkspaceID), paths, c.options.WorkspaceUID, c.options.WorkspaceGID)
		}
		if err == nil {
			err = c.recordApplied(ctx, item, result.Session.ID, result.Session.TargetRevision)
		}
		if err == nil {
			err = c.reportSyncStatus(ctx, item, "healthy", nil)
		}
		if err == nil {
			c.clearPaused(item.WorkspaceID, item.BindingVersion)
			return
		}
		if ctx.Err() != nil {
			return
		}
		if !reported {
			c.reportError(errors.New("Host workspace applied revision receipt failed; retrying"))
			reported = true
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (c *Controller) stopUnneededWatchers(ctx context.Context, active map[string]replicaItem) error {
	for {
		c.mu.Lock()
		var workspaceID string
		var current watcher
		for id, candidate := range c.watchers {
			item, exists := active[id]
			if exists && item.BindingVersion == candidate.version {
				continue
			}
			workspaceID, current = id, candidate
			delete(c.watchers, id)
			break
		}
		c.mu.Unlock()
		if workspaceID == "" {
			return nil
		}
		current.cancel()
		select {
		case <-current.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *Controller) stopAll() {
	for {
		c.mu.Lock()
		var workspaceID string
		var current watcher
		for id, candidate := range c.watchers {
			workspaceID, current = id, candidate
			delete(c.watchers, id)
			break
		}
		c.mu.Unlock()
		if workspaceID == "" {
			return
		}
		current.cancel()
		<-current.done
	}
}

func (c *Controller) reconcilePaused(items map[string]replicaItem) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for workspaceID, paused := range c.paused {
		item, exists := items[workspaceID]
		if !exists || item.State != "active" || item.BindingVersion != paused.version {
			delete(c.paused, workspaceID)
			continue
		}
		if item.SyncState == "conflict" {
			paused.reported = true
			c.paused[workspaceID] = paused
		}
	}
}

func (c *Controller) pause(item replicaItem, conflicts []string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, exists := c.watchers[item.WorkspaceID]
	if !exists || current.version != item.BindingVersion {
		return false
	}
	c.paused[item.WorkspaceID] = pausedSync{version: item.BindingVersion, conflicts: append([]string(nil), conflicts...)}
	return true
}

func (c *Controller) markPausedReported(workspaceID string, version int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if paused, exists := c.paused[workspaceID]; exists && paused.version == version {
		paused.reported = true
		c.paused[workspaceID] = paused
	}
}

func (c *Controller) clearPaused(workspaceID string, version int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if paused, exists := c.paused[workspaceID]; exists && paused.version == version {
		delete(c.paused, workspaceID)
	}
}

func (c *Controller) reportSyncStatus(ctx context.Context, item replicaItem, state string, conflicts []string) error {
	if state != "healthy" && state != "conflict" {
		return errors.New("Host workspace sync state is invalid")
	}
	if conflicts == nil {
		conflicts = []string{}
	}
	for _, conflictPath := range conflicts {
		if !validSyncConflictPath(conflictPath) {
			return errors.New("Host workspace sync conflict is invalid")
		}
	}
	body := struct {
		State     string   `json:"state"`
		Conflicts []string `json:"conflicts"`
	}{State: state, Conflicts: conflicts}
	return c.requestJSON(ctx, http.MethodPost, c.bindingActionURL(item, "sync-status"), c.options.BootstrapCredential, body, nil)
}

func (c *Controller) applyProjectUploadApplications(ctx context.Context, items map[string]replicaItem) error {
	workspaceIDs := make([]string, 0, len(items))
	for workspaceID := range items {
		workspaceIDs = append(workspaceIDs, workspaceID)
	}
	sort.Strings(workspaceIDs)
	var reportErr error
	for _, workspaceID := range workspaceIDs {
		item := items[workspaceID]
		if item.State != "active" {
			continue
		}
		for _, application := range item.ProjectUploadApplications {
			body := projectUploadAppliedRequest{
				SourceBindingID: application.SourceBindingID, TargetRevision: application.TargetRevision,
				ManifestSHA256: application.ManifestSHA256, Impacts: []projectUploadImpact{},
			}
			switch {
			case item.SyncState == "healthy" && item.AppliedRevision >= application.TargetRevision:
				syncState, appliedRevision := "healthy", item.AppliedRevision
				body.Status, body.SyncState, body.AppliedRevision, body.Impacts = "applied", &syncState, &appliedRevision, []projectUploadImpact{}
			case item.SyncState == "conflict":
				syncState := "conflict"
				body.Status, body.SyncState = "unknown", &syncState
				body.Impacts = projectConflictImpacts(application.ProjectPath, item.SyncConflicts)
				if len(body.Impacts) > 0 {
					body.Status = "conflict"
				}
			default:
				continue
			}
			if err := c.requestJSON(ctx, http.MethodPost, c.projectUploadAppliedURL(item, application),
				c.options.BootstrapCredential, body, nil); err != nil && reportErr == nil {
				reportErr = fmt.Errorf("project upload application receipt failed: %w", err)
			}
		}
	}
	return reportErr
}

func projectConflictImpacts(projectPath string, conflicts []string) []projectUploadImpact {
	prefix := projectPath + "/"
	result := make([]projectUploadImpact, 0, len(conflicts))
	seen := make(map[string]struct{}, len(conflicts))
	for _, conflictPath := range conflicts {
		if !validSyncConflictPath(conflictPath) || !strings.HasPrefix(conflictPath, prefix) {
			continue
		}
		relative := strings.TrimPrefix(conflictPath, prefix)
		if !validSyncConflictPath(relative) {
			continue
		}
		if _, exists := seen[relative]; exists {
			continue
		}
		seen[relative] = struct{}{}
		result = append(result, projectUploadImpact{Path: relative, State: "conflict", Reason: "workspace_conflict"})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result
}

func validProjectUploadApplication(application projectUploadApplication) bool {
	if !validReplicaSessionID(application.SessionID) || !validProjectSourceBindingID(application.SourceBindingID) ||
		!validSyncConflictPath(application.ProjectPath) || !strings.HasPrefix(application.ProjectPath, "project/") ||
		application.TargetRevision < 0 || !validLowerHexSHA256(application.ManifestSHA256) {
		return false
	}
	return true
}

func validReplicaSessionID(value string) bool {
	if len(value) != 8 || value[0] != 'p' {
		return false
	}
	for i := 1; i < len(value); i++ {
		if !(value[i] >= 'a' && value[i] <= 'z') && !(value[i] >= '0' && value[i] <= '9') {
			return false
		}
	}
	return true
}

func validProjectSourceBindingID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' ||
		value[14] != '4' || (value[19] != '8' && value[19] != '9' && value[19] != 'a' && value[19] != 'b') {
		return false
	}
	for i, char := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func validLowerHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for i := range value {
		if !(value[i] >= '0' && value[i] <= '9') && !(value[i] >= 'a' && value[i] <= 'f') {
			return false
		}
	}
	return true
}

func syncStatusConflicts(conflicts []mordesync.Conflict) []string {
	result := make([]string, 0, len(conflicts))
	seen := make(map[string]struct{}, len(conflicts))
	for _, conflict := range conflicts {
		if validSyncConflictPath(conflict.Path) {
			if _, exists := seen[conflict.Path]; exists {
				continue
			}
			seen[conflict.Path] = struct{}{}
			result = append(result, conflict.Path)
		}
	}
	return result
}

func validSyncConflictPath(value string) bool {
	return value != "" && value != "." && fs.ValidPath(value) && path.Clean(value) == value &&
		!strings.ContainsRune(value, '\\')
}

func (c *Controller) activate(ctx context.Context, item replicaItem) error {
	var response replicaItem
	if err := c.requestJSON(ctx, http.MethodPost, c.bindingActionURL(item, "activate"), c.options.BootstrapCredential, struct{}{}, &response); err != nil {
		return err
	}
	return nil
}

func (c *Controller) finishUnbinding(ctx context.Context, item replicaItem) error {
	if item.SyncCredential == "" {
		return errors.New("Portal omitted an unbinding Host workspace credential")
	}
	if err := c.ensureWorkspaceDirectory(item.WorkspaceID); err != nil {
		return err
	}
	syncClient, err := mordesync.New(c.portal.String(), item.SyncCredential, nil)
	if err != nil {
		return errors.New("create Host workspace checkpoint client")
	}
	revision, err := syncClient.HandoffCheckpoint(ctx, mordesync.LiveSyncOptions{
		WorkspaceID: item.WorkspaceID, Directory: c.workspacePath(item.WorkspaceID),
	})
	if err != nil {
		return fmt.Errorf("Host workspace checkpoint is not safe: %w", err)
	}
	body := struct {
		CheckpointRevision int64 `json:"checkpoint_revision"`
	}{CheckpointRevision: revision}
	return c.requestJSON(ctx, http.MethodPost, c.bindingActionURL(item, "detached"), c.options.BootstrapCredential, body, &replicaItem{})
}

func (c *Controller) recordApplied(ctx context.Context, item replicaItem, sessionID string, revision int64) error {
	body := struct {
		SessionID       string `json:"session_id"`
		AppliedRevision int64  `json:"applied_revision"`
	}{SessionID: sessionID, AppliedRevision: revision}
	return c.requestJSON(ctx, http.MethodPost, c.bindingActionURL(item, "applied"), c.options.BootstrapCredential, body, &replicaItem{})
}

func (c *Controller) requestJSON(ctx context.Context, method, target, credential string, body any, output any) error {
	var source io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return errors.New("encode Host workspace request")
		}
		source = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, source)
	if err != nil {
		return errors.New("create Host workspace request")
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.admin.Do(request)
	if err != nil {
		return errors.New("Portal Host workspace request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Portal Host workspace request failed with status %d", response.StatusCode)
	}
	if output == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
	if err := decoder.Decode(output); err != nil {
		return errors.New("decode Portal Host workspace response")
	}
	return nil
}

func (c *Controller) bindingsURL() string {
	return c.portal.JoinPath("api", "host-replicas", c.options.HostID, "generations", strconv.FormatInt(c.options.Generation, 10), "workspace-bindings").String()
}

func (c *Controller) taskWorkspaceViewDeletionsURL() string {
	return c.portal.JoinPath("api", "host-replicas", c.options.HostID, "generations", strconv.FormatInt(c.options.Generation, 10),
		"task-workspace-view-deletions").String()
}

func (c *Controller) taskWorkspaceViewDeletionAppliedURL(item contract.PendingTaskWorkspaceViewDeletion) string {
	return c.portal.JoinPath("api", "host-replicas", c.options.HostID, "generations", strconv.FormatInt(c.options.Generation, 10),
		"tasks", item.TaskID, "workspace-view-deletions", item.RequestID, "applied").String()
}

func (c *Controller) bindingActionURL(item replicaItem, action string) string {
	return c.portal.JoinPath("api", "host-replicas", c.options.HostID, "generations", strconv.FormatInt(c.options.Generation, 10),
		"workspaces", item.WorkspaceID, "bindings", strconv.FormatInt(item.BindingVersion, 10), action).String()
}

func (c *Controller) projectUploadAppliedURL(item replicaItem, application projectUploadApplication) string {
	return c.portal.JoinPath("api", "host-replicas", c.options.HostID, "generations", strconv.FormatInt(c.options.Generation, 10),
		"workspaces", item.WorkspaceID, "bindings", strconv.FormatInt(item.BindingVersion, 10),
		"project-uploads", application.SessionID, "applied").String()
}

func (c *Controller) workspacePath(workspaceID string) string {
	return filepath.Join(c.options.WorkspaceRoot, workspaceID)
}

func (c *Controller) ensureWorkspaceDirectory(workspaceID string) error {
	if err := os.MkdirAll(c.options.WorkspaceRoot, 0o750); err != nil {
		return errors.New("create Host workspace root")
	}
	if err := os.MkdirAll(c.workspacePath(workspaceID), 0o750); err != nil {
		return errors.New("create Host workspace directory")
	}
	info, err := os.Lstat(c.workspacePath(workspaceID))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Host workspace directory is unsafe")
	}
	return nil
}

func appliedPaths(actions []mordesync.DownloadAction) []string {
	paths := make([]string, 0, len(actions))
	for _, action := range actions {
		if action.Applied {
			paths = append(paths, action.Path)
		}
	}
	return paths
}

func sameFence(previous *contract.HostFence, current contract.HostFence) bool {
	return previous != nil && *previous == current
}

func sameBindings(left, right []contract.WorkspaceBinding) bool {
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

func (c *Controller) reportError(err error) {
	if c.options.OnError != nil {
		c.options.OnError(err)
	}
}

func validHostID(value string) bool {
	return len(value) == 8 && value[0] == 'h' && validIDTail(value[1:])
}

func validWorkspaceID(value string) bool {
	return len(value) == 8 && value[0] == 'w' && validIDTail(value[1:])
}

func validIDTail(value string) bool {
	for _, char := range value {
		if !(char >= 'a' && char <= 'z') && !(char >= '0' && char <= '9') {
			return false
		}
	}
	return true
}
