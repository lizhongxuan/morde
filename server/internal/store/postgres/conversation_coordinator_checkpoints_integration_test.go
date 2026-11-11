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
