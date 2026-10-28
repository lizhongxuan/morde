package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"morde/server/internal/domain/host"
	"morde/server/internal/domain/task"
)

func TestRuntimeControlStopTaskOperationRequiresExactRuntimeFenceAndHandlesNotFound(t *testing.T) {
	service, _, manager, server := newRuntimeControlTest(t, 0, 0, "blocked", host.ControlAI, "")
	defer func() { _ = manager.Close() }()
	defer server.Close()

	ctx := context.Background()
	session, err := service.Connect(ctx, "h1234567", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	claimed := task.Task{ID: "q1234567", OwnerID: "u1234567", HostID: "h1234567", Status: task.StatusCancelling}
	expiresAt := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
	if err := session.SetTaskLease(ctx, claimed.ID, 1, expiresAt); err != nil {
		t.Fatal(err)
	}
	arguments := json.RawMessage(`{}`)
	_, _ = session.Execute(ctx, claimed.ID, "o1234567", 1, "missing.capability", "run", arguments, "", 0, nil)
	runtimeOperation, err := session.GetOperation(ctx, claimed.ID, "o1234567", 1)
	if err != nil {
		t.Fatal(err)
	}
	stored := task.Operation{
		ID: runtimeOperation.OperationID, TaskID: runtimeOperation.TaskID, HostID: runtimeOperation.HostID,
		RuntimeGeneration: runtimeOperation.RuntimeGeneration, RuntimeInstanceID: runtimeOperation.RuntimeInstanceID,
		ControlVersion: runtimeOperation.ControlVersion, LeaseVersion: 3, RuntimeLeaseVersion: runtimeOperation.LeaseVersion,
		Status: task.OperationDispatched,
	}
	status, _, notFound, err := service.StopTaskOperation(ctx, claimed, stored)
	if err != nil || notFound || status != task.OperationFailed {
		t.Fatalf("stop existing terminal operation = %q, notFound=%v, err=%v; want failed", status, notFound, err)
	}

	stored.ID = "o7654321"
	status, _, notFound, err = service.StopTaskOperation(ctx, claimed, stored)
	if err != nil || !notFound || status != "" {
		t.Fatalf("stop absent operation = %q, notFound=%v, err=%v; want explicit not-found", status, notFound, err)
	}

	stored.ID = "o1234567"
	stored.RuntimeInstanceID = "other-runtime"
	if _, _, notFound, err = service.StopTaskOperation(ctx, claimed, stored); err == nil || notFound {
		t.Fatalf("stale runtime instance was accepted: notFound=%v err=%v", notFound, err)
	}
}
