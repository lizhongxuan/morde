package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"morde/server/internal/app"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"
)

type cloudDSHPersonalWriteReceiptReader interface {
	ConfirmPersonalContextWrite(context.Context, task.Task, app.AgentInstanceFence, int64, []byte, app.ModelToolCall) (app.CloudDSHMemoryVersion, string, error)
}

type cloudDSHRecoveredPersonalWrite struct {
	name      string
	arguments []byte
	version   app.CloudDSHMemoryVersion
	output    string
}

type cloudDSHRecoveredPersonalWrites map[string]map[string]cloudDSHRecoveredPersonalWrite

func cloudDSHMayMutatePersonalContext(name string) bool {
	return name == "morde_personal_object_update" || name == "morde_goal_create" || name == "morde_goal_update" || name == "morde_goal_delete"
}

func (e *CloudDSHExecutor) confirmCloudDSHPersonalWrites(ctx context.Context, claimed task.Task, fence app.AgentInstanceFence,
	through int64, digest []byte, history *cloudDSHCheckpointHistory) (cloudDSHRecoveredPersonalWrites, error) {
	confirmed := make(cloudDSHRecoveredPersonalWrites)
	for callID, call := range history.calls {
		if !cloudDSHMayMutatePersonalContext(call.name) || call.status != "" {
			continue
		}
		reader, ok := e.options.Tools.(cloudDSHPersonalWriteReceiptReader)
		if !ok || !call.callSeen || len(confirmed[history.sessionID]) >= 100 {
			return nil, errors.New("personal context write has no provable receipt")
		}
		version, output, err := reader.ConfirmPersonalContextWrite(ctx, claimed, fence, through, digest,
			app.ModelToolCall{CallID: callID, Name: call.name, Arguments: string(call.arguments)})
		if err != nil || output == "" || call.resultSeen && call.resultText != output {
			return nil, errors.New("personal context write receipt could not be reconciled")
		}
		if confirmed[history.sessionID] == nil {
			confirmed[history.sessionID] = make(map[string]cloudDSHRecoveredPersonalWrite)
		}
		confirmed[history.sessionID][callID] = cloudDSHRecoveredPersonalWrite{name: call.name,
			arguments: append([]byte(nil), call.arguments...), version: version, output: output}
	}
	return confirmed, nil
}

func cloudDSHRecoveredPersonalReceipt(receipts cloudDSHRecoveredPersonalWrites, history *cloudDSHCheckpointHistory,
	callID string, call *cloudDSHToolCall) (app.CloudDSHMemoryVersion, string, bool) {
	receipt, ok := receipts[history.sessionID][callID]
	if !ok || receipt.name != call.name || !bytes.Equal(receipt.arguments, call.arguments) ||
		call.resultSeen && call.resultText != receipt.output {
		return app.CloudDSHMemoryVersion{}, "", false
	}
	if call.name == "morde_goal_create" || call.name == "morde_goal_update" || call.name == "morde_goal_delete" {
		var arguments struct {
			GoalID           string `json:"goal_id"`
			ExpectedRevision int64  `json:"expected_revision"`
		}
		var output struct {
			ID       string `json:"id"`
			Revision int64  `json:"revision"`
			Deleted  bool   `json:"deleted"`
		}
		if json.Unmarshal(call.arguments, &arguments) != nil || json.Unmarshal([]byte(receipt.output), &output) != nil ||
			receipt.version.ObjectType != personal.ObjectTypeGoal || receipt.version.ID != output.ID ||
			receipt.version.Revision != output.Revision || personal.ValidateObjectIdentifier(personal.ObjectTypeGoal, output.ID) != nil {
			return app.CloudDSHMemoryVersion{}, "", false
		}
		if call.name == "morde_goal_create" {
			if output.Revision != 1 || output.Deleted {
				return app.CloudDSHMemoryVersion{}, "", false
			}
		} else if arguments.GoalID != output.ID || arguments.ExpectedRevision < 1 || arguments.ExpectedRevision >= 1<<53-1 ||
			output.Revision != arguments.ExpectedRevision+1 || output.Deleted != (call.name == "morde_goal_delete") {
			return app.CloudDSHMemoryVersion{}, "", false
		}
		return receipt.version, receipt.output, true
	}
	if call.name != "morde_personal_object_update" {
		return app.CloudDSHMemoryVersion{}, "", false
	}
	var arguments struct {
		ObjectType   string `json:"object_type"`
		ObjectID     string `json:"object_id"`
		BaseRevision int64  `json:"base_revision"`
	}
	var output struct {
		ObjectType string `json:"object_type"`
		ObjectID   string `json:"object_id"`
		Revision   int64  `json:"revision"`
	}
	if json.Unmarshal(call.arguments, &arguments) != nil || json.Unmarshal([]byte(receipt.output), &output) != nil ||
		arguments.BaseRevision < 0 || arguments.BaseRevision >= 1<<53-1 ||
		personal.ValidateObjectIdentifier(arguments.ObjectType, arguments.ObjectID) != nil ||
		receipt.version.ObjectType != arguments.ObjectType || receipt.version.ID != arguments.ObjectID ||
		receipt.version.Revision != arguments.BaseRevision+1 || output.ObjectType != receipt.version.ObjectType ||
		output.ObjectID != receipt.version.ID || output.Revision != receipt.version.Revision {
		return app.CloudDSHMemoryVersion{}, "", false
	}
	return receipt.version, receipt.output, true
}
