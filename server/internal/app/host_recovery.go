package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"

	"go.uber.org/zap"
)

type HostRecoveryResult struct {
	HostID            string
	RuntimeGeneration int64
	Action            string
	SandboxCount      int
}

func (s *hostService) RecoverUnknownRestore(ctx context.Context, principal identity.Principal, hostID string, expectedGeneration int64, confirmRetry bool) (HostRecoveryResult, error) {
	if err := authorizeAdmin(principal); err != nil {
		return HostRecoveryResult{}, err
	}
	if !domain.ValidHostID(hostID) || expectedGeneration <= 0 {
		return HostRecoveryResult{}, domain.InvalidArgument("host.recovery", "host recovery request is invalid")
	}
	value, err := s.hosts.GetHost(ctx, hostID)
	if err != nil {
		return HostRecoveryResult{}, err
	}
	if value.State == host.StateFailed {
		return s.recoverFailedHost(ctx, principal, value, expectedGeneration, confirmRetry)
	}
	if value.State != host.StateCreating || value.StateReason != "restore_unknown" ||
		value.RuntimeGeneration != expectedGeneration || value.RuntimeGeneration <= 1 || value.CreateAttemptedAt == nil {
		return HostRecoveryResult{}, domain.NewError(domain.CodeConflict, "host is no longer waiting for restore recovery")
	}
	if _, err := s.requireUsableHostWorkspace(ctx, principal, hostID, value.OwnerID); err != nil {
		return HostRecoveryResult{}, err
	}
	client, err := s.sandboxes.For(value.RuntimeProfile)
	if err != nil {
		return HostRecoveryResult{}, sandboxOperationError("select sandbox profile", err)
	}
	matches, err := hostRecoverySandboxes(ctx, client, s.instance, value)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeConflict {
			return HostRecoveryResult{}, err
		}
		return HostRecoveryResult{}, sandboxOperationError("inspect sandboxes before restore recovery", err)
	}
	retryBefore := time.Now().UTC().Add(-s.readyTimeout)
	if value.CreateAttemptedAt.After(retryBefore) {
		return HostRecoveryResult{}, domain.NewError(domain.CodeConflict, "the previous create attempt is still within its recovery window")
	}
	store, ok := s.hosts.(HostRestoreRecoveryStore)
	if !ok {
		return HostRecoveryResult{}, domain.NewError(domain.CodeUnsupported, "host restore recovery is unavailable")
	}
	result := HostRecoveryResult{HostID: hostID, RuntimeGeneration: value.RuntimeGeneration, SandboxCount: len(matches)}
	adoptExisting := false
	for _, sandbox := range matches {
		if sandbox.State == SandboxFailed {
			if !confirmRetry {
				return HostRecoveryResult{}, domain.NewError(domain.CodeConflict, "a failed sandbox was found; confirm the external issue is fixed before replacing it")
			}
			if err := deleteAndConfirm(ctx, client, sandbox.ID, time.Now().Add(s.readyTimeout), s.pollInterval); err != nil {
				return HostRecoveryResult{}, sandboxOperationError("remove failed sandbox before Host recovery", err)
			}
			continue
		}
		if sandbox.State == SandboxRunning || sandbox.State == SandboxPending {
			adoptExisting = true
			continue
		}
		if sandbox.State != SandboxTerminated {
			return HostRecoveryResult{}, domain.NewError(domain.CodeConflict, "sandbox state is not safe to adopt automatically")
		}
	}
	if adoptExisting {
		if err := store.RequeueUnknownHostRestore(ctx, hostID, value.RuntimeGeneration, value.SandboxID, retryBefore); err != nil {
			return HostRecoveryResult{}, err
		}
		result.Action = "adopt_existing"
		s.auditHostRecovery(ctx, principal, value, result)
		if err := s.notifyCreate(ctx, hostID); err != nil {
			s.logger.Error("notify recovered host sandbox worker failed", zap.String("host_id", hostID), zap.Error(err))
		}
		return result, nil
	}
	if !confirmRetry {
		return HostRecoveryResult{}, domain.NewError(domain.CodeConflict, "no matching sandbox was found; confirm the external issue is fixed before retrying")
	}
	if err := store.RequeueUnknownHostRestore(ctx, hostID, value.RuntimeGeneration, value.SandboxID, retryBefore); err != nil {
		return HostRecoveryResult{}, err
	}
	result.Action = "retry_create"
	s.auditHostRecovery(ctx, principal, value, result)
	if err := s.notifyCreate(ctx, hostID); err != nil {
		s.logger.Error("notify recovered host restore worker failed", zap.String("host_id", hostID), zap.Error(err))
	}
	return result, nil
}

func (s *hostService) recoverFailedHost(ctx context.Context, principal identity.Principal, value host.Host, expectedGeneration int64, confirmRetry bool) (HostRecoveryResult, error) {
	if value.RuntimeGeneration <= 1 || value.RuntimeGeneration != expectedGeneration ||
		(value.StateReason != "orchestrator_error" && value.StateReason != "ready_timeout") {
		return HostRecoveryResult{}, domain.NewError(domain.CodeConflict, "this failed Host state cannot be recovered automatically")
	}
	if _, err := s.requireUsableHostWorkspace(ctx, principal, value.ID, value.OwnerID); err != nil {
		return HostRecoveryResult{}, err
	}
	client, err := s.sandboxes.For(value.RuntimeProfile)
	if err != nil {
		return HostRecoveryResult{}, sandboxOperationError("select sandbox profile", err)
	}
	matches, err := hostRecoverySandboxes(ctx, client, s.instance, value)
	if err != nil {
		return HostRecoveryResult{}, sandboxOperationError("inspect sandboxes before failed Host recovery", err)
	}
	if len(matches) > 1 {
		return HostRecoveryResult{}, domain.NewError(domain.CodeConflict, "multiple sandboxes match this failed Host; manual review is required")
	}
	store, ok := s.hosts.(HostRestoreRecoveryStore)
	if !ok {
		return HostRecoveryResult{}, domain.NewError(domain.CodeUnsupported, "host restore recovery is unavailable")
	}
	result := HostRecoveryResult{HostID: value.ID, RuntimeGeneration: value.RuntimeGeneration, SandboxCount: len(matches)}
	if len(matches) == 1 {
		sandbox := matches[0]
		switch sandbox.State {
		case SandboxRunning, SandboxPending:
			if err := store.AdoptFailedHostRestore(ctx, value.ID, value.RuntimeGeneration, value.StateReason, sandbox.ID); err != nil {
				return HostRecoveryResult{}, err
			}
			result.Action = "adopt_existing"
			s.auditHostRecovery(ctx, principal, value, result)
			if err := s.notifyCreate(ctx, value.ID); err != nil {
				s.logger.Error("notify recovered failed Host sandbox worker failed", zap.String("host_id", value.ID), zap.Error(err))
			}
			return result, nil
		case SandboxFailed:
			if !confirmRetry {
				return HostRecoveryResult{}, domain.NewError(domain.CodeConflict, "a failed sandbox was found; confirm the external issue is fixed before replacing it")
			}
			if err := deleteAndConfirm(ctx, client, sandbox.ID, time.Now().Add(s.readyTimeout), s.pollInterval); err != nil {
				return HostRecoveryResult{}, sandboxOperationError("remove failed sandbox before Host recovery", err)
			}
		default:
			return HostRecoveryResult{}, domain.NewError(domain.CodeConflict, "sandbox state is not safe to adopt or replace automatically")
		}
	}
	if !confirmRetry {
		return HostRecoveryResult{}, domain.NewError(domain.CodeConflict, "no usable sandbox was found; confirm the external issue is fixed before retrying")
	}
	tmpl, ok := s.templates[value.TemplateID]
	if !ok {
		return HostRecoveryResult{}, domain.NewError(domain.CodeInternal, "host template is unavailable")
	}
	if err := s.diskCapacity.CheckCapacity(ctx); err != nil {
		if domain.CodeOf(err) == domain.CodeCapacityExceeded {
			return HostRecoveryResult{}, err
		}
		return HostRecoveryResult{}, domain.NewError(domain.CodeUpstreamError, "host storage capacity is unavailable")
	}
	credentialBytes := make([]byte, 32)
	if _, err := rand.Read(credentialBytes); err != nil {
		return HostRecoveryResult{}, fmt.Errorf("generate host runtime credential: %w", err)
	}
	credential := base64.RawURLEncoding.EncodeToString(credentialBytes)
	clear(credentialBytes)
	sealedCredential, err := s.sealer.Seal([]byte(credential))
	if err != nil {
		return HostRecoveryResult{}, fmt.Errorf("seal host runtime credential: %w", err)
	}
	updated, changed, err := s.hosts.BeginHostRestore(ctx, RestoreHostParams{
		HostID: value.ID, RuntimeCredentialEnc: sealedCredential,
		ExpiresAt: time.Now().UTC().Add(tmpl.Lifecycle.TTL), Capacity: s.capacity,
		RecoveryFromFailed: true, ExpectedGeneration: value.RuntimeGeneration,
		ExpectedReason: value.StateReason, ExpectedSandboxID: value.SandboxID,
	})
	if err != nil {
		return HostRecoveryResult{}, err
	}
	if !changed || updated.State != host.StateCreating || updated.StateReason != "restore_pending" {
		return HostRecoveryResult{}, domain.NewError(domain.CodeConflict, "Host recovery state changed; reload and retry")
	}
	result.RuntimeGeneration = updated.RuntimeGeneration
	result.Action = "retry_create"
	s.auditHostRecovery(ctx, principal, value, result)
	if err := s.notifyCreate(ctx, value.ID); err != nil {
		s.logger.Error("notify recovered failed Host restore worker failed", zap.String("host_id", value.ID), zap.Error(err))
	}
	return result, nil
}

func hostRecoverySandboxes(ctx context.Context, client SandboxClient, instance string, value host.Host) ([]Sandbox, error) {
	rows, err := client.List(ctx, map[string]string{"ch-env": instance, "ch-host": value.ID})
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Sandbox)
	for _, remote := range rows {
		if !metadataMatches(remote, instance, value.ID) {
			continue
		}
		if remote.ID == "" || !validSandboxGeneration(remote) {
			return nil, domain.NewError(domain.CodeConflict, "a remote sandbox has incomplete Host identity; manual review is required")
		}
		generation, _ := strconv.ParseInt(remote.Metadata["ch-generation"], 10, 64)
		if generation != value.RuntimeGeneration {
			if remote.State != SandboxTerminated {
				return nil, domain.NewError(domain.CodeConflict, "a sandbox from another runtime generation still exists; manual review is required")
			}
			continue
		}
		if remote.State == SandboxTerminated {
			continue
		}
		byID[remote.ID] = remote
	}
	if value.SandboxID != "" {
		if _, found := byID[value.SandboxID]; !found {
			remote, getErr := client.Get(ctx, value.SandboxID)
			switch {
			case errors.Is(getErr, ErrSandboxNotFound):
			case getErr != nil:
				return nil, getErr
			case remote.ID != value.SandboxID || !metadataMatches(remote, instance, value.ID) || !validSandboxGeneration(remote):
				return nil, domain.NewError(domain.CodeConflict, "saved sandbox identity does not match this Host; manual review is required")
			default:
				generation, _ := strconv.ParseInt(remote.Metadata["ch-generation"], 10, 64)
				if generation != value.RuntimeGeneration {
					if remote.State != SandboxTerminated {
						return nil, domain.NewError(domain.CodeConflict, "saved sandbox belongs to another active runtime generation")
					}
				} else if remote.State != SandboxTerminated {
					byID[remote.ID] = remote
				} else {
					delete(byID, remote.ID)
				}
			}
		}
	}
	result := make([]Sandbox, 0, len(byID))
	for _, remote := range byID {
		result = append(result, remote)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (s *hostService) auditHostRecovery(ctx context.Context, principal identity.Principal, value host.Host, result HostRecoveryResult) {
	s.writeAudit(ctx, audit.Entry{
		ActorID: adminActorID(principal), ActorKind: adminActorKind(principal),
		Action: audit.ActionHostRestoreRecovery, Target: value.ID,
		Detail: map[string]any{"runtime_generation": value.RuntimeGeneration, "action": result.Action, "sandbox_count": result.SandboxCount},
	})
}
