package postgres

import (
	"context"
	"crypto/sha256"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func TestConversationCoordinatorCheckpointsBindContextAndProtectPageSequence(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	claim := f.receive(t, "coordinator-checkpoint-context-binding", "Continue the discussion", app.ConversationRoutingInput{})
	identity := app.ConversationCoordinatorIdentity{ServiceInstanceID: claim.ServiceInstanceID, OwnerID: claim.Message.OwnerID,
		ActorID: claim.Message.ActorID, ConversationID: claim.Message.ConversationID, MessageID: claim.Message.ID,
		AgentID: "coord_12345678", Generation: 1, AuthorizationVersion: claim.Authorization.Version, SessionID: "coord_session_12345678",
		Purpose: "conversation"}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewConversationCoordinatorCheckpointService(f.store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	contextDigest := sha256.Sum256([]byte("authorized route context v1"))
	changedContextDigest := sha256.Sum256([]byte("authorized route context v2"))
	firstPayload := []byte(`{"events":[{"seq":0,"type":"message"}]}`)
	firstDigest := sha256.Sum256(firstPayload)
	first := app.ConversationCoordinatorCheckpointCommand{Claim: claim, Identity: identity, ContextDigest: contextDigest[:],
		From: 0, Through: 0, Digest: firstDigest[:], Payload: firstPayload}
	if replayed, err := service.Save(ctx, first); err != nil || replayed {
		t.Fatalf("save initial checkpoint: replayed=%t err=%v", replayed, err)
	}
	head, err := service.Head(ctx, claim, identity, contextDigest[:])
	if err != nil || head.Through != 0 || string(head.Digest) != string(firstDigest[:]) || string(head.ContextDigest) != string(contextDigest[:]) {
		t.Fatalf("read bound checkpoint head: head=%+v err=%v", head, err)
	}
	page, err := service.ReadPage(ctx, app.ConversationCoordinatorCheckpointPageRequest{Claim: claim, Identity: identity,
		ContextDigest: contextDigest[:], From: 0, Through: 0, ThroughDigest: firstDigest[:]})
	if err != nil || page.From != 0 || page.Through != 0 || string(page.Payload) != string(firstPayload) {
		t.Fatalf("restore matching checkpoint page: page=%+v err=%v", page, err)
	}
	otherSession := identity
	otherSession.SessionID = "coord_session_other"
	otherSessionHead, err := service.Head(ctx, claim, otherSession, contextDigest[:])
	if err != nil || otherSessionHead.Through != -1 || len(otherSessionHead.Digest) != 0 {
		t.Fatalf("checkpoint head leaked into another session: head=%+v err=%v", otherSessionHead, err)
	}
	if _, err := service.ReadPage(ctx, app.ConversationCoordinatorCheckpointPageRequest{Claim: claim, Identity: otherSession,
		ContextDigest: contextDigest[:], From: 0, Through: 0, ThroughDigest: firstDigest[:]}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("checkpoint page leaked into another session: %v", err)
	}
	foreignOwner := identity
	foreignOwnerID := "u7654321"
	if foreignOwnerID == claim.Message.OwnerID {
		foreignOwnerID = "u7654322"
	}
	foreignOwner.OwnerID, foreignOwner.ActorID = foreignOwnerID, foreignOwnerID
	if _, err := service.Head(ctx, claim, foreignOwner, contextDigest[:]); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("checkpoint accepted a different owner identity: %v", err)
	}
	if replayed, err := service.Save(ctx, first); err != nil || !replayed {
		t.Fatalf("same page retry: replayed=%t err=%v", replayed, err)
	}

	secondPayload := []byte(`{"events":[{"seq":1,"type":"message"}]}`)
	secondDigest := sha256.Sum256(secondPayload)
	outOfOrder := app.ConversationCoordinatorCheckpointCommand{Claim: claim, Identity: identity, ContextDigest: contextDigest[:],
		From: 2, Through: 2, Digest: secondDigest[:], Payload: secondPayload}
	if _, err := service.Save(ctx, outOfOrder); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("out-of-order checkpoint page = %v", err)
	}
	changedDuplicatePayload := []byte(`{"events":[{"seq":0,"type":"changed"}]}`)
	changedDuplicateDigest := sha256.Sum256(changedDuplicatePayload)
	changedDuplicate := first
	changedDuplicate.Payload, changedDuplicate.Digest = changedDuplicatePayload, changedDuplicateDigest[:]
	if _, err := service.Save(ctx, changedDuplicate); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("duplicate sequence with changed payload = %v", err)
	}

	if _, err := service.Head(ctx, claim, identity, changedContextDigest[:]); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("changed context head = %v", err)
	}
	if _, err := service.ReadPage(ctx, app.ConversationCoordinatorCheckpointPageRequest{Claim: claim, Identity: identity,
		ContextDigest: changedContextDigest[:], From: 0, Through: 0, ThroughDigest: firstDigest[:]}); err == nil {
		t.Fatal("restored checkpoint under a changed context")
	}
	changedNextPage := app.ConversationCoordinatorCheckpointCommand{Claim: claim, Identity: identity, ContextDigest: changedContextDigest[:],
		From: 1, Through: 1, Digest: secondDigest[:], Payload: secondPayload}
	if _, err := service.Save(ctx, changedNextPage); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("save under changed route context = %v", err)
	}

	secondPage := app.ConversationCoordinatorCheckpointCommand{Claim: claim, Identity: identity, ContextDigest: contextDigest[:],
		From: 1, Through: 1, Digest: secondDigest[:], Payload: secondPayload}
	if replayed, err := service.Save(ctx, secondPage); err != nil || replayed {
		t.Fatalf("save contiguous checkpoint page: replayed=%t err=%v", replayed, err)
	}
	newHead, err := service.Head(ctx, claim, identity, contextDigest[:])
	if err != nil || newHead.Through != 1 || string(newHead.Digest) != string(secondDigest[:]) {
		t.Fatalf("advance checkpoint head: head=%+v err=%v", newHead, err)
	}
	firstRestored, err := service.ReadPage(ctx, app.ConversationCoordinatorCheckpointPageRequest{Claim: claim, Identity: identity,
		ContextDigest: contextDigest[:], From: 0, Through: 1, ThroughDigest: secondDigest[:]})
	if err != nil || string(firstRestored.Payload) != string(firstPayload) {
		t.Fatalf("restore prior page in matching chain: page=%+v err=%v", firstRestored, err)
	}
}

func TestConversationCoordinatorCheckpointsAreIsolatedAcrossConversations(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	firstClaim := f.receive(t, "coordinator-checkpoint-session-one", "First conversation", app.ConversationRoutingInput{})
	secondConversation := createMainConversationForTest(t, f.store, f.ownerID)
	secondClaim := f.receiveInConversation(t, secondConversation.ID, "coordinator-checkpoint-session-two", "Second conversation", app.ConversationRoutingInput{})
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewConversationCoordinatorCheckpointService(f.store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	contextDigest := sha256.Sum256([]byte("authorized route context"))

	type checkpoint struct {
		claim    app.ConversationRouteClaim
		identity app.ConversationCoordinatorIdentity
		payload  []byte
		digest   []byte
	}
	makeCheckpoint := func(claim app.ConversationRouteClaim, session, event string) checkpoint {
		identity := app.ConversationCoordinatorIdentity{ServiceInstanceID: claim.ServiceInstanceID, OwnerID: claim.Message.OwnerID,
			ActorID: claim.Message.ActorID, ConversationID: claim.Message.ConversationID, MessageID: claim.Message.ID,
			AgentID: "coord_12345678", Generation: 1, AuthorizationVersion: claim.Authorization.Version, SessionID: session,
			Purpose: "conversation"}
		payload := []byte(`{"events":[{"type":"message","text":"` + event + `"}]}`)
		digest := sha256.Sum256(payload)
		return checkpoint{claim: claim, identity: identity, payload: payload, digest: digest[:]}
	}
	checkpoints := []checkpoint{
		makeCheckpoint(firstClaim, "coord_session_12345678", "first conversation sentinel"),
		makeCheckpoint(secondClaim, "coord_session_12345678", "second conversation sentinel"),
	}
	for _, item := range checkpoints {
		if replayed, err := service.Save(ctx, app.ConversationCoordinatorCheckpointCommand{Claim: item.claim, Identity: item.identity,
			ContextDigest: contextDigest[:], From: 0, Through: 0, Digest: item.digest, Payload: item.payload}); err != nil || replayed {
			t.Fatalf("save conversation checkpoint: replayed=%t err=%v", replayed, err)
		}
	}
	for _, item := range checkpoints {
		head, err := service.Head(ctx, item.claim, item.identity, contextDigest[:])
		if err != nil || head.Through != 0 || string(head.Digest) != string(item.digest) {
			t.Fatalf("read conversation checkpoint head: head=%+v err=%v", head, err)
		}
		page, err := service.ReadPage(ctx, app.ConversationCoordinatorCheckpointPageRequest{Claim: item.claim, Identity: item.identity,
			ContextDigest: contextDigest[:], From: 0, Through: 0, ThroughDigest: item.digest})
		if err != nil || string(page.Payload) != string(item.payload) {
			t.Fatalf("restore conversation checkpoint page: page=%+v err=%v", page, err)
		}
	}
	if _, err := service.ReadPage(ctx, app.ConversationCoordinatorCheckpointPageRequest{Claim: secondClaim, Identity: checkpoints[0].identity,
		ContextDigest: contextDigest[:], From: 0, Through: 0, ThroughDigest: checkpoints[0].digest}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("checkpoint from another conversation was readable: %v", err)
	}
}
