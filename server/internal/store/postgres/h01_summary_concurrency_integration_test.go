package postgres

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
)

func h01PrepareSummaryOperation(t *testing.T, store *Store, claim app.ConversationRouteClaim, baseline int64, owner string) app.ConversationSummaryOperation {
	t.Helper()
	ctx := context.Background()
	digest := sha256.Sum256([]byte(owner))
	operation, _, err := store.ClaimConversationSummaryOperation(ctx, app.ClaimConversationSummaryOperation{
		Claim: claim, OperationID: mustID(t, domain.NewOperationID), InputSHA256: digest[:], BaselineVersion: baseline,
		LeaseOwner: owner, LeaseDuration: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveConversationSummaryCheckpoint(ctx, app.ConversationSummaryCheckpoint{
		Claim: claim, OperationID: operation.OperationID, LeaseOwner: owner, LeaseVersion: operation.LeaseVersion,
		SessionID: "session-" + owner, Through: 1, SHA256: digest[:],
	}); err != nil {
		t.Fatal(err)
	}
	return operation
}

func h01CommitSummary(t *testing.T, store *Store, claim app.ConversationRouteClaim, identity app.ConversationSummaryIdentity,
	operation app.ConversationSummaryOperation, text string, sources []app.ConversationSummarySource) error {
	t.Helper()
	sealed, err := app.SealConversationSummary(testConversationSummarySealer(t), app.ConversationSummaryDocument{
		Identity: identity, Version: operation.BaselineVersion + 1, OperationID: operation.OperationID, Text: text, Sources: sources,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store.CommitConversationSummary(context.Background(), app.ConversationSummaryCommit{
		Claim: claim, Identity: identity, OperationID: operation.OperationID, LeaseOwner: operation.LeaseOwner,
		LeaseVersion: operation.LeaseVersion, BaselineVersion: operation.BaselineVersion,
		SummarySHA256: sealed.SummarySHA256, SourcesSHA256: sealed.SourcesSHA256, Ciphertext: sealed.Ciphertext, Sources: sources,
	})
}

func TestH01SummaryConcurrentMessageAndStaleCommit(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	prior := f.receive(t, "h01-summary-prior", "The first verified fact.", app.ConversationRoutingInput{})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, prior)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, prior, snapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteReply, Reply: "已记录。",
	}); err != nil {
		t.Fatal(err)
	}
	claim := f.receive(t, "h01-summary-current", "Continue.", app.ConversationRoutingInput{})
	priorSource := conversationSummaryMessageSource(prior.Message.ID, prior.Message.Content, prior.Message.Sequence)
	commitRouteTestSummary(t, f, claim, 0, "Previously verified summary.", []app.ConversationSummarySource{priorSource})
	head, err := f.store.ReadConversationSummaryHead(ctx, claim)
	if err != nil || head.Version != 1 {
		t.Fatalf("initial summary head=%+v err=%v", head, err)
	}
	stale := h01PrepareSummaryOperation(t, f.store, claim, 1, "h01-stale")
	late := f.receive(t, "h01-summary-concurrent", "This arrived while the summary was running.", app.ConversationRoutingInput{})
	if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_summary_heads SET lease_expires_at=clock_timestamp()-interval '1 second'
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND scope_kind=$5 AND scope_id=$6`,
		head.Identity.ServiceInstanceID, head.Identity.OwnerID, head.Identity.ActorID, head.Identity.ConversationID,
		head.Identity.ScopeKind, head.Identity.ScopeID); err != nil {
		t.Fatal(err)
	}
	winner := h01PrepareSummaryOperation(t, f.store, claim, 1, "h01-winner")
	lateSource := conversationSummaryMessageSource(late.Message.ID, late.Message.Content, late.Message.Sequence)
	if err := h01CommitSummary(t, f.store, claim, head.Identity, winner, "Impossible future coverage.",
		[]app.ConversationSummarySource{priorSource, lateSource}); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("a later message was accepted as covered: %v", err)
	}
	if err := h01CommitSummary(t, f.store, claim, head.Identity, winner, "Current verified summary.",
		[]app.ConversationSummarySource{priorSource}); err != nil {
		t.Fatal(err)
	}
	if err := h01CommitSummary(t, f.store, claim, head.Identity, stale, "Stale summary.",
		[]app.ConversationSummarySource{priorSource}); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale worker overwrote the winning summary: %v", err)
	}
	stored, found, err := f.store.ReadConversationSummary(ctx, claim)
	if err != nil || !found || stored.Version != 2 || len(stored.Sources) != 1 || stored.Sources[0].ID != prior.Message.ID {
		t.Fatalf("winning summary was not preserved: found=%t summary=%+v err=%v", found, stored, err)
	}
	opened, err := app.OpenConversationSummary(testConversationSummarySealer(t), stored)
	if err != nil || opened.Text != "Current verified summary." {
		t.Fatalf("winning summary content=%q err=%v", opened.Text, err)
	}
	var raw string
	if err := f.store.pool.QueryRow(ctx, `SELECT content FROM conversation_messages WHERE id=$1`, late.Message.ID).Scan(&raw); err != nil || raw != late.Message.Content {
		t.Fatalf("concurrent message was changed or lost: %q err=%v", raw, err)
	}
}

func TestH01SummaryCommitRechecksProjectMembership(t *testing.T) {
	f := newProjectAttachmentFixture(t)
	ctx := context.Background()
	const priorText = "A member's earlier fact."
	priorID := projectHistoryInsertMessage(t, f, f.member.UserID, "", "", priorText, conversation.RoleUser)
	var sequence int64
	if err := f.store.pool.QueryRow(ctx, `SELECT sequence FROM conversation_messages WHERE id=$1`, priorID).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	claim := projectHistoryClaim(t, f, "h01-summary-membership")
	source := conversationSummaryMessageSource(priorID, priorText, sequence)
	commitProjectRouteTestSummary(t, f.store, claim, "Previously authorized summary.", []app.ConversationSummarySource{source})
	head, err := f.store.ReadConversationSummaryHead(ctx, claim)
	if err != nil || head.Version != 1 {
		t.Fatalf("initial project summary head=%+v err=%v", head, err)
	}
	operation := h01PrepareSummaryOperation(t, f.store, claim, 1, "h01-revoked")
	project, err := f.store.GetProject(ctx, f.owner.UserID, f.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.RemoveProjectMember(ctx, f.owner.UserID, f.projectID, f.member.UserID, project.Version,
		"h01-summary-member-revoked"); err != nil {
		t.Fatal(err)
	}
	if err := h01CommitSummary(t, f.store, claim, head.Identity, operation, "Stale authorized summary.",
		[]app.ConversationSummarySource{source}); err == nil {
		t.Fatal("summary commit succeeded after membership revocation")
	}
	var versionCount int
	var state, raw string
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*),max(state) FROM conversation_summary_versions WHERE owner_id=$1 AND actor_id=$2
AND conversation_id=$3 AND scope_kind='project' AND scope_id=$4`, f.owner.UserID, f.member.UserID, f.conversationID, f.projectID).
		Scan(&versionCount, &state); err != nil || versionCount != 1 || state != "invalid" {
		t.Fatalf("revoked summary state: count=%d state=%q err=%v", versionCount, state, err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT content FROM conversation_messages WHERE id=$1`, priorID).Scan(&raw); err != nil || raw != priorText {
		t.Fatalf("revoked source was changed or lost: %q err=%v", raw, err)
	}
}

func TestH01SummaryScopeVersionChangeInvalidatesOldSummaryAndReopensSources(t *testing.T) {
	f := newProjectAttachmentFixture(t)
	ctx := context.Background()
	prior := projectHistoryClaim(t, f, "h01-summary-scope-prior")
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, prior)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, prior, snapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteReply, Reply: "已记录。",
	}); err != nil {
		t.Fatal(err)
	}
	current := projectHistoryClaim(t, f, "h01-summary-scope-current")
	source := conversationSummaryMessageSource(prior.Message.ID, prior.Message.Content, prior.Message.Sequence)
	commitProjectRouteTestSummary(t, f.store, current, "此前已授权的摘要。", []app.ConversationSummarySource{source})
	operation := h01PrepareSummaryOperation(t, f.store, current, 1, "h01-summary-scope-stale")
	if _, err := f.store.pool.Exec(ctx, `UPDATE project_members SET version=version+1 WHERE project_id=$1 AND user_id=$2`,
		f.projectID, f.member.UserID); err != nil {
		t.Fatal(err)
	}
	page, err := f.store.LoadConversationSummaryInputPage(ctx, app.ConversationSummaryInputPageRequest{
		Claim: current, ContextWindowTokens: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.Summary != nil || page.Head.Version != 1 || page.Identity.ScopeVersion != 2 {
		t.Fatalf("changed scope reused old summary: summary=%+v head=%+v identity=%+v", page.Summary, page.Head, page.Identity)
	}
	foundPrior := false
	for _, item := range page.Sources {
		if item.Source.ID == prior.Message.ID && item.Role == string(conversation.RoleUser) {
			foundPrior = true
		}
	}
	if !foundPrior {
		t.Fatalf("old summary coverage hid authorized history after scope change: sources=%+v", page.Sources)
	}
	var summaryState, invalidReason, operationState string
	if err := f.store.pool.QueryRow(ctx, `SELECT state,invalid_reason FROM conversation_summary_versions
WHERE owner_id=$1 AND actor_id=$2 AND conversation_id=$3 AND scope_kind='project' AND scope_id=$4 AND version=1`,
		f.owner.UserID, f.member.UserID, f.conversationID, f.projectID).Scan(&summaryState, &invalidReason); err != nil {
		t.Fatal(err)
	}
	if summaryState != "invalid" || invalidReason != "scope_revoked" {
		t.Fatalf("stale summary state=%q reason=%q", summaryState, invalidReason)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT state FROM conversation_compaction_operations
WHERE owner_id=$1 AND actor_id=$2 AND conversation_id=$3 AND scope_kind='project' AND scope_id=$4 AND operation_id=$5`,
		f.owner.UserID, f.member.UserID, f.conversationID, f.projectID, operation.OperationID).Scan(&operationState); err != nil {
		t.Fatal(err)
	}
	if operationState != "invalidated" {
		t.Fatalf("stale summary operation state=%q", operationState)
	}
}
