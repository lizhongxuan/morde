package worker

import (
	"bytes"
	"context"
	"testing"

	"morde/server/internal/domain/task"
)

type cloudDSHProjectContextTestStore struct {
	context *task.ConversationContext
	err     error
	pauses  int
}

func (s cloudDSHProjectContextTestStore) LoadProjectTaskContext(context.Context, task.Task) (*task.ConversationContext, error) {
	return s.context, s.err
}

func (s *cloudDSHProjectContextTestStore) PauseProjectTaskRecovery(context.Context, task.Task) error {
	s.pauses++
	return nil
}

func TestCloudDSHProjectRecoveryRequiresVerifiedContextBeforeRuntime(t *testing.T) {
	for _, mode := range []string{"missing provider", "wrong context"} {
		t.Run(mode, func(t *testing.T) {
			executor, _, instances, _, models, value := cloudDSHFixture(t)
			value.ProjectID = "project_123"
			value.AgentInstanceID, value.AgentGeneration, value.AgentSessionID = "agent_old", 1, "session_old"
			value.AgentEngineVersion, value.AgentProfileVersion = cloudDSHEngineVersion, cloudDSHProfileVersion
			value.CheckpointThroughSequence, value.CheckpointDigest = 0, bytes.Repeat([]byte{1}, 32)
			value.ConversationContext = &task.ConversationContext{OwnerID: value.OwnerID, InitiatorID: value.InitiatorID,
				ProjectID: value.ProjectID, ConversationID: value.ConversationID, Scope: "project"}
			var projectStore *cloudDSHProjectContextTestStore
			if mode == "wrong context" {
				projectStore = &cloudDSHProjectContextTestStore{context: &task.ConversationContext{
					OwnerID: value.OwnerID, InitiatorID: value.InitiatorID, ProjectID: "other_project",
					ConversationID: value.ConversationID, Scope: "project"}}
				executor.options.ProjectContexts = projectStore
			}
			if err := executor.ExecuteTask(context.Background(), value); err == nil || instances.generation != 0 || len(models.calls) != 0 {
				t.Fatalf("unverified project recovery started: err=%v generation=%d calls=%d", err, instances.generation, len(models.calls))
			}
			if projectStore != nil && projectStore.pauses != 1 {
				t.Fatalf("unverified project recovery was not paused: calls=%d", projectStore.pauses)
			}
		})
	}
}
