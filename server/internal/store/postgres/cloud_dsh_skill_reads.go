package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"

	"github.com/jackc/pgx/v5"
)

const (
	cloudDSHSkillReadToolName = "nanomuse_skill_read"
)

type cloudDSHSkillReadEventDetail struct {
	Tool                  string `json:"tool"`
	SkillCallID           string `json:"skill_call_id"`
	AgentSessionID        string `json:"agent_session_id"`
	AuthorizationVersion  int64  `json:"authorization_version"`
	SkillID               string `json:"skill_id"`
	SkillVersion          string `json:"skill_version"`
	PersonalSkillRevision int64  `json:"personal_skill_revision"`
	BundleVersion         string `json:"bundle_version"`
	SourceRef             string `json:"source_ref"`
	ContentSHA256         string `json:"content_sha256"`
	OutputSHA256          string `json:"output_sha256"`
	FingerprintSHA256     string `json:"fingerprint_sha256"`
}

func findCloudDSHSkillReadEvent(ctx context.Context, tx pgx.Tx, taskID, sessionID, callID string) (task.Event, bool, error) {
	var value task.Event
	err := tx.QueryRow(ctx, `SELECT task_id,sequence,event_type,payload,created_at FROM task_events
WHERE task_id=$1 AND event_type='task.step' AND payload #>> '{detail,agent_session_id}'=$2
AND payload #>> '{detail,skill_call_id}'=$3 ORDER BY sequence LIMIT 1`, taskID, sessionID, callID).
		Scan(&value.TaskID, &value.Sequence, &value.Type, &value.Payload, &value.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// A reused call ID from another session is a conflict, not a new call.
		err = tx.QueryRow(ctx, `SELECT task_id,sequence,event_type,payload,created_at FROM task_events
WHERE task_id=$1 AND event_type='task.step' AND payload #>> '{detail,skill_call_id}'=$2
ORDER BY sequence LIMIT 1`, taskID, callID).Scan(&value.TaskID, &value.Sequence, &value.Type, &value.Payload, &value.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return task.Event{}, false, nil
		}
	}
	if err != nil {
		return task.Event{}, false, fmt.Errorf("find prior cloud dsh skill event: %w", err)
	}
	return value, true, nil
}

func validateCloudDSHSkillReadEvent(payload []byte, expected app.CloudDSHSkillReadCommit) error {
	if len(payload) == 0 || len(payload) > app.MaxCloudDSHSkillEventBytes {
		return domain.NewError(domain.CodeInvalidState, "cloud dsh skill event size is invalid")
	}
	var value struct {
		Detail cloudDSHSkillReadEventDetail `json:"detail"`
	}
	if json.Unmarshal(payload, &value) != nil || value.Detail.Tool != cloudDSHSkillReadToolName ||
		value.Detail.SkillCallID != expected.CallID || value.Detail.AgentSessionID != expected.AgentSessionID ||
		value.Detail.AuthorizationVersion != expected.AuthorizationVersion || value.Detail.SkillID != expected.SkillID ||
		value.Detail.SkillVersion != expected.SkillVersion || value.Detail.BundleVersion != expected.BundleVersion ||
		value.Detail.PersonalSkillRevision != expected.PersonalSkillRevision ||
		value.Detail.SourceRef != expected.SourceRef || value.Detail.ContentSHA256 != expected.ContentSHA256 ||
		value.Detail.OutputSHA256 != expected.OutputSHA256 || value.Detail.FingerprintSHA256 != expected.FingerprintSHA256 {
		return domain.NewError(domain.CodeInvalidState, "cloud dsh skill event fingerprint changed")
	}
	return nil
}

func validCloudDSHSkillReadCommit(value app.CloudDSHSkillReadCommit) bool {
	validSource := false
	if value.PersonalSkillRevision > 0 {
		skillVersion := strconv.FormatInt(value.PersonalSkillRevision, 10)
		validSource = personal.ValidSkillCandidateID(value.SkillID) && value.SkillVersion == skillVersion &&
			value.BundleVersion == "morde-personal-skill-v1" &&
			value.SourceRef == "personal://"+value.SkillID+"/revision/"+skillVersion
	} else if skill, ok := app.CloudDSHSkillMetadataForID(value.SkillID); ok {
		validSource = value.SkillVersion == skill.Version && value.BundleVersion == "nanomuse-builtin-v1" &&
			value.SourceRef == skill.SourceRef
	}
	if !domain.ValidTaskID(value.TaskID) || !domain.ValidUserID(value.OwnerID) || value.AgentSessionID == "" ||
		len(value.AgentSessionID) > 128 || !utf8.ValidString(value.AgentSessionID) ||
		value.AuthorizationVersion < 1 || value.CallID == "" || len(value.CallID) > 256 || !utf8.ValidString(value.CallID) ||
		!validSource ||
		!validPostgresSHA256(value.ContentSHA256) || !validPostgresSHA256(value.OutputSHA256) || !validPostgresSHA256(value.FingerprintSHA256) {
		return false
	}
	return value.FingerprintSHA256 == cloudDSHSkillReadFingerprint(value)
}

func validateCloudDSHPersonalSkillReadInTx(ctx context.Context, tx pgx.Tx, value app.CloudDSHSkillReadCommit) error {
	if value.PersonalSkillRevision == 0 {
		return nil
	}
	instanceID, err := personalServiceInstanceIDInTx(ctx, tx)
	if err != nil {
		return err
	}
	var body []byte
	err = tx.QueryRow(ctx, `SELECT COALESCE(body::text,'') FROM personal_objects
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND object_type=$3 AND object_id=$4 FOR SHARE`,
		instanceID, value.OwnerID, personal.ObjectTypeSkillCandidate, value.SkillID).Scan(&body)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeInvalidState, "personal skill was removed before it could be loaded")
		}
		return fmt.Errorf("lock personal skill object for Task commit: %w", err)
	}
	var revision int64
	var deleted bool
	err = tx.QueryRow(ctx, `SELECT revision,is_deleted FROM account_object_versions
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND object_type=$3 AND object_id=$4 FOR SHARE`,
		instanceID, value.OwnerID, personal.ObjectTypeSkillCandidate, value.SkillID).Scan(&revision, &deleted)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeInvalidState, "personal skill revision was removed before it could be loaded")
		}
		return fmt.Errorf("lock personal skill revision for Task commit: %w", err)
	}
	var candidate personal.SkillCandidateBody
	if deleted || revision != value.PersonalSkillRevision ||
		personal.ValidateObjectBody(personal.ObjectTypeSkillCandidate, value.SkillID, body) != nil || json.Unmarshal(body, &candidate) != nil ||
		candidate.Scope != "personal" || candidate.Status != personal.SkillCandidateActive {
		return domain.NewError(domain.CodeInvalidState, "personal skill changed or was revoked before it could be loaded")
	}
	hash := sha256.Sum256([]byte(candidate.Content))
	if hex.EncodeToString(hash[:]) != value.ContentSHA256 {
		return domain.NewError(domain.CodeInvalidState, "personal skill content fingerprint changed")
	}
	return nil
}

func cloudDSHSkillReadFingerprint(value app.CloudDSHSkillReadCommit) string {
	value.FingerprintSHA256 = ""
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}
