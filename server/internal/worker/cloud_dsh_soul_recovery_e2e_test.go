package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
)

func TestCloudDSHRecoversLegacySoulAndReconcilesToolThroughWorker(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "runtime", "morde-cloud-runtime", "test", "fixtures", "dsh-session-0.2.0-rc.2-pending-mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Tool struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"tool"`
		Pages []struct {
			From    int64  `json:"from_seq"`
			Through int64  `json:"through_seq"`
			Payload string `json:"payload"`
			SHA256  string `json:"sha256"`
		} `json:"pages"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil || len(fixture.Pages) == 0 {
		t.Fatalf("invalid legacy fixture: %v", err)
	}
	executor, _, instances, checkpoints, models, value := cloudDSHFixture(t)
	value.AgentInstanceID, value.AgentGeneration, value.AgentSessionID = "legacy_agent", 1, "session_456"
	value.AgentEngineVersion, value.AgentProfileVersion = cloudDSHEngineVersion, cloudDSHProfileVersion
	instances.generation, instances.wasStopped = 1, true
	instances.active = app.AgentInstanceFence{AgentInstanceID: value.AgentInstanceID, AgentSessionID: value.AgentSessionID,
		EngineVersion: value.AgentEngineVersion, ProfileVersion: value.AgentProfileVersion}
	var modelIDsReplaced int
	for _, page := range fixture.Pages {
		digest, err := hex.DecodeString(page.SHA256)
		originalDigest := sha256.Sum256([]byte(page.Payload))
		if err != nil || !bytes.Equal(digest, originalDigest[:]) {
			t.Fatalf("legacy checkpoint digest is invalid: %v", err)
		}
		oldModel, newModel := []byte(`"model":"test-model"`), []byte(`"model":"p1234567"`)
		modelIDsReplaced += bytes.Count([]byte(page.Payload), oldModel)
		payload := bytes.ReplaceAll([]byte(page.Payload), oldModel, newModel)
		currentDigest := sha256.Sum256(payload)
		checkpoints.commands = append(checkpoints.commands, app.AgentCheckpointCommand{
			Fence: app.AgentCheckpointFence{AgentSessionID: value.AgentSessionID}, From: page.From, Through: page.Through,
			Digest: currentDigest[:], Payload: payload,
		})
	}
	if modelIDsReplaced != 3 {
		t.Fatalf("legacy fixture model IDs replaced = %d, want 3", modelIDsReplaced)
	}
	tail := checkpoints.commands[len(checkpoints.commands)-1]
	value.CheckpointThroughSequence, value.CheckpointDigest = tail.Through, tail.Digest
	value.Lease.Version++
	expires := time.Now().Add(time.Minute)
	value.Lease.ExpiresAt = &expires
	tools := &cloudDSHTestTools{definitions: []app.AgentToolDefinition{{ModelTool: app.ModelTool{
		Name: fixture.Tool.Name, Description: fixture.Tool.Description, Parameters: fixture.Tool.Parameters,
	}}}, result: app.AgentToolResult{Status: app.AgentToolSucceeded, Output: "committed once"}}
	executor.options.Tools = tools
	if err := executor.ExecuteTask(context.Background(), value); !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("legacy checkpoint recovery: %v (executions=%d reconciliations=%d models=%d saves=%d)", err,
			len(tools.calls), len(tools.recoveryCalls), len(models.calls), len(checkpoints.commands)-len(fixture.Pages))
	}
	if len(tools.calls) != 0 || len(tools.recoveryCalls) != 1 || len(models.calls) != 1 ||
		tools.recoveryCalls[0].CallID != "call_migration_1" ||
		tools.recoveryCalls[0].Name != fixture.Tool.Name || tools.recoveryCalls[0].Arguments != `{"message":"write once"}` ||
		!strings.Contains(string(models.calls[0].RawRequest), "committed once") {
		t.Fatalf("legacy recovery did not reconcile the expected call: executions=%d reconciliations=%+v models=%d",
			len(tools.calls), tools.recoveryCalls, len(models.calls))
	}
	var replacement bool
	for _, command := range checkpoints.commands[len(fixture.Pages):] {
		var page struct {
			Events []struct {
				Type      string          `json:"type"`
				Sequence  int64           `json:"seq"`
				SurfaceOp json.RawMessage `json:"surfaceOp"`
				Sources   []int64         `json:"sourceEventSeqs"`
			} `json:"events"`
		}
		if err := json.Unmarshal(command.Payload, &page); err != nil {
			t.Fatal(err)
		}
		for _, event := range page.Events {
			var operation struct {
				Op    string `json:"op"`
				Start int64  `json:"startSeq"`
				End   int64  `json:"endSeq"`
			}
			if event.Type == "system/message" && json.Unmarshal(event.SurfaceOp, &operation) == nil &&
				operation.Op == "replace" && operation.Start == 4 && operation.End == 4 &&
				event.Sequence > 4 && len(event.Sources) == 1 && event.Sources[0] == 4 {
				replacement = true
			}
		}
	}
	if !replacement || strings.Count(string(models.calls[0].RawRequest), "You are Morde, the user's AI assistant.") != 1 {
		t.Fatal("recovery did not replace the legacy system message with one current Morde identity")
	}
}
