package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"

	"github.com/jackc/pgx/v5"
)

func TestPostgresCloudDSHTaskPlanRequirementSourceResolution(t *testing.T) {
	fixture := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := fixture.service.Start(ctx, fixture.request)
	if err != nil {
		t.Fatalf("start Task Agent: %v", err)
	}
	details, err := fixture.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil || details.Task.CurrentRevision == nil || details.Task.AgentSessionID == "" {
		t.Fatalf("load running Task: task=%+v err=%v", details.Task, err)
	}
	current := details.Task

	var originalID, originalContent, originalSourceKind string
	if err := fixture.store.pool.QueryRow(ctx, `SELECT id,content,source_kind FROM conversation_messages
WHERE task_id=$1 AND owner_id=$2 AND conversation_id=$3 AND role='user' AND delivery_status='applied'
ORDER BY sequence ASC,id ASC LIMIT 1`, current.ID, current.OwnerID, current.ConversationID).
		Scan(&originalID, &originalContent, &originalSourceKind); err != nil {
		t.Fatalf("read original Task input: %v", err)
	}
	if originalSourceKind != "task" {
		t.Fatalf("initial Task input source_kind = %q; want legitimate direct Task input kind task", originalSourceKind)
	}
	resolve := func(source task.PlanVerificationRequirementSource) task.PlanVerificationRequirementSource {
		t.Helper()
		resolved, err := fixture.store.ResolveCloudDSHTaskPlanRequirementSource(ctx, current, source)
		if err != nil {
			t.Fatalf("resolve source %+v: %v", source, err)
		}
		want := sha256.Sum256([]byte(originalContent))
		if resolved.Kind != task.PlanVerificationRequirementUserMessage || resolved.Locator != "" || resolved.MessageID != originalID ||
			resolved.ContentSHA256 != hex.EncodeToString(want[:]) {
			t.Fatalf("resolved source = %+v; want original message ID and server-computed digest", resolved)
		}
		return resolved
	}
	first := resolve(task.PlanVerificationRequirementSource{Kind: task.PlanVerificationRequirementUserMessage,
		Locator: task.PlanVerificationRequirementCurrentTaskInputLocator})
	// A bogus but syntactically valid caller digest is ignored; only the
	// database message body determines the canonical digest.
	second := resolve(task.PlanVerificationRequirementSource{Kind: task.PlanVerificationRequirementUserMessage,
		MessageID: originalID, ContentSHA256: strings.Repeat("0", 64)})
	if first != second {
		t.Fatalf("repeated source resolution changed: first=%+v second=%+v", first, second)
	}

	followup := conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: current.OwnerID, ActorID: current.InitiatorID,
		ConversationID: current.ConversationID, TaskID: current.ID, Role: conversation.RoleUser,
		Content: "A later follow-up with another verification requirement.", SourceKind: "conversation", DeliveryStatus: conversation.DeliveryApplied}
	if err := fixture.store.WithTx(ctx, func(tx pgx.Tx) error { return insertMessage(ctx, tx, followup) }); err != nil {
		t.Fatalf("insert later Task follow-up: %v", err)
	}
	stable, err := fixture.store.ResolveCloudDSHTaskPlanRequirementSource(ctx, current,
		task.PlanVerificationRequirementSource{Kind: task.PlanVerificationRequirementUserMessage,
			Locator: task.PlanVerificationRequirementCurrentTaskInputLocator})
	if err != nil || stable.MessageID != first.MessageID || stable.ContentSHA256 != first.ContentSHA256 {
		t.Fatalf("current_task_input changed after a follow-up: source=%+v err=%v", stable, err)
	}
	followupDigest := sha256.Sum256([]byte(followup.Content))
	byExactID, err := fixture.store.ResolveCloudDSHTaskPlanRequirementSource(ctx, current,
		task.PlanVerificationRequirementSource{Kind: task.PlanVerificationRequirementUserMessage, MessageID: followup.ID})
	if err != nil || byExactID.MessageID != followup.ID || byExactID.ContentSHA256 != hex.EncodeToString(followupDigest[:]) {
		t.Fatalf("exact follow-up source = %+v err=%v", byExactID, err)
	}

	variant := conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: current.OwnerID, ActorID: current.InitiatorID,
		ConversationID: current.ConversationID, TaskID: current.ID, Role: conversation.RoleUser,
		Content: originalContent, SourceKind: "conversation_variant_input", DeliveryStatus: conversation.DeliveryApplied}
	if err := fixture.store.WithTx(ctx, func(tx pgx.Tx) error { return insertMessage(ctx, tx, variant) }); err != nil {
		t.Fatalf("insert applied same-Task synthetic variant input: %v", err)
	}
	if _, err := fixture.store.ResolveCloudDSHTaskPlanRequirementSource(ctx, current,
		task.PlanVerificationRequirementSource{Kind: task.PlanVerificationRequirementUserMessage, MessageID: variant.ID}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("same-Task applied conversation_variant_input resolution error = %v; want not found", err)
	}

	other := newTestTaskSubmission(t, current.OwnerID, "", "foreign-source-task", "another task input")
	created, err := fixture.store.SubmitTask(ctx, other)
	if err != nil {
		t.Fatalf("create foreign source Task: %v", err)
	}
	var foreignMessageID string
	if err := fixture.store.pool.QueryRow(ctx, `SELECT id FROM conversation_messages WHERE task_id=$1 AND role='user' ORDER BY sequence LIMIT 1`,
		created.Task.ID).Scan(&foreignMessageID); err != nil {
		t.Fatalf("read foreign Task message ID: %v", err)
	}
	if _, err := fixture.store.ResolveCloudDSHTaskPlanRequirementSource(ctx, current,
		task.PlanVerificationRequirementSource{Kind: task.PlanVerificationRequirementUserMessage, MessageID: foreignMessageID}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-Task source resolution error = %v; want not found", err)
	}

	revision := int64(0)
	if _, err := fixture.store.ResolveCloudDSHTaskPlanRequirementSource(ctx, current,
		task.PlanVerificationRequirementSource{Kind: task.PlanVerificationRequirementProjectFile,
			WorkspacePath: "AGENTS.md", WorkspaceRevision: &revision, ContentSHA256: strings.Repeat("1", 64)}); domain.CodeOf(err) != domain.CodeUnsupported {
		t.Fatalf("unreadable immutable project source error = %v; want unsupported/fail-closed", err)
	}
}
