package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
)

func terminalSessionReservation(t *testing.T, hostValue host.Host, ownerID string) app.TerminalSession {
	t.Helper()
	attemptID, err := domain.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	return app.TerminalSession{
		HostID: hostValue.ID, OwnerID: ownerID, ActorID: ownerID,
		RuntimeGeneration: hostValue.RuntimeGeneration, ControlVersion: hostValue.ControlVersion,
		WorkspaceID: "", AttemptID: attemptID, State: app.TerminalSessionCreating,
	}
}

func TestPostgresTerminalSessionSerializesFirstCreationAndReusesReady(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner := createTestUser(t, store, 4, 8_000, 16_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlUser, 7)
	hostValue.ControlVersion = 7

	const contenders = 8
	start := make(chan struct{})
	type result struct {
		session app.TerminalSession
		created bool
		err     error
	}
	results := make(chan result, contenders)
	var group sync.WaitGroup
	reservations := make([]app.TerminalSession, contenders)
	for i := range reservations {
		reservations[i] = terminalSessionReservation(t, hostValue, owner.ID)
	}
	for i := range reservations {
		group.Add(1)
		go func(reservation app.TerminalSession) {
			defer group.Done()
			<-start
			session, created, err := store.EnsureTerminalSession(ctx, reservation, false)
			results <- result{session: session, created: created, err: err}
		}(reservations[i])
	}
	close(start)
	group.Wait()
	close(results)
	createdCount := 0
	var initial app.TerminalSession
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent ensure: %v", result.err)
		}
		if result.created {
			createdCount++
			initial = result.session
		} else if result.session.State != app.TerminalSessionCreating {
			t.Fatalf("competing ensure observed %+v, want creating", result.session)
		}
	}
	if createdCount != 1 || initial.State != app.TerminalSessionCreating {
		t.Fatalf("created reservations = %d, initial=%+v", createdCount, initial)
	}
	completed, err := store.CompleteTerminalSession(ctx, initial, "550e8400-e29b-41d4-a716-446655440000")
	if err != nil || !completed {
		t.Fatalf("complete terminal session = %t, %v", completed, err)
	}
	reused, created, err := store.EnsureTerminalSession(ctx, terminalSessionReservation(t, hostValue, owner.ID), false)
	if err != nil || created || reused.ID != initial.ID || reused.State != app.TerminalSessionReady || reused.AIOSessionID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Fatalf("reuse terminal session = %+v created=%t err=%v", reused, created, err)
	}
}

func TestPostgresTerminalSessionSeparatesOwnerAndMemberActors(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner := createTestUser(t, store, 4, 8_000, 16_000)
	member := createTestUser(t, store, 4, 8_000, 16_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlUser, 7)
	hostValue.ControlVersion = 7

	ownerReservation := terminalSessionReservation(t, hostValue, owner.ID)
	ownerSession, created, err := store.EnsureTerminalSession(ctx, ownerReservation, false)
	if err != nil || !created {
		t.Fatalf("reserve owner terminal session = %+v created=%t err=%v", ownerSession, created, err)
	}
	if completed, err := store.CompleteTerminalSession(ctx, ownerSession, "owner-session"); err != nil || !completed {
		t.Fatalf("complete owner terminal session = %t, %v", completed, err)
	}

	memberReservation := terminalSessionReservation(t, hostValue, owner.ID)
	memberReservation.ActorID = member.ID
	memberSession, created, err := store.EnsureTerminalSession(ctx, memberReservation, false)
	if err != nil || !created {
		t.Fatalf("reserve member terminal session = %+v created=%t err=%v", memberSession, created, err)
	}
	otherHost := createRunningControlHost(t, ctx, store, owner.ID, host.ControlUser, 7)
	wrongHost := memberSession
	wrongHost.HostID = otherHost.ID
	wrongOwner := memberSession
	wrongOwner.OwnerID = member.ID
	wrongGeneration := memberSession
	wrongGeneration.RuntimeGeneration++
	wrongControlVersion := memberSession
	wrongControlVersion.ControlVersion++
	for _, mismatch := range []struct {
		name        string
		reservation app.TerminalSession
	}{
		{name: "Host", reservation: wrongHost},
		{name: "owner", reservation: wrongOwner},
		{name: "generation", reservation: wrongGeneration},
		{name: "control version", reservation: wrongControlVersion},
	} {
		if completed, err := store.CompleteTerminalSession(ctx, mismatch.reservation, "stale-session"); err != nil || completed {
			t.Errorf("complete with mismatched %s fence = %t, %v; want false", mismatch.name, completed, err)
		}
	}
	if completed, err := store.CompleteTerminalSession(ctx, memberSession, "member-session"); err != nil || !completed {
		t.Fatalf("complete member terminal session = %t, %v", completed, err)
	}

	for _, actor := range []struct {
		name    string
		id      string
		session string
	}{{name: "owner", id: owner.ID, session: "owner-session"}, {name: "member", id: member.ID, session: "member-session"}} {
		reservation := terminalSessionReservation(t, hostValue, owner.ID)
		reservation.ActorID = actor.id
		reused, created, err := store.EnsureTerminalSession(ctx, reservation, false)
		if err != nil || created {
			t.Fatalf("reuse %s terminal session = %+v created=%t err=%v", actor.name, reused, created, err)
		}
		stored, err := terminalSessionByHostActor(ctx, pool, hostValue.ID, actor.id, false)
		if err != nil || stored.ID != reused.ID || stored.OwnerID != owner.ID || stored.ActorID != actor.id || stored.State != app.TerminalSessionReady {
			t.Fatalf("stored %s terminal session = %+v, reused=%+v, err=%v", actor.name, stored, reused, err)
		}
		if stored.AIOSessionID != actor.session {
			t.Fatalf("stored %s AIO session = %q, want %q", actor.name, stored.AIOSessionID, actor.session)
		}
	}
	if ownerSession.ID == memberSession.ID {
		t.Fatalf("owner and member share terminal reservation id %d", ownerSession.ID)
	}
}

func TestPostgresTerminalSessionUnknownGenerationFenceAndExplicitReplacement(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner := createTestUser(t, store, 4, 8_000, 16_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlUser, 2)
	first := terminalSessionReservation(t, hostValue, owner.ID)
	reserved, created, err := store.EnsureTerminalSession(ctx, first, false)
	if err != nil || !created {
		t.Fatalf("initial reserve = %+v created=%t err=%v", reserved, created, err)
	}
	if err := store.MarkTerminalSessionUnknown(ctx, reserved); err != nil {
		t.Fatal(err)
	}
	unknown, created, err := store.EnsureTerminalSession(ctx, terminalSessionReservation(t, hostValue, owner.ID), false)
	if err != nil || created || unknown.State != app.TerminalSessionUnknown {
		t.Fatalf("unknown session ensure = %+v created=%t err=%v", unknown, created, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET runtime_generation=runtime_generation+1, control_version=control_version+1 WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	newFence := hostValue
	newFence.RuntimeGeneration++
	newFence.ControlVersion++
	interrupted, created, err := store.EnsureTerminalSession(ctx, terminalSessionReservation(t, newFence, owner.ID), false)
	if err != nil || created || interrupted.State != app.TerminalSessionInterrupted || interrupted.AIOSessionID != "" {
		t.Fatalf("stale generation ensure = %+v created=%t err=%v", interrupted, created, err)
	}
	replacement, created, err := store.EnsureTerminalSession(ctx, terminalSessionReservation(t, newFence, owner.ID), true)
	if err != nil || !created || replacement.RuntimeGeneration != newFence.RuntimeGeneration || replacement.State != app.TerminalSessionCreating {
		t.Fatalf("explicit replacement = %+v created=%t err=%v", replacement, created, err)
	}
	var oldState string
	if err := pool.QueryRow(ctx, `SELECT state FROM host_terminal_sessions WHERE id=$1`, reserved.ID).Scan(&oldState); err != nil {
		t.Fatal(err)
	}
	if oldState != app.TerminalSessionAbandoned {
		t.Fatalf("old terminal session state = %q, want abandoned", oldState)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET control_version=control_version+1 WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if completed, err := store.CompleteTerminalSession(ctx, replacement, "stale-id"); err != nil || completed {
		t.Fatalf("stale control fence complete = %t, %v; want false", completed, err)
	}
}

func TestPostgresTerminalSessionNotFoundRequiresCurrentAIOHandle(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner := createTestUser(t, store, 4, 8_000, 16_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlUser, 3)
	hostValue.ControlVersion = 3
	reservation := terminalSessionReservation(t, hostValue, owner.ID)
	reserved, created, err := store.EnsureTerminalSession(ctx, reservation, false)
	if err != nil || !created {
		t.Fatalf("reserve terminal session = %+v created=%t err=%v", reserved, created, err)
	}
	const sessionID = "550e8400-e29b-41d4-a716-446655440000"
	if completed, err := store.CompleteTerminalSession(ctx, reserved, sessionID); err != nil || !completed {
		t.Fatalf("complete terminal session = %t, %v", completed, err)
	}
	for _, mismatch := range []struct {
		generation     int64
		controlVersion int64
		sessionID      string
	}{
		{generation: hostValue.RuntimeGeneration, controlVersion: hostValue.ControlVersion, sessionID: "other-session"},
		{generation: hostValue.RuntimeGeneration + 1, controlVersion: hostValue.ControlVersion, sessionID: sessionID},
		{generation: hostValue.RuntimeGeneration, controlVersion: hostValue.ControlVersion + 1, sessionID: sessionID},
	} {
		updated, err := store.MarkTerminalSessionInterrupted(ctx, hostValue.ID, owner.ID, mismatch.generation, mismatch.controlVersion, mismatch.sessionID)
		if err != nil || updated {
			t.Fatalf("mismatched terminal identity update = %t, %v", updated, err)
		}
	}
	current, err := terminalSessionByHostActor(ctx, pool, hostValue.ID, owner.ID, false)
	if err != nil || current.State != app.TerminalSessionReady || current.AIOSessionID != sessionID {
		t.Fatalf("mismatches changed current terminal session = %+v, %v", current, err)
	}
	updated, err := store.MarkTerminalSessionInterrupted(ctx, hostValue.ID, owner.ID, hostValue.RuntimeGeneration, hostValue.ControlVersion, sessionID)
	if err != nil || !updated {
		t.Fatalf("matching terminal identity update = %t, %v", updated, err)
	}
	current, err = terminalSessionByHostActor(ctx, pool, hostValue.ID, owner.ID, false)
	if err != nil || current.State != app.TerminalSessionInterrupted || current.AIOSessionID != "" {
		t.Fatalf("interrupted terminal session = %+v, %v", current, err)
	}
	updated, err = store.MarkTerminalSessionInterrupted(ctx, hostValue.ID, owner.ID, hostValue.RuntimeGeneration, hostValue.ControlVersion, sessionID)
	if err != nil || updated {
		t.Fatalf("replayed terminal interruption = %t, %v", updated, err)
	}
}

func TestPostgresTerminalSessionCreatingExpiresBeforeExplicitReplacement(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner := createTestUser(t, store, 4, 8_000, 16_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlUser, 0)
	first := terminalSessionReservation(t, hostValue, owner.ID)
	creating, created, err := store.EnsureTerminalSession(ctx, first, false)
	if err != nil || !created {
		t.Fatalf("initial reserve = %+v created=%t err=%v", creating, created, err)
	}
	earlyReplace, created, err := store.EnsureTerminalSession(ctx, terminalSessionReservation(t, hostValue, owner.ID), true)
	if err != nil || created || earlyReplace.ID != creating.ID || earlyReplace.State != app.TerminalSessionCreating {
		t.Fatalf("fresh creating replacement = %+v created=%t err=%v", earlyReplace, created, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE host_terminal_sessions SET created_at=clock_timestamp()-interval '2 minutes' WHERE id=$1`, creating.ID); err != nil {
		t.Fatal(err)
	}
	unknown, created, err := store.EnsureTerminalSession(ctx, terminalSessionReservation(t, hostValue, owner.ID), false)
	if err != nil || created || unknown.State != app.TerminalSessionUnknown {
		t.Fatalf("expired creating ensure = %+v created=%t err=%v", unknown, created, err)
	}
	replacement, created, err := store.EnsureTerminalSession(ctx, terminalSessionReservation(t, hostValue, owner.ID), true)
	if err != nil || !created || replacement.State != app.TerminalSessionCreating || replacement.ID == creating.ID {
		t.Fatalf("replacement after unknown = %+v created=%t err=%v", replacement, created, err)
	}
}
