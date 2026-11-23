package postgres

import (
	"testing"

	"morde/server/internal/domain/task"
)

func TestConversationToolActivityStatusRequiresAConfirmedToolResult(t *testing.T) {
	for _, item := range []struct {
		taskStatus task.Status
		stepStatus task.StepStatus
		dispatched bool
		unknown    bool
		want       task.StepStatus
	}{
		{task.StatusQueued, "", false, false, task.StepQueued},
		{task.StatusQueued, "", true, false, task.StepUnknown},
		{task.StatusRunning, task.StepQueued, false, false, task.StepRunning},
		{task.StatusRunning, task.StepRunning, true, true, task.StepUnknown},
		{task.StatusWaitingUser, task.StepRunning, false, false, task.StepWaitingUser},
		{task.StatusCompleted, "", false, false, task.StepUnknown},
		{task.StatusFailed, "", false, false, task.StepFailed},
		{task.StatusCancelled, "", false, false, task.StepCancelled},
		{task.StatusFailed, "", true, false, task.StepUnknown},
		{task.StatusCancelled, "", true, false, task.StepUnknown},
		{task.StatusFailed, task.StepRunning, false, false, task.StepUnknown},
		{task.StatusCancelled, task.StepRunning, false, false, task.StepUnknown},
		{task.StatusCompleted, task.StepSucceeded, true, false, task.StepSucceeded},
		{task.StatusCompleted, task.StepSucceeded, true, true, task.StepUnknown},
		{task.StatusCompleted, task.StepFailed, true, false, task.StepFailed},
		{task.StatusCompleted, task.StepUnknown, true, false, task.StepUnknown},
		{task.StatusCancelled, task.StepCancelled, false, false, task.StepCancelled},
	} {
		if got := conversationToolActivityStatus(item.taskStatus, item.stepStatus, item.dispatched, item.unknown); got != item.want {
			t.Errorf("task=%s step=%s: got %s, want %s", item.taskStatus, item.stepStatus, got, item.want)
		}
	}
}
