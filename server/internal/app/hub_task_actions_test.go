package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain/hub"
)

func TestHubTaskCallBindsTaskAuthorizationAndOperationToTarget(t *testing.T) {
	service, _ := newHubFixture(t)
	target := connectHubTestPeer(t, service, "u1234567", "i7654321")
	_ = target.next(t, "devices")
	taskContext := &hub.TaskContext{
		TaskID: "q1234567", AuthorizationVersion: 3, TargetGrantVersion: 5,
		OperationID: "o1234567", TaskRevision: 7,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	args, err := json.Marshal(hub.FileFetchArgs{
		FetchID: "o1234567", Path: "reports/report.pdf", TaskRevision: taskContext.TaskRevision,
		Ticket: strings.Repeat("A", 43), TicketExpiresAt: time.Now().UTC().Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	resultCh := make(chan HubTaskCallResult, 1)
	go func() {
		resultCh <- service.CallTaskAction(ctx, "u1234567", "i7654321", taskContext, hub.ActionFileFetch, args)
	}()

	call := target.next(t, "call")
	from, ok := call["from"].(map[string]any)
	if !ok || from["kind"] != "task" || from["task_id"] != taskContext.TaskID ||
		from["authorization_version"] != float64(taskContext.AuthorizationVersion) ||
		from["target_grant_version"] != float64(taskContext.TargetGrantVersion) ||
		from["operation_id"] != taskContext.OperationID || from["task_revision"] != float64(taskContext.TaskRevision) ||
		call["action"] != hub.ActionFileFetch {
		t.Fatalf("task Hub call context = %#v", call)
	}
	receipt, err := json.Marshal(hub.FileFetchReceipt{
		FetchID: "o1234567", TaskRevision: 7, Status: "completed", TargetGrantVersion: 5,
		SourceVersion: strings.Repeat("a", 64), SourceSHA256: strings.Repeat("b", 64), SizeBytes: 10,
		ContentID: "o1234568", ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	target.push(t, map[string]any{"type": "result", "id": call["id"], "ok": true, "body": json.RawMessage(receipt)})
	select {
	case result := <-resultCh:
		if !result.OK || result.Unknown || !json.Valid(result.Body) {
			t.Fatalf("task Hub result = %#v", result)
		}
	case <-ctx.Done():
		t.Fatal("task Hub call did not receive the target result")
	}
}

func TestHubTaskCallTreatsGrantVersionMismatchAsUnknown(t *testing.T) {
	service, _ := newHubFixture(t)
	target := connectHubTestPeer(t, service, "u1234567", "i7654321")
	_ = target.next(t, "devices")
	taskContext := &hub.TaskContext{
		TaskID: "q1234567", AuthorizationVersion: 3, TargetGrantVersion: 5,
		OperationID: "agent:" + strings.Repeat("b", 64),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resultCh := make(chan HubTaskCallResult, 1)
	go func() {
		resultCh <- service.CallTaskAction(ctx, "u1234567", "i7654321", taskContext, hub.ActionFiles, json.RawMessage(`{}`))
	}()
	call := target.next(t, "call")
	from, ok := call["from"].(map[string]any)
	if !ok {
		t.Fatalf("task Hub call has no caller context: %#v", call)
	}
	if _, exists := from["task_revision"]; exists {
		t.Fatalf("legacy Task action unexpectedly includes task_revision: %#v", from)
	}
	target.push(t, map[string]any{"type": "result", "id": call["id"], "ok": true,
		"body": map[string]any{"entries": []any{}, "target_grant_version": 6}})
	select {
	case result := <-resultCh:
		if !result.Unknown || result.OK || result.Error != "unknown_result" {
			t.Fatalf("mismatched grant result = %#v", result)
		}
	case <-ctx.Done():
		t.Fatal("task Hub call did not report the mismatched grant")
	}
	if got := target.next(t, "error"); got["code"] != "bad_result" {
		t.Fatalf("target grant mismatch response = %#v", got)
	}
}

func TestHubTaskFileFetchRequiresCallerOperationAndRevisionToMatchArgs(t *testing.T) {
	service, _ := newHubFixture(t)
	target := connectHubTestPeer(t, service, "u1234567", "i7654321")
	_ = target.next(t, "devices")
	expiresAt := time.Now().UTC().Add(2 * time.Minute)
	for name, test := range map[string]struct {
		operationID  string
		revision     int64
		argsRevision int64
		fetchID      string
	}{
		"mismatched fetch ID": {operationID: "o1234567", revision: 7, argsRevision: 7, fetchID: "o7654321"},
		"mismatched revision": {operationID: "o1234567", revision: 7, argsRevision: 8, fetchID: "o1234567"},
	} {
		t.Run(name, func(t *testing.T) {
			taskContext := &hub.TaskContext{
				TaskID: "q1234567", AuthorizationVersion: 3, TargetGrantVersion: 5,
				OperationID: test.operationID, TaskRevision: test.revision,
			}
			args, err := json.Marshal(hub.FileFetchArgs{
				FetchID: test.fetchID, Path: "reports/summary.pdf", TaskRevision: test.argsRevision,
				Ticket: strings.Repeat("A", 43), TicketExpiresAt: expiresAt,
			})
			if err != nil {
				t.Fatal(err)
			}
			result := service.CallTaskAction(context.Background(), "u1234567", "i7654321", taskContext, hub.ActionFileFetch, args)
			if result.OK || result.Unknown || result.Error != "invalid_argument" {
				t.Fatalf("mismatched Task file.fetch binding result = %#v", result)
			}
		})
	}
	if frame := target.maybeNext(); frame != nil {
		t.Fatalf("mismatched Task file.fetch binding reached the target: %#v", frame)
	}
}
