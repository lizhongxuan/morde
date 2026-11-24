package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"

	"github.com/jackc/pgx/v5"
)

func requireCloudDSHPersonalHistoryReadFence(ctx context.Context, tx pgx.Tx, current task.Task, currentRevision int64,
	progress app.TaskProgress) error {
	fence := progress.CloudDSHPersonalHistoryRead
	if fence == nil || !app.ValidCloudDSHPersonalHistoryReadCommit(progress) || current.ProjectID != "" ||
		current.InitiatorID != current.OwnerID || current.ExecutionLocation != "server" ||
		(current.Authorization.Kind != string(identity.KindClient) && current.Authorization.Kind != string(identity.KindSession)) ||
		current.AgentSessionID != fence.AgentSessionID || current.Authorization.Version != fence.AuthorizationVersion ||
		currentRevision != fence.TaskRevision {
		return domain.NewError(domain.CodeInvalidState, "personal conversation history consent changed before result commit")
	}
	conversationContext, err := readTaskConversationContext(ctx, tx, current)
	if err != nil {
		return err
	}
	if conversationContext == nil || conversationContext.Scope != taskConversationContextScopePersonal ||
		conversationContext.OwnerID != current.OwnerID || conversationContext.InitiatorID != current.InitiatorID ||
		conversationContext.ProjectID != "" || conversationContext.ConversationID != current.ConversationID {
		return domain.NewError(domain.CodeForbidden, "personal conversation history is outside this Task's scope")
	}
	instanceID, err := personalServiceInstanceIDInTx(ctx, tx)
	if err != nil {
		return err
	}
	// Personal mutations acquire this owner-wide counter before changing any
	// object, including memory deletion and the memory setting. Holding it
	// through this progress commit serializes the result against those changes,
	// so a deleted or newly unauthorized history result cannot be committed.
	changeWatermark, err := lockAccountChangeCounter(ctx, tx, instanceID, current.OwnerID)
	if err != nil {
		return err
	}
	if changeWatermark != fence.PersonalChangeWatermark {
		return domain.NewError(domain.CodeInvalidState, "personal history changed before result commit")
	}
	var settingRevision int64
	var deleted bool
	var body []byte
	err = tx.QueryRow(ctx, `SELECT v.revision,v.is_deleted,p.body FROM account_object_versions v
LEFT JOIN personal_objects p ON p.service_instance_id=v.service_instance_id AND p.owner_id=v.owner_id
 AND p.object_type=v.object_type AND p.object_id=v.object_id
WHERE v.service_instance_id=$1::uuid AND v.owner_id=$2 AND v.object_type=$3 AND v.object_id='builtin_memory'`,
		instanceID, current.OwnerID, personal.ObjectTypeSkillSetting).Scan(&settingRevision, &deleted, &body)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && deleted {
		if fence.SettingRevision != 0 {
			return domain.NewError(domain.CodeInvalidState, "personal conversation history consent changed before result commit")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read personal history consent during Task progress commit: %w", err)
	}
	var setting struct {
		Enabled bool `json:"enabled"`
	}
	if settingRevision != fence.SettingRevision || len(body) == 0 ||
		personal.ValidateObjectBody(personal.ObjectTypeSkillSetting, "builtin_memory", body) != nil ||
		json.Unmarshal(body, &setting) != nil || !setting.Enabled {
		return domain.NewError(domain.CodeInvalidState, "personal conversation history consent changed before result commit")
	}
	return nil
}
