package postgres

import (
	"context"
	"encoding/json"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/control"
	"morde/server/internal/domain/host"
)

func TestPostgresHostControlLeaseActorsRenewalRevocationAndExpiry(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 8_000, 16_000)
	member := createTestUser(t, store, 4, 8_000, 16_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	if _, err := store.UpsertHostMember(ctx, host.Member{
		HostID: hostValue.ID, OwnerID: owner.ID, UserID: member.ID,
		Permissions: []host.Permission{host.PermissionDesktopControl},
	}); err != nil {
		t.Fatal(err)
	}

	transition := completeControlTransitionForTest(t, store, hostValue.ID, owner.ID, member.ID,
		control.ActionTakeOver, 0, "lease-takeover-member")
	ref := host.Ref{ID: hostValue.ID, RuntimeGeneration: hostValue.RuntimeGeneration,
		ControlVersion: transition.TargetControlVersion, ControlState: host.ControlUser}
	lease, err := store.GetHostControlLease(ctx, hostValue.ID)
	if err != nil || !lease.Active || lease.ActorUserID != member.ID || lease.ControlVersion != ref.ControlVersion || lease.ExpiresAt == nil {
		t.Fatalf("active member lease = %#v, %v", lease, err)
	}
	if err := store.CheckHostControlLease(ctx, hostValue.ID, member.ID, ref.RuntimeGeneration, ref.ControlVersion); err != nil {
		t.Fatalf("check holder lease: %v", err)
	}
	if err := store.CheckHostControlLease(ctx, hostValue.ID, owner.ID, ref.RuntimeGeneration, ref.ControlVersion); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("non-holder check = %v; want conflict", err)
	}
	for connection := 0; connection < 2; connection++ {
		forwarded := false
		err := store.WithHostControlPermit(ctx, ref, member.ID, host.PermissionDesktopControl, func() error {
			forwarded = true
			return nil
		})
		if err != nil || !forwarded {
			t.Fatalf("same actor connection %d permit = forwarded %t, err=%v", connection+1, forwarded, err)
		}
	}
	if _, err := store.BeginControlTransition(ctx, control.BeginRequest{
		HostID: hostValue.ID, OwnerID: owner.ID, ActorUserID: owner.ID, RequestID: "owner-release-active-member",
		Action: control.ActionRelease, ExpectedControlVersion: ref.ControlVersion,
	}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("owner release while member holds active lease = %v; want fenced conflict", err)
	}

	renewed, err := store.RenewHostControlLease(ctx, hostValue.ID, member.ID, ref.ControlVersion)
	if err != nil || !renewed.Active || renewed.ExpiresAt == nil || !renewed.ExpiresAt.After(*lease.ExpiresAt) {
		t.Fatalf("renew member lease = %#v, %v", renewed, err)
	}
	if _, err := store.RenewHostControlLease(ctx, hostValue.ID, member.ID, ref.ControlVersion-1); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale version renewal = %v; want conflict", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET control_lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginControlTransition(ctx, control.BeginRequest{
		HostID: hostValue.ID, OwnerID: owner.ID, ActorUserID: member.ID, RequestID: "expired-member-release",
		Action: control.ActionRelease, ExpectedControlVersion: ref.ControlVersion,
	}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("expired member release = %v; want conflict", err)
	}

	if err := store.RevokeHostMember(ctx, hostValue.ID, owner.ID, member.ID); err != nil {
		t.Fatalf("revoke current controller: %v", err)
	}
	if err := store.CheckHostControlLease(ctx, hostValue.ID, member.ID, ref.RuntimeGeneration, ref.ControlVersion); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("revoked holder check = %v; want conflict", err)
	}
	forwarded := false
	if err := store.WithHostControlPermit(ctx, ref, member.ID, host.PermissionDesktopControl, func() error {
		forwarded = true
		return nil
	}); domain.CodeOf(err) != domain.CodeConflict || forwarded {
		t.Fatalf("revoked holder input = forwarded %t err=%v; want fenced", forwarded, err)
	}
	lease, err = store.GetHostControlLease(ctx, hostValue.ID)
	if err != nil || lease.Active || lease.ActorUserID != "" {
		t.Fatalf("lease after revoke = %#v, %v", lease, err)
	}

	// The owner can take over after revocation; expiration blocks writes and
	// cannot be used to resurrect an already expired lease.
	current, err := store.GetHost(ctx, hostValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	ownerRelease := completeControlTransitionForTest(t, store, hostValue.ID, owner.ID, owner.ID,
		control.ActionRelease, current.ControlVersion, "lease-release-revoked-member")
	forwarded = false
	if err := store.WithHostControlPermit(ctx, ref, member.ID, host.PermissionDesktopControl, func() error {
		forwarded = true
		return nil
	}); domain.CodeOf(err) != domain.CodeConflict || forwarded {
		t.Fatalf("old connection input after AI handoff = forwarded %t err=%v; want fenced", forwarded, err)
	}
	lease, err = store.GetHostControlLease(ctx, hostValue.ID)
	if err != nil || lease.Active || lease.ActorUserID != "" {
		t.Fatalf("lease after AI handoff = %#v, %v", lease, err)
	}
	ownerTransition := completeControlTransitionForTest(t, store, hostValue.ID, owner.ID, owner.ID,
		control.ActionTakeOver, ownerRelease.TargetControlVersion, "lease-takeover-owner")
	ownerRef := host.Ref{ID: hostValue.ID, RuntimeGeneration: hostValue.RuntimeGeneration,
		ControlVersion: ownerTransition.TargetControlVersion, ControlState: host.ControlUser}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET control_lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	lease, err = store.GetHostControlLease(ctx, hostValue.ID)
	if err != nil || lease.Active || lease.ActorUserID != "" {
		t.Fatalf("expired lease status = %#v, %v", lease, err)
	}
	if err := store.CheckHostControlLease(ctx, hostValue.ID, owner.ID, ownerRef.RuntimeGeneration, ownerRef.ControlVersion); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("expired holder check = %v; want conflict", err)
	}
	if _, err := store.RenewHostControlLease(ctx, hostValue.ID, owner.ID, ownerRef.ControlVersion); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("expired lease renewal = %v; want conflict", err)
	}
	forwarded = false
	if err := store.WithHostControlPermit(ctx, ownerRef, owner.ID, host.PermissionDesktopControl, func() error {
		forwarded = true
		return nil
	}); domain.CodeOf(err) != domain.CodeConflict || forwarded {
		t.Fatalf("expired holder input = forwarded %t err=%v; want fenced", forwarded, err)
	}
}

func completeControlTransitionForTest(t *testing.T, store *Store, hostID, ownerID, actorID string,
	action control.Action, expectedVersion int64, requestID string) control.Transition {
	t.Helper()
	ctx := context.Background()
	transition, err := store.BeginControlTransition(ctx, control.BeginRequest{
		HostID: hostID, OwnerID: ownerID, ActorUserID: actorID, RequestID: requestID,
		Action: action, ExpectedControlVersion: expectedVersion,
	})
	if err != nil {
		t.Fatalf("begin %s control transition: %v", action, err)
	}
	transition, err = store.SaveControlTaskIDs(ctx, hostID, requestID, nil)
	if err != nil || !transition.TaskIDsSaved {
		t.Fatalf("save %s task snapshot: %#v %v", action, transition, err)
	}
	if _, err := store.SetControlTransitionPhase(ctx, hostID, requestID, control.PhaseDraining); err != nil {
		t.Fatalf("start %s drain: %v", action, err)
	}
	if action == control.ActionTakeOver {
		receipt, err := json.Marshal(map[string]any{"receipt_id": requestID, "target_control_version": transition.TargetControlVersion})
		if err != nil {
			t.Fatal(err)
		}
		transition, err = store.SaveControlDrainedReceipt(ctx, hostID, requestID, receipt)
		if err != nil {
			t.Fatalf("save %s drain receipt: %v", action, err)
		}
	} else {
		transition, err = store.SetControlTransitionPhase(ctx, hostID, requestID, control.PhaseDrained)
		if err != nil {
			t.Fatalf("finish %s drain: %v", action, err)
		}
	}
	transition, err = store.CompleteControlTransition(ctx, hostID, requestID)
	if err != nil || transition.Phase != control.PhaseCompleted {
		t.Fatalf("complete %s control transition: %#v %v", action, transition, err)
	}
	return transition
}
