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
		OperationID: "agent:" + strings.Repeat("a", 64),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resultCh := make(chan HubTaskCallResult, 1)
	go func() {
		resultCh <- service.CallTaskAction(ctx, "u1234567", "i7654321", taskContext, hub.ActionFiles, json.RawMessage(`{}`))
	}()

	call := target.next(t, "call")
	from, ok := call["from"].(map[string]any)
	if !ok || from["kind"] != "task" || from["task_id"] != taskContext.TaskID ||
		from["authorization_version"] != float64(taskContext.AuthorizationVersion) ||
		from["target_grant_version"] != float64(taskContext.TargetGrantVersion) ||
		from["operation_id"] != taskContext.OperationID || call["action"] != hub.ActionFiles {
		t.Fatalf("task Hub call context = %#v", call)
	}
	target.push(t, map[string]any{"type": "result", "id": call["id"], "ok": true,
		"body": map[string]any{"entries": []any{}, "target_grant_version": 5}})
	select {
	case result := <-resultCh:
		if !result.OK || result.Unknown || string(result.Body) != `{"entries":[],"target_grant_version":5}` {
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
