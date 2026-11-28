package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type taskSourceLocationStoreStub struct {
	TaskStore
	records map[string]TaskStepDetailRecord
	calls   []string
}

func (s *taskSourceLocationStoreStub) GetTaskStepDetail(_ context.Context, actorID, taskID, stepID string) (TaskStepDetailRecord, error) {
	s.calls = append(s.calls, stepID)
	record, ok := s.records[stepID]
	if !ok || record.OwnerID != actorID || record.TaskID != taskID || record.StepID != stepID {
		return TaskStepDetailRecord{}, taskSourceLocationNotFoundError()
	}
	return record, nil
}

func taskSourceLocationNotFoundError() error {
	return domain.NewError(domain.CodeNotFound, "task step detail not found")
}

func TestTaskServiceAttachesClaimSourceLocationsOnlyFromReferencedCurrentRevisionSteps(t *testing.T) {
	const ownerID, taskID = "u1234567", "q1234567"
	const revision int64 = 7
	sealer, err := NewSealer(bytes.Repeat([]byte{41}, 32))
	if err != nil {
		t.Fatal(err)
	}

	store := &taskSourceLocationStoreStub{records: make(map[string]TaskStepDetailRecord)}
	steps := make([]task.Step, 0, 5)
	locationIDs := make(map[string]string)
	addStep := func(stepID string, stepRevision int64, detailState string, withLocation bool) {
		stepRevisionValue := stepRevision
		callID := "call-" + stepID
		steps = append(steps, task.Step{ID: stepID, TaskID: taskID, Sequence: int64(len(steps) + 1), Kind: "tool",
			Status: task.StepSucceeded, ToolName: cloudDSHWebReadToolName, ToolCallKey: callID,
			TaskRevision: &stepRevisionValue, DetailState: detailState})
		if detailState != "available" {
			return
		}
		content := "source passage for " + stepID
		contentDigest := sha256.Sum256([]byte(content))
		contentEvidence := &TaskStepContentEvidence{SHA256: hex.EncodeToString(contentDigest[:]), Preview: content}
		document := TaskStepDetailDocument{Object: cloudDSHWebReadToolName, Input: "Read source", Result: "Read completed.",
			ContentEvidence: contentEvidence}
		if withLocation {
			location := TaskStepSourceLocation{Kind: taskStepSourceLocationKindPublicWebPage,
				URI: "https://example.test/" + stepID, State: taskStepSourceLocationStateRequestedURIOnly,
				ContentSHA256: contentEvidence.SHA256}
			location.ID = taskStepSourceLocationID(ownerID, taskID, stepID, stepRevision, callID, location)
			document.SourceLocations = []TaskStepSourceLocation{location}
			locationIDs[stepID] = location.ID
		}
		ciphertext, digest, size, err := sealTaskStepDetail(sealer, ownerID, taskID, stepID, stepRevision, document)
		if err != nil {
			t.Fatalf("seal step %s detail: %v", stepID, err)
		}
		store.records[stepID] = TaskStepDetailRecord{TaskID: taskID, OwnerID: ownerID, StepID: stepID,
			TaskRevision: stepRevision, EventSequence: int64(len(steps) + 10), Ciphertext: ciphertext,
			SHA256: digest, ContentEvidenceSHA256: contentDigest[:], SizeBytes: size}
	}

	addStep("s1000001", revision, "available", true)
	addStep("s1000002", revision, "available", true)
	addStep("s1000003", revision-1, "available", true)
	addStep("s1000004", revision, "restricted", true)
	addStep("s1000005", revision, "available", false)

	ref := func(id string, stepRevision int64, sequence int64, kind TaskValidationEvidenceKind) TaskValidationEvidenceRefRecord {
		return TaskValidationEvidenceRefRecord{Kind: kind, ID: id, TaskRevision: stepRevision, Sequence: sequence}
	}
	evidence := &TaskValidationEvidenceRecord{TaskRevision: revision, EventSequence: 80, Claims: []TaskValidationEvidenceClaimRecord{
		{ID: "claim-a", EvidenceRefs: []TaskValidationEvidenceRefRecord{ref("s1000001", revision, 1, TaskValidationEvidenceAction)}},
		{ID: "claim-b", EvidenceRefs: []TaskValidationEvidenceRefRecord{ref("s1000002", revision, 2, TaskValidationEvidenceResult)}},
		{ID: "claim-stale", EvidenceRefs: []TaskValidationEvidenceRefRecord{ref("s1000003", revision, 3, TaskValidationEvidenceAction)}},
		{ID: "claim-restricted", EvidenceRefs: []TaskValidationEvidenceRefRecord{ref("s1000004", revision, 4, TaskValidationEvidenceAction)}},
		{ID: "claim-no-locator", EvidenceRefs: []TaskValidationEvidenceRefRecord{ref("s1000005", revision, 5, TaskValidationEvidenceAction)}},
		{ID: "claim-partial", EvidenceRefs: []TaskValidationEvidenceRefRecord{
			ref("s1000001", revision, 1, TaskValidationEvidenceAction), ref("s9999999", revision, 9, TaskValidationEvidenceAction)}},
		{ID: "claim-performance", PerformanceComparison: &TaskValidationPerformanceComparison{
			BaselineRef: ref("s1000001", revision, 1, TaskValidationEvidenceBaseline),
			AfterRef:    ref("s1000002", revision, 2, TaskValidationEvidenceBeforeAfter),
		}},
	}}
	details := TaskDetails{Task: task.Task{ID: taskID, OwnerID: ownerID}, Steps: steps}
	service := &TaskService{store: store}
	if err := service.SetTaskStepDetailSealer(sealer); err != nil {
		t.Fatal(err)
	}

	got, err := service.attachTaskValidationSourceLocations(context.Background(), identity.Principal{UserID: ownerID}, details, evidence)
	if err != nil {
		t.Fatalf("attach source locations: %v", err)
	}
	if got == nil || len(got.Claims) != len(evidence.Claims) || len(got.SourceLocations) != 2 {
		t.Fatalf("attached result has unexpected cardinality: claims=%d locations=%d result=%+v", len(got.Claims), len(got.SourceLocations), got)
	}
	claims := make(map[string]TaskValidationEvidenceClaimRecord, len(got.Claims))
	for _, claim := range got.Claims {
		claims[claim.ID] = claim
	}
	for _, test := range []struct {
		claimID string
		state   string
		ids     []string
	}{
		{claimID: "claim-a", state: TaskValidationSourceLocationsMapped, ids: []string{locationIDs["s1000001"]}},
		{claimID: "claim-b", state: TaskValidationSourceLocationsMapped, ids: []string{locationIDs["s1000002"]}},
		{claimID: "claim-stale", state: TaskValidationSourceLocationsUnavailable, ids: []string{}},
		{claimID: "claim-restricted", state: TaskValidationSourceLocationsRestricted, ids: []string{}},
		{claimID: "claim-no-locator", state: TaskValidationSourceLocationsNone, ids: []string{}},
		{claimID: "claim-partial", state: TaskValidationSourceLocationsPartial, ids: []string{locationIDs["s1000001"]}},
		{claimID: "claim-performance", state: TaskValidationSourceLocationsMapped,
			ids: []string{locationIDs["s1000001"], locationIDs["s1000002"]}},
	} {
		claim, ok := claims[test.claimID]
		expectedIDs := append([]string{}, test.ids...)
		sort.Strings(expectedIDs)
		if !ok || claim.SourceLocationState != test.state || !reflect.DeepEqual(claim.SourceLocationIDs, expectedIDs) {
			t.Errorf("claim %s locators=%v state=%q; want %v state=%q", test.claimID,
				claim.SourceLocationIDs, claim.SourceLocationState, expectedIDs, test.state)
		}
	}
	if reflect.DeepEqual(claims["claim-a"].SourceLocationIDs, claims["claim-b"].SourceLocationIDs) ||
		claims["claim-a"].SourceLocationIDs[0] == locationIDs["s1000002"] ||
		claims["claim-b"].SourceLocationIDs[0] == locationIDs["s1000001"] {
		t.Fatalf("claim locators crossed between claims: A=%v B=%v", claims["claim-a"].SourceLocationIDs, claims["claim-b"].SourceLocationIDs)
	}
	if !reflect.DeepEqual(store.calls, []string{"s1000001", "s1000002", "s1000005"}) {
		t.Fatalf("step detail reads=%v; stale and restricted steps must not be read", store.calls)
	}
}

func TestTaskServiceMarksClaimLocatorsTruncatedAtBoundedSourceLimit(t *testing.T) {
	const ownerID, taskID = "u1234567", "q1234567"
	const revision int64 = 4
	sealer, err := NewSealer(bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := &taskSourceLocationStoreStub{records: make(map[string]TaskStepDetailRecord)}
	steps := make([]task.Step, 0, maxTaskValidationSourceLocations+1)
	refs := make([]TaskValidationEvidenceRefRecord, 0, maxTaskValidationSourceLocations+1)
	lastStepID := ""
	for index := range maxTaskValidationSourceLocations + 1 {
		stepID := fmt.Sprintf("s%07d", index+1)
		lastStepID = stepID
		callID := fmt.Sprintf("web-read-%d", index+1)
		stepRevision := revision
		steps = append(steps, task.Step{ID: stepID, TaskID: taskID, Sequence: int64(index + 1), Kind: "tool",
			Status: task.StepSucceeded, ToolName: cloudDSHWebReadToolName, ToolCallKey: callID,
			TaskRevision: &stepRevision, DetailState: "available"})
		refs = append(refs, TaskValidationEvidenceRefRecord{Kind: TaskValidationEvidenceAction, ID: stepID,
			TaskRevision: revision, Sequence: int64(index + 1)})
		content := fmt.Sprintf("bounded source %d", index+1)
		contentDigest := sha256.Sum256([]byte(content))
		contentEvidence := &TaskStepContentEvidence{SHA256: hex.EncodeToString(contentDigest[:]), Preview: content}
		location := TaskStepSourceLocation{Kind: taskStepSourceLocationKindPublicWebPage,
			URI: fmt.Sprintf("https://example.test/source/%d", index+1), State: taskStepSourceLocationStateRequestedURIOnly,
			ContentSHA256: contentEvidence.SHA256}
		location.ID = taskStepSourceLocationID(ownerID, taskID, stepID, revision, callID, location)
		document := TaskStepDetailDocument{Object: cloudDSHWebReadToolName, Input: "Read source", Result: "Read completed.",
			ContentEvidence: contentEvidence, SourceLocations: []TaskStepSourceLocation{location}}
		ciphertext, digest, size, err := sealTaskStepDetail(sealer, ownerID, taskID, stepID, revision, document)
		if err != nil {
			t.Fatalf("seal step %s detail: %v", stepID, err)
		}
		store.records[stepID] = TaskStepDetailRecord{TaskID: taskID, OwnerID: ownerID, StepID: stepID,
			TaskRevision: revision, EventSequence: int64(index + 20), Ciphertext: ciphertext,
			SHA256: digest, ContentEvidenceSHA256: contentDigest[:], SizeBytes: size}
	}
	evidence := &TaskValidationEvidenceRecord{TaskRevision: revision, EventSequence: 99, Claims: []TaskValidationEvidenceClaimRecord{
		{ID: "claim-wide", EvidenceRefs: refs},
		{ID: "claim-overflow-only", EvidenceRefs: []TaskValidationEvidenceRefRecord{refs[len(refs)-1]}},
	}}
	service := &TaskService{store: store}
	if err := service.SetTaskStepDetailSealer(sealer); err != nil {
		t.Fatal(err)
	}

	got, err := service.attachTaskValidationSourceLocations(context.Background(), identity.Principal{UserID: ownerID},
		TaskDetails{Task: task.Task{ID: taskID, OwnerID: ownerID}, Steps: steps}, evidence)
	if err != nil {
		t.Fatalf("attach bounded source locations: %v", err)
	}
	if len(got.SourceLocations) != maxTaskValidationSourceLocations {
		t.Fatalf("global source locations=%d; want bounded maximum %d", len(got.SourceLocations), maxTaskValidationSourceLocations)
	}
	claims := map[string]TaskValidationEvidenceClaimRecord{}
	for _, claim := range got.Claims {
		claims[claim.ID] = claim
	}
	if claim := claims["claim-wide"]; claim.SourceLocationState != TaskValidationSourceLocationsPartial ||
		len(claim.SourceLocationIDs) != maxTaskValidationSourceLocations {
		t.Fatalf("wide claim state=%q IDs=%d; want partial with %d locators", claim.SourceLocationState,
			len(claim.SourceLocationIDs), maxTaskValidationSourceLocations)
	}
	if claim := claims["claim-overflow-only"]; claim.SourceLocationState != TaskValidationSourceLocationsTruncated ||
		len(claim.SourceLocationIDs) != 0 {
		t.Fatalf("overflow-only claim state=%q IDs=%v; want truncated without an invented locator", claim.SourceLocationState, claim.SourceLocationIDs)
	}
	if len(store.calls) != maxTaskValidationSourceLocations+1 || store.calls[len(store.calls)-1] != lastStepID {
		t.Fatalf("detail reads=%d final=%q; want all referenced steps and overflow source %q", len(store.calls), store.calls[len(store.calls)-1], lastStepID)
	}
}
