package postgres

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/evaluation"
)

func TestRecordPCDeliveryObservationRequiresPublishedReplyAndBindsWorkflowTrace(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "pc-delivery-observation", false)
	ctx := context.Background()
	store := fixture.route.store
	ownerID := fixture.task.OwnerID
	installationID := mustID(t, domain.NewClientInstallationID)
	tokenID := mustID(t, domain.NewTokenID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO client_installations (id,owner_id,display_name,platform,expires_at)
VALUES ($1,$2,'PC delivery observation test','desktop',clock_timestamp()+interval '1 day')`, installationID, ownerID); err != nil {
		t.Fatalf("create desktop installation: %v", err)
	}
	sessionHash := sha256.Sum256([]byte("pc-delivery-observation-session"))
	if _, err := store.CreateToken(ctx, identity.Token{ID: tokenID, OwnerID: ownerID,
		ClientInstallationID: installationID, Kind: identity.KindSession,
		SecretHash: sessionHash[:], ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatalf("create installation-bound session: %v", err)
	}

	messageID := mustID(t, domain.NewMessageID)
	const replyContent = "PC delivery observation completed reply"
	replyDigest := sha256.Sum256([]byte(replyContent))
	var resultSequence, messageSequence int64
	if err := store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM task_events WHERE task_id=$1`, fixture.task.ID).Scan(&resultSequence); err != nil {
		t.Fatalf("allocate Task result event sequence: %v", err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM conversation_messages WHERE conversation_id=$1`, fixture.task.ConversationID).Scan(&messageSequence); err != nil {
		t.Fatalf("allocate assistant message sequence: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_events
(task_id,sequence,event_type,payload,task_revision,result_content_sha256)
VALUES ($1,$2,'task.result','{"status":"completed"}'::jsonb,$3,$4)`, fixture.task.ID, resultSequence,
		fixture.task.CurrentRevision, replyDigest[:]); err != nil {
		t.Fatalf("insert Task result event: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='completed',result=$2,finished_at=clock_timestamp()
WHERE id=$1`, fixture.task.ID, replyContent); err != nil {
		t.Fatalf("complete Task: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO conversation_messages
(id,conversation_id,owner_id,actor_id,role,content,task_id,sequence,delivery_status,output_status,output_task_revision,output_result_event_sequence)
VALUES ($1,$2,$3,$3,'assistant',$4,$5,$6,'applied','completed',$7,$8)`, messageID, fixture.task.ConversationID,
		ownerID, replyContent, fixture.task.ID, messageSequence, fixture.task.CurrentRevision, resultSequence); err != nil {
		t.Fatalf("insert applied assistant reply: %v", err)
	}
	tamperedMessageID := mustID(t, domain.NewMessageID)
	tamperedMessageSequence := messageSequence + 1
	if _, err := store.pool.Exec(ctx, `INSERT INTO conversation_messages
(id,conversation_id,owner_id,actor_id,role,content,task_id,sequence,delivery_status,output_status,output_task_revision,output_result_event_sequence)
VALUES ($1,$2,$3,$3,'assistant','different content than the task.result digest',$4,$5,'applied','completed',$6,$7)`, tamperedMessageID,
		fixture.task.ConversationID, ownerID, fixture.task.ID, tamperedMessageSequence, fixture.task.CurrentRevision, resultSequence); err != nil {
		t.Fatalf("insert mismatched assistant result: %v", err)
	}
	// A normal Portal session without a bound desktop installation cannot
	// create a PC-visible receipt, even when it belongs to the Task owner.
	if err := store.RecordPCDeliveryObservation(ctx, ownerID, fixture.route.authorization.TokenID,
		installationID, fixture.task.ID, messageID, *fixture.task.CurrentRevision); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("unbound session observation error = %v, want unauthenticated", err)
	}
	if err := store.RecordPCDeliveryObservation(ctx, ownerID, tokenID, installationID, fixture.task.ID,
		messageID, *fixture.task.CurrentRevision+1); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("wrong-revision observation error = %v, want not-found", err)
	}
	if err := store.RecordPCDeliveryObservation(ctx, ownerID, tokenID, installationID, fixture.task.ID,
		tamperedMessageID, *fixture.task.CurrentRevision); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("mismatched task.result digest observation error = %v, want not-found", err)
	}
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM task_pc_delivery_observations WHERE task_id=$1`, fixture.task.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected observations persisted count=%d err=%v, want zero", count, err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		if err := store.RecordPCDeliveryObservation(ctx, ownerID, tokenID, installationID, fixture.task.ID,
			messageID, *fixture.task.CurrentRevision); err != nil {
			t.Fatalf("record exact published reply observation attempt %d: %v", attempt+1, err)
		}
	}
	var observedCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM task_pc_delivery_observations
WHERE task_id=$1 AND owner_id=$2 AND client_installation_id=$3 AND message_id=$4 AND task_revision=$5`,
		fixture.task.ID, ownerID, installationID, messageID, *fixture.task.CurrentRevision).Scan(&observedCount); err != nil || observedCount != 1 {
		t.Fatalf("idempotent receipt count=%d err=%v, want one", observedCount, err)
	}

	trace, err := store.GetTaskWorkflowTrace(ctx, fixture.task.ID, ownerID)
	if err != nil {
		t.Fatalf("load workflow trace after PC observation: %v", err)
	}
	if err := evaluation.ValidateWorkflowTrace(trace); err != nil {
		t.Fatalf("validate workflow trace with PC receipt: %v", err)
	}
	var observedNode bool
	expectedDeliveryNodeID := workflowStableID("node_"+string(evaluation.WorkflowNodeDelivery),
		fixture.task.ID+":message:"+messageID)
	for _, node := range trace.Nodes {
		if node.ID == expectedDeliveryNodeID &&
			node.Kind == evaluation.WorkflowNodeDelivery && node.State == evaluation.WorkflowNodeCompleted {
			observedNode = true
		}
	}
	if !observedNode {
		t.Fatalf("workflow trace lacks completed PC-observed delivery node for message %s", messageID)
	}
	for _, gap := range trace.Gaps {
		if gap.Stage == evaluation.WorkflowStageDelivery && gap.State == evaluation.WorkflowGapUnknown &&
			gap.Scope == "PC visible-area observation is absent for published reply: "+messageID {
			t.Fatalf("workflow trace retained absent-observation gap after exact receipt: %+v", gap)
		}
	}
	var pcSource bool
	for _, source := range trace.Sources {
		if source.Kind == evaluation.WorkflowSourcePCObservation && source.Locator ==
			fmt.Sprintf("db://task_pc_delivery_observations/%s/%s/%s/%d", fixture.task.ID, messageID, installationID, *fixture.task.CurrentRevision) {
			pcSource = true
		}
	}
	if !pcSource {
		t.Fatalf("workflow trace lacks the exact PC observation source locator for %s", messageID)
	}
	tamperedDeliveryNodeID := workflowStableID("node_"+string(evaluation.WorkflowNodeDelivery),
		fixture.task.ID+":message:"+tamperedMessageID)
	var tamperedNodeUnknown bool
	var tamperedDeliveredEdge bool
	var tamperedResultGap bool
	for _, node := range trace.Nodes {
		if node.ID == tamperedDeliveryNodeID && node.State == evaluation.WorkflowNodeUnknown {
			tamperedNodeUnknown = true
		}
	}
	for _, edge := range trace.Edges {
		if edge.Kind == evaluation.WorkflowEdgeDelivered && edge.To == tamperedDeliveryNodeID {
			tamperedDeliveredEdge = true
		}
	}
	for _, gap := range trace.Gaps {
		if gap.Stage == evaluation.WorkflowStageDelivery && gap.State == evaluation.WorkflowGapUnknown &&
			gap.Scope == "assistant Task result event is missing or its content digest does not match: "+tamperedMessageID {
			tamperedResultGap = true
		}
	}
	if !tamperedNodeUnknown || tamperedDeliveredEdge || !tamperedResultGap {
		t.Fatalf("mismatched result was not kept unknown: nodeUnknown=%t deliveredEdge=%t resultGap=%t",
			tamperedNodeUnknown, tamperedDeliveredEdge, tamperedResultGap)
	}
	// A later ledger/content mismatch invalidates the previously recorded PC
	// observation as evidence for this result and must not retain a completed edge.
	changedDigest := sha256.Sum256([]byte("event digest changed after observation"))
	if _, err := store.pool.Exec(ctx, `UPDATE task_events SET result_content_sha256=$3 WHERE task_id=$1 AND sequence=$2`,
		fixture.task.ID, resultSequence, changedDigest[:]); err != nil {
		t.Fatalf("tamper result event digest after observation: %v", err)
	}
	changedTrace, err := store.GetTaskWorkflowTrace(ctx, fixture.task.ID, ownerID)
	if err != nil {
		t.Fatalf("load workflow trace after result digest mismatch: %v", err)
	}
	var changedNodeUnknown bool
	var changedDeliveredEdge bool
	var observationMismatchGap bool
	for _, node := range changedTrace.Nodes {
		if node.ID == expectedDeliveryNodeID && node.State == evaluation.WorkflowNodeUnknown {
			changedNodeUnknown = true
		}
	}
	for _, edge := range changedTrace.Edges {
		if edge.Kind == evaluation.WorkflowEdgeDelivered && edge.To == expectedDeliveryNodeID {
			changedDeliveredEdge = true
		}
	}
	for _, gap := range changedTrace.Gaps {
		if gap.Stage == evaluation.WorkflowStageDelivery && gap.State == evaluation.WorkflowGapUnknown &&
			gap.Scope == "PC observation does not match a published assistant Task result: "+messageID {
			observationMismatchGap = true
		}
	}
	if !changedNodeUnknown || changedDeliveredEdge || !observationMismatchGap {
		t.Fatalf("changed result digest retained PC delivery proof: nodeUnknown=%t deliveredEdge=%t observationMismatchGap=%t",
			changedNodeUnknown, changedDeliveredEdge, observationMismatchGap)
	}

	if _, err := store.pool.Exec(ctx, `UPDATE client_installations SET revoked_at=clock_timestamp() WHERE id=$1`, installationID); err != nil {
		t.Fatalf("revoke observed PC installation: %v", err)
	}
	if err := store.RecordPCDeliveryObservation(ctx, ownerID, tokenID, installationID, fixture.task.ID,
		messageID, *fixture.task.CurrentRevision); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked installation observation error = %v, want unauthenticated", err)
	}
}
