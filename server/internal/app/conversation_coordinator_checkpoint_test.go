package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

type coordinatorCheckpointStoreTest struct {
	head     ConversationCoordinatorCheckpointHead
	page     StoredConversationCoordinatorCheckpoint
	claim    ConversationRouteClaim
	identity ConversationCoordinatorIdentity
}

func (s *coordinatorCheckpointStoreTest) SaveConversationCoordinatorCheckpoint(_ context.Context, claim ConversationRouteClaim,
	page StoredConversationCoordinatorCheckpoint) (bool, error) {
	if len(page.ContextDigest) != sha256.Size {
		return false, domain.InvalidArgument("coordinator_checkpoint", "context digest is invalid")
	}
	if s.head.Through >= 0 && !bytes.Equal(s.head.ContextDigest, page.ContextDigest) {
		return false, domain.NewError(domain.CodeConflict, "coordinator context changed")
	}
	s.claim, s.identity, s.page = claim, page.Identity, page
	s.head = ConversationCoordinatorCheckpointHead{Through: page.Through, Digest: append([]byte(nil), page.Digest...),
		ContextDigest: append([]byte(nil), page.ContextDigest...)}
	return false, nil
}

func (s *coordinatorCheckpointStoreTest) ReadConversationCoordinatorCheckpointHead(_ context.Context, claim ConversationRouteClaim,
	identity ConversationCoordinatorIdentity, _ []byte) (ConversationCoordinatorCheckpointHead, error) {
	s.claim, s.identity = claim, identity
	return s.head, nil
}

func (s *coordinatorCheckpointStoreTest) ReadConversationCoordinatorCheckpointPage(_ context.Context, claim ConversationRouteClaim,
	identity ConversationCoordinatorIdentity, contextDigest []byte, from, through int64, _ []byte) (StoredConversationCoordinatorCheckpoint, error) {
	s.claim, s.identity = claim, identity
	if s.page.From != from || s.page.Through > through || !bytes.Equal(s.page.ContextDigest, contextDigest) {
		return StoredConversationCoordinatorCheckpoint{}, domain.NewError(domain.CodeNotFound, "coordinator checkpoint page was not found")
	}
	return s.page, nil
}

func coordinatorCheckpointFixture() (ConversationRouteClaim, ConversationCoordinatorIdentity) {
	claim := ConversationRouteClaim{Message: conversation.Message{ID: "m1234567", ConversationID: "c1234567", OwnerID: "u1234567",
		ActorID: "u1234567", Role: conversation.RoleUser, Sequence: 1}, ServiceInstanceID: "00000000-0000-4000-8000-000000000001",
		LeaseOwner: "route-worker", LeaseVersion: 1, Authorization: task.Authorization{Version: 3}}
	identity := ConversationCoordinatorIdentity{ServiceInstanceID: claim.ServiceInstanceID, OwnerID: claim.Message.OwnerID,
		ActorID: claim.Message.ActorID, ConversationID: claim.Message.ConversationID, MessageID: claim.Message.ID,
		AgentID: "coord_12345678", Generation: 1, AuthorizationVersion: claim.Authorization.Version, SessionID: "coord_session_12345678",
		Purpose: "conversation"}
	return claim, identity
}

func TestConversationCoordinatorContextDigestIsStableAndBindsModelAndAuthorizedInput(t *testing.T) {
	value := ConversationRouteContext{ActorID: "u1234567", ModelID: "p1234567", Content: "hello",
		History: []ConversationRouteHistory{{MessageID: "m7654321", Sequence: 1, Role: conversation.RoleUser, Content: "prior"}},
		Candidates: []ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567", Objective: "work",
			Status: task.StatusRunning, Revision: 2}}}
	first, err := ConversationCoordinatorContextDigest(value)
	if err != nil || len(first) != sha256.Size {
		t.Fatalf("context digest = %x, %v", first, err)
	}
	second, err := ConversationCoordinatorContextDigest(value)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("identical context digest changed: %x / %x, %v", first, second, err)
	}
	changedHistory := value
	changedHistory.History = append([]ConversationRouteHistory(nil), value.History...)
	changedHistory.History[0].Content = "different prior"
	third, err := ConversationCoordinatorContextDigest(changedHistory)
	if err != nil || bytes.Equal(first, third) {
		t.Fatalf("history change did not change context digest: %x / %x, %v", first, third, err)
	}
	changedModel := value
	changedModel.ModelID = "p7654321"
	fourth, err := ConversationCoordinatorContextDigest(changedModel)
	if err != nil || bytes.Equal(first, fourth) {
		t.Fatalf("model change did not change context digest: %x / %x, %v", first, fourth, err)
	}
	changedSummary := value
	changedSummary.SummaryVersion = 1
	changedSummary.SummaryText = "authorized older context"
	summaryDigest := sha256.Sum256([]byte(changedSummary.SummaryText))
	changedSummary.SummarySHA256 = summaryDigest[:]
	changedSummary.SummarySourcesSHA256 = make([]byte, sha256.Size)
	fifth, err := ConversationCoordinatorContextDigest(changedSummary)
	if err != nil || bytes.Equal(first, fifth) {
		t.Fatalf("summary change did not change context digest: %x / %x, %v", first, fifth, err)
	}
}

func TestConversationCoordinatorCheckpointSaveAndReadPreservesFence(t *testing.T) {
	claim, identity := coordinatorCheckpointFixture()
	key := make([]byte, 32)
	for index := range key {
		key[index] = byte(index + 1)
	}
	sealer, err := NewSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	store := &coordinatorCheckpointStoreTest{head: ConversationCoordinatorCheckpointHead{Through: -1}}
	service, err := NewConversationCoordinatorCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"events":[{"type":"checkpoint"}]}`)
	digest := sha256.Sum256(payload)
	contextDigest := sha256.Sum256([]byte("authorized route context"))
	if _, err := service.Save(context.Background(), ConversationCoordinatorCheckpointCommand{Claim: claim, Identity: identity,
		ContextDigest: contextDigest[:], From: 0, Through: 2, Digest: digest[:], Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if store.page.Identity != identity || string(store.page.Ciphertext) == string(payload) {
		t.Fatal("checkpoint was not encrypted under its coordinator identity")
	}
	head, err := service.Head(context.Background(), claim, identity, contextDigest[:])
	if err != nil || head.Through != 2 || string(head.Digest) != string(digest[:]) || !bytes.Equal(head.ContextDigest, contextDigest[:]) {
		t.Fatalf("coordinator checkpoint head was not returned: %+v, %v", head, err)
	}
	page, err := service.ReadPage(context.Background(), ConversationCoordinatorCheckpointPageRequest{Claim: claim, Identity: identity,
		ContextDigest: contextDigest[:], From: 0, Through: 2, ThroughDigest: digest[:]})
	if err != nil || page.From != 0 || page.Through != 2 || string(page.Payload) != string(payload) {
		t.Fatalf("coordinator checkpoint page did not round trip: %+v, %v", page, err)
	}
}

func TestConversationCoordinatorCheckpointRejectsChangedContext(t *testing.T) {
	claim, identity := coordinatorCheckpointFixture()
	key := make([]byte, 32)
	sealer, err := NewSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	original := sha256.Sum256([]byte("original context"))
	changed := sha256.Sum256([]byte("changed context"))
	store := &coordinatorCheckpointStoreTest{head: ConversationCoordinatorCheckpointHead{Through: -1}}
	service, err := NewConversationCoordinatorCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"events":[{"type":"checkpoint"}]}`)
	digest := sha256.Sum256(payload)
	if _, err := service.Save(context.Background(), ConversationCoordinatorCheckpointCommand{Claim: claim, Identity: identity,
		ContextDigest: original[:], From: 0, Through: 0, Digest: digest[:], Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Head(context.Background(), claim, identity, changed[:]); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("changed route context head = %v", err)
	}
	if _, err := service.ReadPage(context.Background(), ConversationCoordinatorCheckpointPageRequest{Claim: claim, Identity: identity,
		ContextDigest: changed[:], From: 0, Through: 0, ThroughDigest: digest[:]}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("changed route context page = %v", err)
	}
	if _, err := service.Save(context.Background(), ConversationCoordinatorCheckpointCommand{Claim: claim, Identity: identity,
		ContextDigest: changed[:], From: 1, Through: 1, Digest: digest[:], Payload: payload}); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("changed route context overwrite = %v", err)
	}
}

func TestConversationCoordinatorCheckpointRejectsCrossConversationAndBadDigest(t *testing.T) {
	claim, identity := coordinatorCheckpointFixture()
	key := make([]byte, 32)
	sealer, err := NewSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewConversationCoordinatorCheckpointService(&coordinatorCheckpointStoreTest{}, sealer)
	if err != nil {
		t.Fatal(err)
	}
	identity.ConversationID = "c7654321"
	if _, err := service.Head(context.Background(), claim, identity, make([]byte, sha256.Size)); err == nil {
		t.Fatal("coordinator checkpoint crossed conversation identity")
	}
	_, identity = coordinatorCheckpointFixture()
	payload := []byte(`{"checkpoint":true}`)
	if _, err := service.Save(context.Background(), ConversationCoordinatorCheckpointCommand{Claim: claim, Identity: identity,
		ContextDigest: make([]byte, sha256.Size), From: 0, Through: 0, Digest: make([]byte, sha256.Size), Payload: payload}); err == nil {
		t.Fatal("coordinator checkpoint accepted a mismatched digest")
	}
}
