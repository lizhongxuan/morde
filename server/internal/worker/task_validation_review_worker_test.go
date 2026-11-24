package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

type taskValidationReviewQueueStub struct {
	claim      app.TaskValidationReviewClaim
	claimed    bool
	completion app.TaskValidationReviewCompletion
	retried    bool
}

func (s *taskValidationReviewQueueStub) ClaimTaskValidationReviews(_ context.Context, _ string, _ time.Duration, _ int32) ([]app.TaskValidationReviewClaim, error) {
	if s.claimed {
		return nil, nil
	}
	s.claimed = true
	return []app.TaskValidationReviewClaim{s.claim}, nil
}

func (s *taskValidationReviewQueueStub) RetryTaskValidationReview(context.Context, app.TaskValidationReviewClaim, time.Duration) (bool, error) {
	s.retried = true
	return true, nil
}

func (s *taskValidationReviewQueueStub) CompleteTaskValidationReview(_ context.Context, value app.TaskValidationReviewCompletion) (bool, error) {
	s.completion = value
	return true, nil
}

type taskValidationReviewSnapshotStub struct {
	details          app.TaskDetails
	stepDetails      map[string]app.TaskStepDetail
	artifactContents map[string]app.TaskArtifactContent
}

type reviewArtifactBody struct{ *bytes.Reader }

func (reviewArtifactBody) Close() error { return nil }

func (s taskValidationReviewSnapshotStub) GetTaskValidationReviewSnapshot(context.Context, app.TaskValidationReviewClaim) (app.TaskDetails, error) {
	return s.details, nil
}

func (s taskValidationReviewSnapshotStub) GetTaskValidationReviewStepDetail(_ context.Context, _ app.TaskValidationReviewClaim, stepID string) (app.TaskStepDetail, error) {
	detail, ok := s.stepDetails[stepID]
	if !ok {
		return app.TaskStepDetail{}, domain.NewError(domain.CodeNotFound, "task step detail not found")
	}
	return detail, nil
}

func (s taskValidationReviewSnapshotStub) OpenTaskValidationReviewArtifactContent(_ context.Context, _ app.TaskValidationReviewClaim,
	artifactID string) (app.TaskArtifactContent, error) {
	content, ok := s.artifactContents[artifactID]
	if !ok {
		return app.TaskArtifactContent{}, domain.NewError(domain.CodeNotFound, "Task validation artifact content not found")
	}
	return content, nil
}

type taskValidationReviewModelsStub struct {
	execution app.TaskValidationReviewModel
	reviewer  app.TaskValidationReviewModel
	text      string
}

func (s taskValidationReviewModelsStub) ResolveTaskValidationReviewExecutionModel(context.Context, app.TaskValidationReviewClaim) (app.TaskValidationReviewModel, error) {
	return s.execution, nil
}

func (s taskValidationReviewModelsStub) ResolveTaskValidationReviewModel(context.Context, string, app.TaskValidationReviewModel) (string, error) {
	return s.reviewer.ConnectionID, nil
}

func (s taskValidationReviewModelsStub) CompleteTaskValidationReviewDSHModel(_ context.Context, fence app.TaskValidationReviewModelFence,
	modelID string, _ json.RawMessage) (app.ModelResponse, error) {
	callID, err := app.DSHTaskValidationReviewModelCallID(fence)
	if err != nil {
		return app.ModelResponse{}, err
	}
	reviewer := s.reviewer
	reviewer.CallID = callID
	reviewer.SessionID = fence.SessionID
	reviewer.ThroughSequence = fence.ThroughSequence
	if modelID != reviewer.ConnectionID {
		return app.ModelResponse{}, io.ErrUnexpectedEOF
	}
	return app.ModelResponse{Text: s.text, Usage: app.ModelUsage{Known: true, InputTokens: 40, OutputTokens: 20},
		StopReason: "stop", TaskValidationReview: &reviewer}, nil
}

type taskValidationReviewFrameWriter struct {
	review *cloudDSHTaskValidationReview
	frames []map[string]json.RawMessage
}

func (w *taskValidationReviewFrameWriter) Write(data []byte) (int, error) {
	var frame map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(data), &frame); err != nil {
		return 0, err
	}
	w.frames = append(w.frames, frame)
	var kind string
	_ = json.Unmarshal(frame["type"], &kind)
	if kind == "model_end" {
		w.review.markTerminal()
	}
	return len(data), nil
}

func (*taskValidationReviewFrameWriter) Close() error { return nil }

type taskValidationReviewRuntimeStub struct {
	claim  app.TaskValidationReviewClaim
	result string
}

func (s taskValidationReviewRuntimeStub) StartTaskValidationReview(_ context.Context, fence app.TaskValidationReviewModelFence,
	start map[string]any) (*cloudDSHTaskValidationReview, error) {
	if !validCloudDSHTaskValidationReviewStart(mustJSON(start), fence) {
		return nil, io.ErrUnexpectedEOF
	}
	identity := cloudDSHTaskValidationReviewIdentityForFence(fence)
	runtimeSession := &cloudDSHAccountRuntimeSession{
		runtime: app.AccountAgentRuntime{Fence: app.AccountAgentRuntimeFence{ServiceInstanceID: "12345678-1234-1234-1234-123456789012",
			ConversationID: fence.Claim.ConversationID, OwnerID: fence.Claim.OwnerID, RuntimeID: "runtime-1", RuntimeGeneration: 1}},
		reviews: make(map[string]*cloudDSHTaskValidationReview),
	}
	review := &cloudDSHTaskValidationReview{ctx: context.Background(), session: runtimeSession, fence: fence,
		identity: identity, sequence: 1, events: make(chan cloudDSHRead, 3), finished: make(chan struct{}), started: true}
	writer := &taskValidationReviewFrameWriter{review: review}
	runtimeSession.input = &cloudDSHRuntimeInput{target: writer}
	runtimeSession.reviews[fence.SessionID] = review
	checkpointPayload := `{"phase":"review"}`
	checkpointDigest := sha256.Sum256([]byte(checkpointPayload))
	modelFence := fence
	modelFence.ThroughSequence = 0
	callID, err := app.DSHTaskValidationReviewModelCallID(modelFence)
	if err != nil {
		return nil, err
	}
	modelID, _ := start["model"].(string)
	request, err := json.Marshal(map[string]any{
		"provider": "morde", "model": modelID,
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "review"}}}},
		"tools":    []any{}, "max_tokens": 128,
	})
	if err != nil {
		return nil, err
	}
	requestDigest := sha256.Sum256(request)
	for _, frame := range []map[string]any{
		taskValidationReviewProtocolFrame("checkpoint", fence, map[string]any{"id": "checkpoint-1", "from_seq": int64(0),
			"through_seq": int64(0), "payload": checkpointPayload, "sha256": hex.EncodeToString(checkpointDigest[:])}),
		taskValidationReviewProtocolFrame("model", fence, map[string]any{"id": callID, "through_seq": int64(0),
			"request": json.RawMessage(request), "sha256": hex.EncodeToString(requestDigest[:])}),
		taskValidationReviewProtocolFrame("task_validation_review_settled", fence, map[string]any{"through_seq": int64(0),
			"result": s.result, "state": "completed"}),
	} {
		raw, err := json.Marshal(frame)
		if err != nil {
			return nil, err
		}
		decoded, err := decodeCloudDSHTaskValidationReviewFrame(raw)
		if err != nil || !cloudDSHTaskValidationReviewIdentityMatches(cloudDSHTaskValidationReviewIdentityFromFrame(decoded), fence) {
			return nil, fmt.Errorf("test runtime generated an invalid review frame: %s (decode=%v)", raw, err)
		}
		review.events <- cloudDSHRead{frame: raw}
	}
	return review, nil
}

func mustJSON(value any) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}

func TestTaskValidationReviewWorkerCompletesToolFreeIndependentReview(t *testing.T) {
	resultDigest := sha256.Sum256([]byte("verified result"))
	ref := app.TaskValidationEvidenceRefRecord{Kind: app.TaskValidationEvidenceSource, ID: "m1234567", TaskRevision: 4, Sequence: 2}
	evidence := &app.TaskValidationEvidenceRecord{TaskRevision: 4, EventSequence: 9,
		ResultSHA256: hex.EncodeToString(resultDigest[:]),
		Claims:       []app.TaskValidationEvidenceClaimRecord{{ID: "c1", StartByte: 0, EndByte: int32(len("verified result")), EvidenceRefs: []app.TaskValidationEvidenceRefRecord{ref}}}}
	findings := make([]app.TaskValidationReviewFinding, 0, 7)
	for _, id := range []app.TaskHardCheckID{app.TaskHardCheckConclusionFirst, app.TaskHardCheckEvidence, app.TaskHardCheckIndependent,
		app.TaskHardCheckClarification, app.TaskHardCheckApproval, app.TaskHardCheckCorrection, app.TaskHardCheckReplay} {
		findings = append(findings, app.TaskValidationReviewFinding{ID: id, Assessment: app.TaskValidationReviewSupports,
			Rationale: "The visible evidence supports this check.", EvidenceRefs: []app.TaskValidationEvidenceRefRecord{ref}})
	}
	report := app.TaskValidationReviewReport{Recommendation: app.TaskValidationPass, Summary: "The supplied evidence supports the result.", Findings: findings}
	result, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	claim := app.TaskValidationReviewClaim{OwnerID: "u1234567", InitiatorID: "u1234567", ConversationID: "c1234567",
		TaskID: "q1234567", TaskRevision: 4, ResultEventSequence: 9, ResultSHA256: resultDigest[:], ClaimGeneration: 2,
		LeaseOwner: "review-worker", LeaseExpiresAt: time.Now().Add(time.Minute)}
	openAI, ok := app.ResolveTaskValidationModelIdentity("https://api.openai.com/v1", "gpt-6-luna", "gpt-6-luna")
	if !ok {
		t.Fatal("test execution model is missing from the identity catalog")
	}
	deepSeek, ok := app.ResolveTaskValidationModelIdentity("https://api.deepseek.com", "deepseek-flash", "deepseek-flash")
	if !ok {
		t.Fatal("test reviewer model is missing from the identity catalog")
	}
	execution := app.TaskValidationReviewModel{CallID: "mdl_execution", ConnectionID: "p1234567",
		ConnectionVersion: time.Unix(100, 0).UTC(), Endpoint: "https://api.openai.com/v1", ConfiguredModel: "gpt-6-luna",
		ReportedModel: "gpt-6-luna", Identity: openAI, ResponseDigest: bytes.Repeat([]byte{0x56}, 32),
		SessionID: "task-root-session", ThroughSequence: 7}
	reviewer := app.TaskValidationReviewModel{ConnectionID: "p2234567", ConnectionVersion: time.Unix(200, 0).UTC(),
		Endpoint: "https://api.deepseek.com", ConfiguredModel: "deepseek-flash", ReportedModel: "deepseek-flash",
		Identity: deepSeek, ResponseDigest: bytes.Repeat([]byte{0x78}, 32)}
	queue := &taskValidationReviewQueueStub{claim: claim}
	revision, desiredRevision := int64(4), int64(4)
	worker, err := NewTaskValidationReviewWorker(TaskValidationReviewWorkerOptions{Queue: queue,
		Snapshots: taskValidationReviewSnapshotStub{details: app.TaskDetails{Task: task.Task{ID: claim.TaskID, OwnerID: claim.OwnerID,
			InitiatorID: claim.InitiatorID, ConversationID: claim.ConversationID, Status: task.StatusCompleted,
			CurrentRevision: &revision, DesiredRevision: &desiredRevision, Objective: "Verify the implementation", Result: "verified result"},
			ValidationEvidence: evidence}},
		AccountRuntime: taskValidationReviewRuntimeStub{claim: claim, result: string(result)},
		Models:         taskValidationReviewModelsStub{execution: execution, reviewer: reviewer, text: string(result)},
		WorkerID:       "review-worker", LeaseDuration: time.Minute, PollInterval: time.Millisecond, MaxConcurrent: 1, Logger: zap.NewNop()})
	if err != nil {
		t.Fatal(err)
	}
	found, err := worker.RunOnce(context.Background())
	if err != nil || !found {
		t.Fatalf("RunOnce() = %v, %v", found, err)
	}
	if queue.retried || queue.completion.SessionID == "" || queue.completion.ReviewerModel.Identity.Family == queue.completion.ExecutionModel.Identity.Family {
		t.Fatalf("worker did not persist an independent review: %#v", queue)
	}
	if queue.completion.Report.Recommendation != app.TaskValidationPass || queue.completion.ReviewerModel.ThroughSequence != 0 {
		t.Fatalf("unexpected completion: %#v", queue.completion)
	}
}

func TestTaskValidationReviewInputIncludesBoundSummariesAndVerifiedArtifactContent(t *testing.T) {
	t.Run("morde_web_search", func(t *testing.T) {
		toolName := "morde_web_search"
		result := "verified result"
		digest := sha256.Sum256([]byte(result))
		revision, desiredRevision := int64(4), int64(4)
		stepRef := app.TaskValidationEvidenceRefRecord{Kind: app.TaskValidationEvidenceAction, ID: "s1234567", TaskRevision: revision, Sequence: 2}
		artifactRef := app.TaskValidationEvidenceRefRecord{Kind: app.TaskValidationEvidenceSideEffect, ID: "a1234567", TaskRevision: revision, Sequence: 2, CallID: "write_report_1"}
		evidence := &app.TaskValidationEvidenceRecord{TaskRevision: revision, EventSequence: 9, ResultSHA256: hex.EncodeToString(digest[:]),
			Claims: []app.TaskValidationEvidenceClaimRecord{{ID: "c1", StartByte: 0, EndByte: int32(len(result)),
				EvidenceRefs: []app.TaskValidationEvidenceRefRecord{stepRef, artifactRef}}}}
		object, inputText, resultText := "search", "validated query summary", "three sources found"
		artifactContent := "Private report content must be checked against the requested result."
		artifactDigest := sha256.Sum256([]byte(artifactContent))
		sourceText := "Example authorized source passage supporting the cited result."
		sourceDigest := sha256.Sum256([]byte(sourceText))
		contentEvidence := &app.TaskStepContentEvidence{SHA256: hex.EncodeToString(sourceDigest[:]), Preview: sourceText}
		claim := app.TaskValidationReviewClaim{OwnerID: "u1234567", InitiatorID: "u1234567", ConversationID: "c1234567",
			TaskID: "q1234567", TaskRevision: revision, ResultEventSequence: 9, ResultSHA256: digest[:], ClaimGeneration: 1, LeaseOwner: "worker"}
		sourceStep := task.Step{ID: "s7654321", TaskID: claim.TaskID, Sequence: 1, Kind: "tool", Status: task.StepSucceeded,
			AgentSessionID: "session_123", TaskRevision: &revision, ToolCallKey: artifactRef.CallID, ToolName: "file_write"}
		snapshots := taskValidationReviewSnapshotStub{details: app.TaskDetails{
			Task: task.Task{ID: claim.TaskID, OwnerID: claim.OwnerID, InitiatorID: claim.InitiatorID, ConversationID: claim.ConversationID,
				Status: task.StatusCompleted, CurrentRevision: &revision, DesiredRevision: &desiredRevision, WorkspaceID: "w1234567",
				Objective: "Verify search", Result: result},
			Steps: []task.Step{sourceStep, {ID: stepRef.ID, TaskID: claim.TaskID, Sequence: stepRef.Sequence, Kind: "tool", Status: task.StepSucceeded,
				Title: "Read source", Summary: "Read completed successfully.", TaskRevision: &revision, ToolName: toolName, DetailState: "available"}},
			Artifacts: []task.Artifact{{ID: artifactRef.ID, TaskID: claim.TaskID, StepID: stepRef.ID, WorkspaceID: "w1234567", Revision: 11,
				Label: "Report", Path: "reports/result.txt", MediaType: "text/plain", SizeBytes: int64(len(artifactContent)), SHA256: hex.EncodeToString(artifactDigest[:])}},
			ValidationEvidence: evidence,
		}, stepDetails: map[string]app.TaskStepDetail{stepRef.ID: {TaskID: claim.TaskID, StepID: stepRef.ID, DetailState: "available",
			Object: &object, Input: &inputText, Result: &resultText, ContentEvidence: contentEvidence}},
			artifactContents: map[string]app.TaskArtifactContent{artifactRef.ID: {Filename: "result.txt", MediaType: "text/plain",
				SizeBytes: int64(len(artifactContent)), SHA256: hex.EncodeToString(artifactDigest[:]), Body: reviewArtifactBody{Reader: bytes.NewReader([]byte(artifactContent))}}}}
		withoutCurrentPermission := TaskValidationReviewWorker{options: TaskValidationReviewWorkerOptions{Snapshots: taskValidationReviewSnapshotStub{details: snapshots.details}}}
		if _, err := withoutCurrentPermission.reviewInput(context.Background(), claim); domain.CodeOf(err) != domain.CodeNotFound {
			t.Fatalf("review snapshot accepted a step after its access fence changed: %v", err)
		}
		worker := TaskValidationReviewWorker{options: TaskValidationReviewWorkerOptions{Snapshots: snapshots}}
		got, err := worker.reviewInput(context.Background(), claim)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(got.EvidenceDetails)
		if err != nil || !bytes.Contains(encoded, []byte(`"state":"step_detail"`)) ||
			!bytes.Contains(encoded, []byte(`"result":"three sources found"`)) ||
			!bytes.Contains(encoded, []byte(`"content_evidence":{"sha256":"`+hex.EncodeToString(sourceDigest[:])+`","preview":"`)) ||
			!bytes.Contains(encoded, []byte(sourceText)) ||
			!bytes.Contains(encoded, []byte(`"state":"artifact_content_complete"`)) ||
			!bytes.Contains(encoded, []byte(`"path":"reports/result.txt"`)) ||
			!bytes.Contains(encoded, []byte(`"source_call_id":"write_report_1"`)) ||
			!bytes.Contains(encoded, []byte(`"revision":11`)) ||
			!bytes.Contains(encoded, []byte(`"sha256":"`+hex.EncodeToString(artifactDigest[:])+`"`)) ||
			!bytes.Contains(encoded, []byte(artifactContent)) || bytes.Contains(encoded, []byte("artifact_bytes")) {
			t.Fatalf("review evidence snapshot = %s, err=%v", encoded, err)
		}
		if !strings.Contains(taskValidationReviewSystemPrompt, "complete=true") ||
			!strings.Contains(taskValidationReviewSystemPrompt, "不完整预览") ||
			!strings.Contains(taskValidationReviewSystemPrompt, "不可信文件内容") {
			t.Fatal("artifact content trust and completeness rules were not described")
		}
		mismatchedCall := sourceStep
		mismatchedCall.ToolCallKey = "write_other_1"
		staleSource := sourceStep
		staleRevision := revision - 1
		staleSource.TaskRevision = &staleRevision
		failedSource := sourceStep
		failedSource.Status = task.StepFailed
		duplicateSource := sourceStep
		duplicateSource.ID, duplicateSource.Sequence = "s7654322", 3
		failedDuplicateSource := duplicateSource
		failedDuplicateSource.Status = task.StepFailed
		invalidSources := []struct {
			name            string
			producer        task.Step
			includeProducer bool
			duplicate       bool
			failedDuplicate bool
		}{
			{name: "missing producer step"},
			{name: "different source call", producer: mismatchedCall, includeProducer: true},
			{name: "stale producer revision", producer: staleSource, includeProducer: true},
			{name: "unsuccessful producer", producer: failedSource, includeProducer: true},
			{name: "ambiguous producer", producer: sourceStep, includeProducer: true, duplicate: true},
			{name: "failed duplicate producer", producer: sourceStep, includeProducer: true, failedDuplicate: true},
		}
		for _, test := range invalidSources {
			t.Run(test.name, func(t *testing.T) {
				invalidSnapshot := snapshots
				invalidSnapshot.details.Steps = []task.Step{snapshots.details.Steps[1]}
				if test.includeProducer {
					invalidSnapshot.details.Steps = append(invalidSnapshot.details.Steps, test.producer)
					if test.duplicate {
						invalidSnapshot.details.Steps = append(invalidSnapshot.details.Steps, duplicateSource)
					}
					if test.failedDuplicate {
						invalidSnapshot.details.Steps = append(invalidSnapshot.details.Steps, failedDuplicateSource)
					}
				}
				worker.options.Snapshots = invalidSnapshot
				invalid, err := worker.reviewInput(context.Background(), claim)
				if err != nil || len(invalid.EvidenceDetails) != 2 || invalid.EvidenceDetails[1].State != "unavailable" ||
					invalid.EvidenceDetails[1].Artifact != nil {
					t.Fatalf("unbound artifact evidence state=%+v err=%v; want unavailable", invalid.EvidenceDetails, err)
				}
			})
		}
		missingHash := snapshots.details.Artifacts[0]
		missingHash.SHA256 = ""
		staleWorkspace := snapshots.details.Artifacts[0]
		staleWorkspace.WorkspaceID = "w7654321"
		invalidRevision := snapshots.details.Artifacts[0]
		invalidRevision.Revision = -1
		oversizedArtifact := snapshots.details.Artifacts[0]
		oversizedArtifact.SizeBytes = workspace.MaxSyncFileBytes + 1
		invalidArtifacts := []struct {
			name     string
			artifact task.Artifact
		}{{name: "missing hash", artifact: missingHash}, {name: "stale workspace", artifact: staleWorkspace},
			{name: "invalid revision", artifact: invalidRevision}, {name: "oversized metadata", artifact: oversizedArtifact}}
		for _, test := range invalidArtifacts {
			t.Run(test.name, func(t *testing.T) {
				invalidSnapshot := snapshots
				invalidSnapshot.details.Artifacts = []task.Artifact{test.artifact}
				worker.options.Snapshots = invalidSnapshot
				invalid, err := worker.reviewInput(context.Background(), claim)
				if err != nil || len(invalid.EvidenceDetails) != 2 || invalid.EvidenceDetails[1].State != "unavailable" ||
					invalid.EvidenceDetails[1].Artifact != nil {
					t.Fatalf("invalid artifact evidence state=%+v err=%v; want unavailable", invalid.EvidenceDetails, err)
				}
			})
		}
		t.Run("artifact content read size is bounded", func(t *testing.T) {
			boundedSnapshot := snapshots
			boundedSnapshot.details.Artifacts = append([]task.Artifact(nil), snapshots.details.Artifacts...)
			boundedSnapshot.details.Artifacts[0].SizeBytes = app.MaxTaskValidationReviewArtifactReadBytes + 1
			boundedSnapshot.artifactContents = nil
			worker.options.Snapshots = boundedSnapshot
			bounded, err := worker.reviewInput(context.Background(), claim)
			if err != nil || len(bounded.EvidenceDetails) != 2 ||
				bounded.EvidenceDetails[1].State != "artifact_content_not_read_size_limit" ||
				bounded.EvidenceDetails[1].Artifact == nil || bounded.EvidenceDetails[1].Artifact.ContentEvidence != nil ||
				len(bounded.ArtifactContentProofs) != 0 {
				t.Fatalf("oversized artifact was not kept as insufficient metadata evidence: input=%+v err=%v", bounded, err)
			}
		})
		t.Run("text beyond model preview stays incomplete", func(t *testing.T) {
			largeContent := strings.Repeat("x", app.MaxTaskValidationReviewArtifactPreviewBytes+1)
			largeDigest := sha256.Sum256([]byte(largeContent))
			largeSnapshot := snapshots
			largeSnapshot.details.Artifacts = append([]task.Artifact(nil), snapshots.details.Artifacts...)
			largeSnapshot.details.Artifacts[0].SizeBytes = int64(len(largeContent))
			largeSnapshot.details.Artifacts[0].SHA256 = hex.EncodeToString(largeDigest[:])
			largeSnapshot.artifactContents = map[string]app.TaskArtifactContent{artifactRef.ID: {
				Filename: "result.txt", MediaType: "text/plain", SizeBytes: int64(len(largeContent)),
				SHA256: hex.EncodeToString(largeDigest[:]), Body: reviewArtifactBody{Reader: bytes.NewReader([]byte(largeContent))},
			}}
			worker.options.Snapshots = largeSnapshot
			large, err := worker.reviewInput(context.Background(), claim)
			if err != nil || len(large.EvidenceDetails) != 2 || large.EvidenceDetails[1].State != "artifact_content_partial" ||
				large.EvidenceDetails[1].Artifact == nil || large.EvidenceDetails[1].Artifact.ContentEvidence == nil ||
				len(large.EvidenceDetails[1].Artifact.ContentEvidence.Preview) != app.MaxTaskValidationReviewArtifactPreviewBytes ||
				!large.EvidenceDetails[1].Artifact.ContentEvidence.Truncated || large.EvidenceDetails[1].Artifact.ContentEvidence.Complete ||
				len(large.ArtifactContentProofs) != 1 || large.ArtifactContentProofs[0].ContentComplete {
				t.Fatalf("text beyond preview limit was not kept explicitly incomplete: input=%+v err=%v", large, err)
			}
		})
		missingSnapshot := snapshots
		missingSnapshot.details.Artifacts = nil
		worker.options.Snapshots = missingSnapshot
		missing, err := worker.reviewInput(context.Background(), claim)
		if err != nil || len(missing.EvidenceDetails) != 2 || missing.EvidenceDetails[1].State != "unavailable" ||
			missing.EvidenceDetails[1].Artifact != nil {
			t.Fatalf("missing artifact evidence state=%+v err=%v; want unavailable", missing.EvidenceDetails, err)
		}
		missingCallID := snapshots
		missingCallEvidence := *snapshots.details.ValidationEvidence
		missingCallEvidence.Claims = append([]app.TaskValidationEvidenceClaimRecord(nil), snapshots.details.ValidationEvidence.Claims...)
		missingCallEvidence.Claims[0].EvidenceRefs = append([]app.TaskValidationEvidenceRefRecord(nil),
			snapshots.details.ValidationEvidence.Claims[0].EvidenceRefs...)
		missingCallEvidence.Claims[0].EvidenceRefs[1].CallID = ""
		missingCallID.details.ValidationEvidence = &missingCallEvidence
		worker.options.Snapshots = missingCallID
		missingCall, err := worker.reviewInput(context.Background(), claim)
		if err != nil || len(missingCall.EvidenceDetails) != 2 || missingCall.EvidenceDetails[1].State != "unavailable" ||
			missingCall.EvidenceDetails[1].Artifact != nil {
			t.Fatalf("artifact without operation CallID state=%+v err=%v; want unavailable", missingCall.EvidenceDetails, err)
		}
		worker.options.Snapshots = snapshots
		snapshots.details.Steps[1].ToolName = "morde_workspace_read"
		worker.options.Snapshots = snapshots
		if _, err := worker.reviewInput(context.Background(), claim); err == nil {
			t.Fatal("Workspace file content entered the external review package")
		}
	})
}

func TestValidReviewContentEvidenceChecksCompletePreviewDigest(t *testing.T) {
	content := "Complete verified source passage."
	digest := sha256.Sum256([]byte(content))
	if !validReviewContentEvidence(&app.TaskStepContentEvidence{SHA256: hex.EncodeToString(digest[:]), Preview: content}) {
		t.Fatal("matching complete preview was rejected")
	}
	if validReviewContentEvidence(&app.TaskStepContentEvidence{SHA256: hex.EncodeToString(digest[:]), Preview: "Altered source passage."}) {
		t.Fatal("complete preview with a mismatched digest was accepted")
	}
	prefix := content[:len("Complete verified")]
	if !validReviewContentEvidence(&app.TaskStepContentEvidence{SHA256: hex.EncodeToString(digest[:]), Preview: prefix, Truncated: true}) {
		t.Fatal("truncated preview was rejected despite its full-content digest")
	}
	if validReviewContentEvidence(&app.TaskStepContentEvidence{SHA256: strings.ToUpper(hex.EncodeToString(digest[:])), Preview: content}) {
		t.Fatal("non-canonical uppercase digest was accepted")
	}
}

func TestTaskValidationReviewInputIncludesOnlyStructuredShellExecutionEvidence(t *testing.T) {
	result := "The test command failed."
	digest := sha256.Sum256([]byte(result))
	revision, desiredRevision := int64(4), int64(4)
	stepRef := app.TaskValidationEvidenceRefRecord{Kind: app.TaskValidationEvidenceAction, ID: "s1234567", TaskRevision: revision, Sequence: 2}
	evidence := &app.TaskValidationEvidenceRecord{TaskRevision: revision, EventSequence: 9, ResultSHA256: hex.EncodeToString(digest[:]),
		Claims: []app.TaskValidationEvidenceClaimRecord{{ID: "c1", StartByte: 0, EndByte: int32(len(result)),
			EvidenceRefs: []app.TaskValidationEvidenceRefRecord{stepRef}}}}
	claim := app.TaskValidationReviewClaim{OwnerID: "u1234567", InitiatorID: "u1234567", ConversationID: "c1234567",
		TaskID: "q1234567", TaskRevision: revision, ResultEventSequence: 9, ResultSHA256: digest[:], ClaimGeneration: 1, LeaseOwner: "worker"}
	object, inputText, resultText := "shell_exec", "Tool request; raw parameters and output are omitted for safety.", "The Host runtime reported a definitive tool failure."
	shellEvidence := &app.TaskStepShellExecutionEvidence{ExitCode: 1, StderrBytes: 23, StderrTruncated: true}
	snapshots := taskValidationReviewSnapshotStub{details: app.TaskDetails{
		Task: task.Task{ID: claim.TaskID, OwnerID: claim.OwnerID, InitiatorID: claim.InitiatorID, ConversationID: claim.ConversationID,
			Status: task.StatusCompleted, CurrentRevision: &revision, DesiredRevision: &desiredRevision, Objective: "Verify the change", Result: result},
		Steps: []task.Step{{ID: stepRef.ID, TaskID: claim.TaskID, Sequence: stepRef.Sequence, Kind: "tool", Status: task.StepFailed,
			Title: "Run tests", Summary: resultText, TaskRevision: &revision, ToolName: "shell_exec", DetailState: "available"}},
		ValidationEvidence: evidence,
	}, stepDetails: map[string]app.TaskStepDetail{stepRef.ID: {TaskID: claim.TaskID, StepID: stepRef.ID, DetailState: "available",
		Object: &object, Input: &inputText, Result: &resultText, ShellExecution: shellEvidence}}}
	worker := TaskValidationReviewWorker{options: TaskValidationReviewWorkerOptions{Snapshots: snapshots}}
	got, err := worker.reviewInput(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(got.EvidenceDetails)
	if err != nil || !bytes.Contains(encoded, []byte(`"shell_execution":{"exit_code":1`)) ||
		!bytes.Contains(encoded, []byte(`"stderr_bytes":23`)) || bytes.Contains(encoded, []byte("private-stderr-value")) ||
		bytes.Contains(encoded, []byte("command")) {
		t.Fatalf("review packet shell evidence = %s, err=%v", encoded, err)
	}
}
