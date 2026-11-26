package postgres

import (
	"context"
	"encoding/json"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/personal"
)

func TestPostgresPersonalRuleCandidateRequiresCurrentSourceAndExplicitActivation(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 8192)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	source := personal.Object{ObjectType: personal.ObjectTypeMemory, ObjectID: "m_abcdef01",
		Body: json.RawMessage(`{"content":"prefers concise responses"}`)}
	if _, err := store.ApplyPersonalMutation(ctx, principal, personalContentMutationForTest(t, owner.ID, 1, source, 0)); err != nil {
		t.Fatalf("create source memory: %v", err)
	}
	candidate := personal.Object{ObjectType: personal.ObjectTypeRuleCandidate, ObjectID: "r_00000001",
		Body: json.RawMessage(`{"scope":"personal","status":"candidate","source_memory_id":"m_abcdef01","source_memory_revision":1}`)}
	mutation := personalContentMutationForTest(t, owner.ID, 2, candidate, 0)
	var directActivation personal.RuleCandidateBody
	if err := json.Unmarshal(candidate.Body, &directActivation); err != nil {
		t.Fatal(err)
	}
	directActivation.Status = personal.RuleCandidateActive
	directBody, _ := json.Marshal(directActivation)
	if _, err := store.ApplyPersonalMutation(ctx, principal, personalContentMutationForTest(t, owner.ID, 3,
		personal.Object{ObjectType: personal.ObjectTypeRuleCandidate, ObjectID: candidate.ObjectID, Body: directBody}, 0)); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("direct activation on create error = %v, want forbidden", err)
	}

	staleCandidate := candidate
	staleCandidate.ObjectID = "r_00000002"
	var staleBody personal.RuleCandidateBody
	if err := json.Unmarshal(staleCandidate.Body, &staleBody); err != nil {
		t.Fatal(err)
	}
	staleBody.SourceMemoryRevision = 2
	staleCandidate.Body, _ = json.Marshal(staleBody)
	if _, err := store.ApplyPersonalMutation(ctx, principal, personalContentMutationForTest(t, owner.ID, 4, staleCandidate, 0)); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("candidate with stale source memory error = %v, want conflict", err)
	}

	created, err := store.ApplyPersonalMutation(ctx, principal, mutation)
	if err != nil || created.ResultRevision != 1 {
		t.Fatalf("create pending candidate = %+v, %v", created, err)
	}
	var approved personal.RuleCandidateBody
	if err := json.Unmarshal(candidate.Body, &approved); err != nil {
		t.Fatal(err)
	}
	approved.Status = personal.RuleCandidateActive
	directBody, _ = json.Marshal(approved)
	if _, err := store.ApplyPersonalMutation(ctx, principal, personalContentMutationForTest(t, owner.ID, 5,
		personal.Object{ObjectType: personal.ObjectTypeRuleCandidate, ObjectID: candidate.ObjectID, Body: directBody}, 1)); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("direct activation without source validation error = %v, want forbidden", err)
	}
	approved.Status = personal.RuleCandidateValidated
	validatedBody, _ := json.Marshal(approved)
	validated, err := store.ApplyPersonalMutation(ctx, principal, personalContentMutationForTest(t, owner.ID, 6,
		personal.Object{ObjectType: personal.ObjectTypeRuleCandidate, ObjectID: candidate.ObjectID, Body: validatedBody}, 1))
	if err != nil || validated.ResultRevision != 2 {
		t.Fatalf("validate unchanged rule candidate = %+v, %v", validated, err)
	}
	approved.Status = personal.RuleCandidateActive
	approvedBody, _ := json.Marshal(approved)
	activated, err := store.ApplyPersonalMutation(ctx, principal, personalContentMutationForTest(t, owner.ID, 7,
		personal.Object{ObjectType: personal.ObjectTypeRuleCandidate, ObjectID: candidate.ObjectID, Body: approvedBody}, 2))
	if err != nil || activated.ResultRevision != 3 {
		t.Fatalf("explicitly activate validated rule candidate = %+v, %v", activated, err)
	}
	if _, err := store.GetPersonalObject(ctx, principal, personal.ObjectTypeRuleCandidate, candidate.ObjectID); err != nil {
		t.Fatalf("read owner rule candidate: %v", err)
	}
	other := createTestUser(t, store, 3, 4000, 8192)
	otherPrincipal := createPersonalReadSessionPrincipal(t, store, other.ID)
	if _, err := store.GetPersonalObject(ctx, otherPrincipal, personal.ObjectTypeRuleCandidate, candidate.ObjectID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner rule candidate read error = %v, want not_found", err)
	}
	var disabled personal.RuleCandidateBody
	if err := json.Unmarshal(approvedBody, &disabled); err != nil {
		t.Fatal(err)
	}
	disabled.Status = personal.RuleCandidatePending
	disabledBody, _ := json.Marshal(disabled)
	if _, err := store.ApplyPersonalMutation(ctx, principal, personalContentMutationForTest(t, owner.ID, 8,
		personal.Object{ObjectType: personal.ObjectTypeRuleCandidate, ObjectID: candidate.ObjectID, Body: disabledBody}, 3)); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("active rule demotion error = %v, want conflict", err)
	}
}
