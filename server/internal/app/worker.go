package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"go.uber.org/zap"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/workspace"
)

type WorkerApp struct {
	service     *hostService
	runtime     *Runtime
	cleanups    OrphanHostVolumeCleanupStore
	locks       LeaderLockStore
	renewBefore time.Duration
}

func NewWorkerApp(service *hostService, runtime *Runtime, cleanups OrphanHostVolumeCleanupStore, locks LeaderLockStore, renewBefore time.Duration) (*WorkerApp, error) {
	if service == nil || runtime == nil || cleanups == nil || locks == nil || renewBefore <= 0 {
		return nil, errors.New("worker app dependencies and positive renew window are required")
	}
	return &WorkerApp{service: service, runtime: runtime, cleanups: cleanups, locks: locks, renewBefore: renewBefore}, nil
}

func (w *WorkerApp) ListCreatingHosts(ctx context.Context) ([]string, error) {
	values, err := w.service.hosts.ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for _, value := range values {
		if value.State == host.StateCreating && value.DeletedAt == nil &&
			(value.RuntimeGeneration <= 1 || value.StateReason != "restore_unknown") {
			ids = append(ids, value.ID)
		}
	}
	return ids, nil
}

func (w *WorkerApp) AdvanceCreate(ctx context.Context, hostID string) (map[string]int, error) {
	before, err := w.service.hosts.GetHost(ctx, hostID)
	if err != nil {
		return nil, err
	}
	advanceErr := w.service.AdvanceCreate(ctx, hostID)
	after, err := w.service.hosts.GetHost(ctx, hostID)
	if err != nil {
		return nil, errors.Join(advanceErr, err)
	}
	actions := make(map[string]int)
	if before.State == host.StateCreating && after.State == host.StateFailed && after.StateReason == "ready_timeout" {
		actions["create_timeout"] = 1
	}
	return actions, advanceErr
}

func (w *WorkerApp) FlushActivity(ctx context.Context) error {
	return w.runtime.FlushActivity(ctx)
}

func (w *WorkerApp) HostCounts(ctx context.Context) (map[host.State]int, error) {
	values, err := w.service.hosts.ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	counts := make(map[host.State]int)
	for _, value := range values {
		counts[value.State]++
	}
	return counts, nil
}

func (w *WorkerApp) TryReconcilerLock(ctx context.Context) (LeaderLock, bool, error) {
	return w.locks.TryLeaderLock(ctx)
}

func (w *WorkerApp) Reconcile(ctx context.Context) (map[string]int, error) {
	actions := make(map[string]int)
	values, err := w.service.hosts.ListHosts(ctx)
	if err != nil {
		return actions, err
	}
	remotes, clients, err := w.listInstanceSandboxes(ctx)
	if err != nil {
		return actions, err
	}
	byID := make(map[string]host.Host, len(values))
	for _, value := range values {
		byID[value.ID] = value
	}
	var failures []error
	if err := w.reconcileOrphanHostVolumeCleanups(ctx, remotes, byID); err != nil {
		failures = append(failures, err)
	}
	for _, value := range values {
		if err := ctx.Err(); err != nil {
			return actions, errors.Join(errors.Join(failures...), err)
		}
		switch value.State {
		case host.StateCreating:
			if value.RuntimeGeneration > 1 && value.StateReason == "restore_unknown" {
				continue
			}
			if value.CreatedAt.IsZero() || time.Since(value.CreatedAt) < w.service.readyTimeout {
				continue
			}
			failed, changed, err := w.service.hosts.FailUnattemptedCreate(ctx, value.ID, "ready_timeout")
			if err != nil {
				failures = append(failures, fmt.Errorf("time out creating host %s: %w", value.ID, err))
				continue
			}
			if !changed {
				continue
			}
			actions["create_timeout"]++
			w.logAction("create_timeout", value.ID)
			if failed.State == host.StateFailed {
				if err := w.reconcileFailedHost(ctx, failed); err != nil {
					failures = append(failures, fmt.Errorf("clean timed-out host %s: %w", value.ID, err))
				}
			} else {
				w.logStateChange(value.ID, host.StateCreating, failed.State, failed.StateReason)
			}
		case host.StateRunning:
			if err := w.reconcileActiveHost(ctx, value, remotes[value.ID], clients, actions); err != nil {
				failures = append(failures, fmt.Errorf("reconcile host %s: %w", value.ID, err))
			}
		case host.StateStopping:
			if err := w.reconcileStoppingHost(ctx, value, actions); err != nil {
				failures = append(failures, fmt.Errorf("release host compute %s: %w", value.ID, err))
			}
		case host.StateFailed:
			if err := w.reconcileFailedHost(ctx, value); err != nil {
				failures = append(failures, fmt.Errorf("clean failed host %s: %w", value.ID, err))
			}
		case host.StateDeleting:
			if err := w.reconcileDeletingHost(ctx, value); err != nil {
				failures = append(failures, fmt.Errorf("delete host %s: %w", value.ID, err))
			}
		}
	}
	if err := w.deleteOrphans(ctx, remotes, byID, actions); err != nil {
		failures = append(failures, err)
	}
	return actions, errors.Join(failures...)
}

type remoteSandbox struct {
	profile string
	client  SandboxClient
	value   Sandbox
}

func (w *WorkerApp) listInstanceSandboxes(ctx context.Context) (map[string][]remoteSandbox, map[string]SandboxClient, error) {
	profiles := w.service.sandboxes.Profiles()
	sort.Strings(profiles)
	byHost := make(map[string][]remoteSandbox)
	clients := make(map[string]SandboxClient, len(profiles))
	for _, profile := range profiles {
		client, err := w.service.sandboxes.For(profile)
		if err != nil {
			return nil, nil, sandboxOperationError("select sandbox profile", err)
		}
		clients[profile] = client
		values, err := client.List(ctx, map[string]string{"ch-env": w.service.instance})
		if err != nil {
			return nil, nil, fmt.Errorf("list sandboxes for instance: %w", err)
		}
		for _, value := range values {
			if value.ID == "" || value.Metadata["ch-env"] != w.service.instance || !domain.ValidHostID(value.Metadata["ch-host"]) {
				continue
			}
			hostID := value.Metadata["ch-host"]
			byHost[hostID] = append(byHost[hostID], remoteSandbox{profile: profile, client: client, value: value})
		}
	}
	return byHost, clients, nil
}

func (w *WorkerApp) reconcileActiveHost(ctx context.Context, value host.Host, listed []remoteSandbox, clients map[string]SandboxClient, actions map[string]int) error {
	var unexpectedProfile bool
	for _, remote := range listed {
		if remote.value.State != SandboxTerminated && !metadataMatchesGeneration(remote.value, w.service.instance, value.ID, value.RuntimeGeneration) {
			return fmt.Errorf("matching Host sandbox has unexpected runtime generation")
		}
		if remote.profile != value.RuntimeProfile && remote.value.State != SandboxTerminated {
			unexpectedProfile = true
		}
	}
	if unexpectedProfile {
		return fmt.Errorf("matching sandbox is listed under an unexpected runtime profile")
	}
	client, ok := clients[value.RuntimeProfile]
	if !ok {
		return fmt.Errorf("runtime profile %q is not configured", value.RuntimeProfile)
	}
	matches, err := w.service.findHostSandboxes(ctx, client, value)
	if err != nil {
		return err
	}
	var active *Sandbox
	for i := range matches {
		if matches[i].State != SandboxTerminated {
			active = &matches[i]
			break
		}
	}
	if active == nil || active.State == SandboxFailed {
		reason, action := "lost", "lost"
		if active != nil {
			reason, action = "orchestrator_error", "sandbox_failed"
		}
		updated, err := w.service.hosts.TransitionHost(ctx, value.ID, value.State, host.StateFailed, reason)
		if err != nil {
			current, getErr := w.service.hosts.GetHost(ctx, value.ID)
			if getErr == nil && current.State != value.State {
				return nil
			}
			return err
		}
		actions[action]++
		w.logAction(action, value.ID)
		cleanupErr := w.reconcileFailedHost(ctx, updated)
		auditErr := w.writeSystemAudit(ctx, audit.ActionSystemLost, value.ID, map[string]any{"reason": action})
		return errors.Join(cleanupErr, auditErr)
	}
	if err := w.reconcileIdleAndExpiry(ctx, value, *active, client, actions); err != nil {
		return err
	}
	return nil
}

func (w *WorkerApp) reconcileIdleAndExpiry(ctx context.Context, value host.Host, remote Sandbox, client SandboxClient, actions map[string]int) error {
	tmpl, ok := w.service.templates[value.TemplateID]
	if !ok {
		return fmt.Errorf("template %q is unavailable", value.TemplateID)
	}
	now := time.Now().UTC()
	if value.ExpiresAt != nil && value.ExpiresAt.After(now.Add(w.renewBefore)) {
		return nil
	}
	expiresAt, err := client.Renew(ctx, remote.ID, now.Add(tmpl.Lifecycle.TTL))
	if err != nil {
		return sandboxOperationError("renew active sandbox", err)
	}
	if _, err := w.service.hosts.SetHostExpiry(ctx, value.ID, value.State, expiresAt); err != nil {
		return err
	}
	actions["renew"]++
	w.logAction("renew", value.ID)
	return w.writeSystemAudit(ctx, audit.ActionSystemRenew, value.ID, map[string]any{"expires_at": expiresAt.Format(time.RFC3339Nano)})
}

func (w *WorkerApp) reconcileFailedHost(ctx context.Context, value host.Host) error {
	if value.RuntimeGeneration > 1 {
		return fmt.Errorf("refuse failed-host cleanup for restored Host generation %d; data must be preserved", value.RuntimeGeneration)
	}
	bindings, err := w.service.workspaces.GetWorkspaceBindingsByHost(ctx, value.ID)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		if binding.State != workspace.BindingDetached {
			return fmt.Errorf("refuse failed-host cleanup while workspace %s is not safely detached", binding.Workspace.ID)
		}
	}
	if err := w.cleanupHostSandboxes(ctx, value); err != nil {
		return err
	}
	if err := w.cleanupRuntimeVolume(ctx, value.ID); err != nil {
		return err
	}
	if value.SandboxID != "" {
		if err := w.service.hosts.SetSandboxID(ctx, value.ID, host.StateFailed, nil); err != nil {
			return err
		}
	}
	return w.service.detachHostWorkspaces(ctx, value.ID)
}

func (w *WorkerApp) reconcileStoppingHost(ctx context.Context, value host.Host, actions map[string]int) error {
	activity, ok := w.service.hosts.(HostActivityLeaseStore)
	if !ok {
		return errors.New("host activity lease inspection is unavailable")
	}
	active, err := activity.HasActiveHostActivityLease(ctx, value.ID, value.RuntimeGeneration)
	if err != nil {
		return fmt.Errorf("check active Host sessions before release %s: %w", value.ID, err)
	}
	if active {
		return nil
	}
	profiles := w.service.sandboxes.Profiles()
	sort.Strings(profiles)
	for _, profile := range profiles {
		client, err := w.service.sandboxes.For(profile)
		if err != nil {
			return sandboxOperationError("select sandbox profile", err)
		}
		rows, err := client.List(ctx, map[string]string{"ch-env": w.service.instance, "ch-host": value.ID})
		if err != nil {
			return fmt.Errorf("list sandboxes before releasing Host %s: %w", value.ID, err)
		}
		for _, remote := range rows {
			if remote.ID == "" || !metadataMatches(remote, w.service.instance, value.ID) || remote.State == SandboxTerminated {
				continue
			}
			if !metadataMatchesGeneration(remote, w.service.instance, value.ID, value.RuntimeGeneration) {
				return fmt.Errorf("refuse cleanup while sandbox %s has an unexpected runtime generation", remote.ID)
			}
			if err := deleteAndConfirm(ctx, client, remote.ID, time.Now().Add(w.service.readyTimeout), w.service.pollInterval); err != nil {
				return fmt.Errorf("delete Host sandbox %s: %w", remote.ID, err)
			}
		}
	}
	if _, err := w.service.hosts.CompleteHostRelease(ctx, value.ID, value.RuntimeGeneration); err != nil {
		current, getErr := w.service.hosts.GetHost(ctx, value.ID)
		if getErr == nil && current.State == host.StateStopped && current.RuntimeGeneration == value.RuntimeGeneration {
			return nil
		}
		return err
	}
	actions["release"]++
	w.logAction("release", value.ID)
	return nil
}

func (w *WorkerApp) reconcileDeletingHost(ctx context.Context, value host.Host) error {
	activity, ok := w.service.hosts.(HostActivityLeaseStore)
	if !ok {
		return errors.New("host activity lease inspection is unavailable")
	}
	active, err := activity.HasActiveHostActivityLease(ctx, value.ID, value.RuntimeGeneration)
	if err != nil {
		return fmt.Errorf("check active Host sessions before deletion %s: %w", value.ID, err)
	}
	if active {
		return nil
	}

	bindings, err := w.service.workspaces.GetWorkspaceBindingsByHost(ctx, value.ID)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		if binding.State != workspace.BindingDetached {
			return fmt.Errorf("refuse Host deletion while workspace %s is not safely detached", binding.Workspace.ID)
		}
	}
	if err := w.cleanupHostSandboxes(ctx, value); err != nil {
		return err
	}
	if err := w.cleanupRuntimeVolume(ctx, value.ID); err != nil {
		return err
	}
	if err := w.cleanupProfileVolume(ctx, value.ID); err != nil {
		return err
	}
	if err := w.cleanupHostAppsVolume(ctx, value.ID); err != nil {
		return err
	}
	if value.SandboxID != "" {
		if err := w.service.hosts.SetSandboxID(ctx, value.ID, host.StateDeleting, nil); err != nil {
			return err
		}
	}
	if err := w.service.detachHostWorkspaces(ctx, value.ID); err != nil {
		return err
	}
	if err := w.cleanupWorkspaceRootVolume(ctx, value.ID); err != nil {
		return err
	}
	if _, err := w.service.hosts.TransitionHost(ctx, value.ID, host.StateDeleting, host.StateDeleted, ""); err != nil {
		current, getErr := w.service.hosts.GetHost(ctx, value.ID)
		if getErr == nil && current.State == host.StateDeleted {
			return nil
		}
		return err
	}
	w.logStateChange(value.ID, host.StateDeleting, host.StateDeleted, "complete")
	return nil
}

func (w *WorkerApp) cleanupRuntimeVolume(ctx context.Context, hostID string) error {
	if !domain.ValidHostID(hostID) {
		return fmt.Errorf("cannot delete runtime data volume for invalid host id %q", hostID)
	}
	if err := w.service.volumes.DeleteVolume(ctx, domain.RuntimeVolumeName(hostID)); err != nil {
		return fmt.Errorf("delete runtime data volume for host %s: %w", hostID, err)
	}
	w.logAction("runtime_volume_deleted", hostID)
	return nil
}

func (w *WorkerApp) cleanupProfileVolume(ctx context.Context, hostID string) error {
	if !domain.ValidHostID(hostID) {
		return fmt.Errorf("cannot delete profile volume for invalid host id %q", hostID)
	}
	if err := w.service.volumes.DeleteVolume(ctx, domain.ProfileVolumeName(hostID)); err != nil {
		return fmt.Errorf("delete profile volume for host %s: %w", hostID, err)
	}
	w.logAction("profile_volume_deleted", hostID)
	return nil
}

func (w *WorkerApp) cleanupHostAppsVolume(ctx context.Context, hostID string) error {
	if !domain.ValidHostID(hostID) {
		return fmt.Errorf("cannot delete host apps volume for invalid host id %q", hostID)
	}
	if err := w.service.volumes.DeleteVolume(ctx, domain.HostAppsVolumeName(hostID)); err != nil {
		return fmt.Errorf("delete host apps volume for host %s: %w", hostID, err)
	}
	w.logAction("host_apps_volume_deleted", hostID)
	return nil
}

func (w *WorkerApp) cleanupWorkspaceRootVolume(ctx context.Context, hostID string) error {
	if !domain.ValidHostID(hostID) {
		return fmt.Errorf("cannot delete workspace root volume for invalid host id %q", hostID)
	}
	if err := w.service.volumes.DeleteVolume(ctx, domain.WorkspaceRootVolumeName(hostID)); err != nil {
		return fmt.Errorf("delete workspace root volume for host %s: %w", hostID, err)
	}
	w.logAction("workspace_root_volume_deleted", hostID)
	return nil
}

func (w *WorkerApp) cleanupHostSandboxes(ctx context.Context, value host.Host) error {
	client, err := w.service.sandboxes.For(value.RuntimeProfile)
	if err != nil {
		return sandboxOperationError("select sandbox profile", err)
	}
	if err := w.service.cleanupHostGenerationSandboxes(ctx, client, value, ""); err != nil {
		return err
	}
	for _, profile := range w.service.sandboxes.Profiles() {
		if profile == value.RuntimeProfile {
			continue
		}
		other, err := w.service.sandboxes.For(profile)
		if err != nil {
			return sandboxOperationError("select sandbox profile", err)
		}
		rows, err := other.List(ctx, map[string]string{"ch-env": w.service.instance, "ch-host": value.ID})
		if err != nil {
			return fmt.Errorf("list sandboxes for host %s: %w", value.ID, err)
		}
		for _, remote := range rows {
			if !metadataMatches(remote, w.service.instance, value.ID) || remote.State == SandboxTerminated {
				continue
			}
			if !metadataMatchesGeneration(remote, w.service.instance, value.ID, value.RuntimeGeneration) {
				return fmt.Errorf("refuse cleanup while sandbox %s has an unexpected runtime generation", remote.ID)
			}
			if remote.ID == "" {
				return fmt.Errorf("sandbox for host %s has no id", value.ID)
			}
			if err := deleteAndConfirm(ctx, other, remote.ID, time.Now().Add(w.service.readyTimeout), w.service.pollInterval); err != nil {
				return fmt.Errorf("delete sandbox for host %s: %w", value.ID, err)
			}
		}
	}
	return nil
}

func (w *WorkerApp) deleteOrphans(ctx context.Context, remotes map[string][]remoteSandbox, hosts map[string]host.Host, actions map[string]int) error {
	var failures []error
	for hostID, entries := range remotes {
		knownHost, hasHostRecord := hosts[hostID]
		present := false
		if hasHostRecord {
			present = knownHost.DeletedAt == nil && knownHost.State != host.StateDeleted
		} else {
			value, err := w.service.hosts.GetHost(ctx, hostID)
			switch {
			case err == nil:
				knownHost, hasHostRecord = value, true
				present = value.DeletedAt == nil && value.State != host.StateDeleted
			case domain.CodeOf(err) == domain.CodeNotFound:
			case err != nil:
				failures = append(failures, fmt.Errorf("check orphan host %s: %w", hostID, err))
				continue
			}
		}
		if present {
			continue
		}
		verified := make([]remoteSandbox, 0, len(entries))
		needsCleanup, safeToClean := false, true
		for _, entry := range entries {
			if !metadataMatches(entry.value, w.service.instance, hostID) {
				continue
			}
			if hasHostRecord && !metadataMatchesGeneration(entry.value, w.service.instance, hostID, knownHost.RuntimeGeneration) {
				failures = append(failures, fmt.Errorf("refuse to delete sandbox %s for host %s: runtime generation does not match", entry.value.ID, hostID))
				safeToClean = false
				continue
			}
			if !hasHostRecord && !validSandboxGeneration(entry.value) {
				failures = append(failures, fmt.Errorf("refuse to delete unattributed orphan sandbox %s for host %s", entry.value.ID, hostID))
				safeToClean = false
				continue
			}
			generation, _ := strconv.ParseInt(entry.value.Metadata["ch-generation"], 10, 64)
			pending, err := w.cleanups.EnsureOrphanHostVolumeCleanup(ctx, w.service.instance, hostID, entry.profile, entry.value.ID, generation, entry.value.State != SandboxTerminated)
			if err != nil {
				failures = append(failures, fmt.Errorf("record orphan sandbox %s cleanup for host %s: %w", entry.value.ID, hostID, err))
				safeToClean = false
				continue
			}
			verified = append(verified, entry)
			needsCleanup = needsCleanup || pending
		}
		if !safeToClean || len(verified) == 0 || !needsCleanup {
			continue
		}
		sandboxesCleaned := true
		for _, entry := range verified {
			if entry.value.State == SandboxTerminated {
				continue
			}
			if err := deleteAndConfirm(ctx, entry.client, entry.value.ID, time.Now().Add(w.service.readyTimeout), w.service.pollInterval); err != nil {
				failures = append(failures, fmt.Errorf("delete orphan sandbox %s: %w", entry.value.ID, err))
				sandboxesCleaned = false
				continue
			}
			actions["orphan_deleted"]++
			w.logAction("orphan_deleted", hostID)
			if err := w.writeSystemAudit(ctx, audit.ActionOrphanDeleted, entry.value.ID, map[string]any{"host_id": hostID}); err != nil {
				failures = append(failures, err)
			}
		}
		if sandboxesCleaned {
			if err := w.cleanupOrphanHostVolumes(ctx, hostID); err != nil {
				failures = append(failures, err)
				continue
			}
			if err := w.cleanups.CompleteOrphanHostVolumeCleanup(ctx, w.service.instance, hostID); err != nil {
				failures = append(failures, fmt.Errorf("complete orphan host %s volume cleanup: %w", hostID, err))
			}
		}
	}
	return errors.Join(failures...)
}

func (w *WorkerApp) reconcileOrphanHostVolumeCleanups(ctx context.Context, remotes map[string][]remoteSandbox, hosts map[string]host.Host) error {
	hostIDs, err := w.cleanups.ListOrphanHostVolumeCleanups(ctx, w.service.instance)
	if err != nil {
		return fmt.Errorf("list orphan host volume cleanups: %w", err)
	}
	var failures []error
	for _, hostID := range hostIDs {
		if !domain.ValidHostID(hostID) {
			failures = append(failures, fmt.Errorf("refuse orphan volume cleanup for invalid host id %q", hostID))
			continue
		}
		if value, ok := hosts[hostID]; ok && value.State != host.StateDeleted {
			continue
		}
		value, err := w.service.hosts.GetHost(ctx, hostID)
		if err == nil && value.State != host.StateDeleted {
			continue
		}
		if err != nil && domain.CodeOf(err) != domain.CodeNotFound {
			failures = append(failures, fmt.Errorf("verify orphan host %s before volume cleanup: %w", hostID, err))
			continue
		}
		activeSandbox := false
		for _, remote := range remotes[hostID] {
			if remote.value.State != SandboxTerminated {
				activeSandbox = true
				break
			}
		}
		if activeSandbox {
			continue
		}
		if err := w.cleanupOrphanHostVolumes(ctx, hostID); err != nil {
			failures = append(failures, err)
			continue
		}
		if err := w.cleanups.CompleteOrphanHostVolumeCleanup(ctx, w.service.instance, hostID); err != nil {
			failures = append(failures, fmt.Errorf("complete orphan host %s volume cleanup: %w", hostID, err))
		}
	}
	return errors.Join(failures...)
}

func (w *WorkerApp) cleanupOrphanHostVolumes(ctx context.Context, hostID string) error {
	var failures []error
	if err := w.cleanupRuntimeVolume(ctx, hostID); err != nil {
		failures = append(failures, fmt.Errorf("clean orphan host %s runtime data: %w", hostID, err))
	}
	if err := w.cleanupProfileVolume(ctx, hostID); err != nil {
		failures = append(failures, fmt.Errorf("clean orphan host %s profile data: %w", hostID, err))
	}
	if err := w.cleanupHostAppsVolume(ctx, hostID); err != nil {
		failures = append(failures, fmt.Errorf("clean orphan host %s installed apps: %w", hostID, err))
	}
	return errors.Join(failures...)
}

func (w *WorkerApp) writeSystemAudit(ctx context.Context, action audit.Action, target string, detail map[string]any) error {
	if err := w.service.audit.Write(ctx, audit.Entry{ActorID: "system", ActorKind: audit.ActorSystem, Action: action, Target: target, Detail: detail}); err != nil {
		if w.service.countAudit != nil {
			w.service.countAudit()
		}
		return fmt.Errorf("write audit action %s: %w", action, err)
	}
	return nil
}

func (w *WorkerApp) logAction(action, hostID string) {
	w.service.logger.Info("reconcile action", zap.String("job", "reconcile"), zap.String("action", action), zap.String("host_id", hostID))
}

func (w *WorkerApp) logStateChange(hostID string, from, to host.State, reason string) {
	w.service.logger.Info("host state changed", zap.String("host_id", hostID), zap.String("from", string(from)), zap.String("to", string(to)), zap.String("reason", reason))
}

var _ interface {
	ListCreatingHosts(context.Context) ([]string, error)
	AdvanceCreate(context.Context, string) (map[string]int, error)
	Reconcile(context.Context) (map[string]int, error)
	FlushActivity(context.Context) error
	HostCounts(context.Context) (map[host.State]int, error)
	TryReconcilerLock(context.Context) (LeaderLock, bool, error)
} = (*WorkerApp)(nil)
