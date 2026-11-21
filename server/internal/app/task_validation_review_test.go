package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"
)

func taskValidationReviewEvidenceFixture() *TaskValidationEvidenceRecord {
	return &TaskValidationEvidenceRecord{
		TaskRevision: 4, EventSequence: 9, ResultSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Claims: []TaskValidationEvidenceClaimRecord{{ID: "c1", EvidenceRefs: []TaskValidationEvidenceRefRecord{{
			Kind: TaskValidationEvidenceSource, ID: "m1234567", TaskRevision: 4, Sequence: 2,
		}}}},
	}
}

func taskValidationReviewReportFixture() TaskValidationReviewReport {
	findings := make([]TaskValidationReviewFinding, 0, len(taskHardCheckOrder))
	for _, id := range taskHardCheckOrder {
		findings = append(findings, TaskValidationReviewFinding{ID: id, Assessment: TaskValidationReviewSupports,
			Rationale:    "The cited evidence supports this check.",
			EvidenceRefs: []TaskValidationEvidenceRefRecord{{Kind: TaskValidationEvidenceSource, ID: "m1234567", TaskRevision: 4, Sequence: 2}}})
	}
	return TaskValidationReviewReport{Recommendation: TaskValidationPass, Summary: "All checks are supported by the cited evidence.", Findings: findings}
}

func TestTaskValidationReviewReportRequiresAllChecksAndAvailableEvidence(t *testing.T) {
	evidence := taskValidationReviewEvidenceFixture()
	report := taskValidationReviewReportFixture()
	if err := ValidateTaskValidationReviewReport(report, evidence); err != nil {
		t.Fatalf("valid review report rejected: %v", err)
	}

	changed := report
	changed.Findings = append([]TaskValidationReviewFinding(nil), report.Findings...)
	changed.Findings[0].EvidenceRefs = []TaskValidationEvidenceRefRecord{{Kind: TaskValidationEvidenceSource, ID: "m9999999", TaskRevision: 4, Sequence: 2}}
	if err := ValidateTaskValidationReviewReport(changed, evidence); err == nil {
		t.Fatal("review report accepted evidence outside the persisted result")
	}

	changed = report
	changed.Findings = append([]TaskValidationReviewFinding(nil), report.Findings...)
	changed.Findings[0].ID = TaskHardCheckEvidence
	if err := ValidateTaskValidationReviewReport(changed, evidence); err == nil {
		t.Fatal("review report accepted reordered or duplicate checks")
	}

	changed = report
	changed.Findings = append([]TaskValidationReviewFinding(nil), report.Findings...)
	changed.Findings[0].EvidenceRefs = nil
	if err := ValidateTaskValidationReviewReport(changed, evidence); err == nil {
		t.Fatal("review report accepted a supported finding without evidence")
	}
}

func TestTaskValidationReviewRecommendationMatchesFindings(t *testing.T) {
	evidence := taskValidationReviewEvidenceFixture()
	tests := []struct {
		name           string
		recommendation TaskValidationState
		assessment     TaskValidationReviewAssessment
		valid          bool
	}{
		{name: "pass with supporting findings", recommendation: TaskValidationPass, assessment: TaskValidationReviewSupports, valid: true},
		{name: "pass with insufficient finding", recommendation: TaskValidationPass, assessment: TaskValidationReviewInsufficient},
		{name: "pass with contradicting finding", recommendation: TaskValidationPass, assessment: TaskValidationReviewContradicts},
		{name: "pass notes with insufficient finding", recommendation: TaskValidationPassNotes, assessment: TaskValidationReviewInsufficient},
		{name: "fail with non supporting finding", recommendation: TaskValidationFail, assessment: TaskValidationReviewInsufficient, valid: true},
		{name: "fail with all supporting findings", recommendation: TaskValidationFail, assessment: TaskValidationReviewSupports},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := taskValidationReviewReportFixture()
			report.Recommendation = test.recommendation
			report.Findings = append([]TaskValidationReviewFinding(nil), report.Findings...)
			if test.assessment != TaskValidationReviewSupports {
				report.Findings[0].Assessment = test.assessment
			}
			err := ValidateTaskValidationReviewReport(report, evidence)
			if (err == nil) != test.valid {
				t.Fatalf("ValidateTaskValidationReviewReport() error=%v; valid=%t", err, test.valid)
			}
		})
	}
}

func TestParseTaskValidationReviewReportRejectsAmbiguousJSON(t *testing.T) {
	evidence := taskValidationReviewEvidenceFixture()
	valid, err := jsonMarshalReview(taskValidationReviewReportFixture())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTaskValidationReviewReport(valid, evidence); err != nil {
		t.Fatalf("valid report could not be parsed: %v", err)
	}
	for _, recommendation := range []TaskValidationState{TaskValidationPass, TaskValidationPassNotes, TaskValidationFail} {
		report := taskValidationReviewReportFixture()
		report.Recommendation = recommendation
		if recommendation == TaskValidationFail {
			report.Findings[0].Assessment = TaskValidationReviewInsufficient
		}
		raw, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseTaskValidationReviewReport(raw, evidence); err != nil {
			t.Errorf("recommendation %q could not be parsed: %v", recommendation, err)
		}
	}
	for _, raw := range [][]byte{
		append(append([]byte(nil), valid[:len(valid)-1]...), []byte(`,"unexpected":true}`)...),
		bytes.Replace(valid, []byte(`"recommendation":"PASS"`), []byte(`"recommendation":"PASS","recommendation":"FAIL"`), 1),
	} {
		if _, err := ParseTaskValidationReviewReport(raw, evidence); err == nil {
			t.Fatalf("ambiguous report JSON accepted: %s", raw)
		}
	}
}

func TestTaskValidationReviewCompletionRequiresCatalogVerifiedIndependentFamily(t *testing.T) {
	model := func(connectionID, endpoint, configured string) TaskValidationReviewModel {
		identity, ok := ResolveTaskValidationModelIdentity(endpoint, configured, configured)
		if !ok {
			t.Fatalf("model %q is missing from identity catalog", configured)
		}
		return TaskValidationReviewModel{CallID: "call_" + connectionID, ConnectionID: connectionID, ConnectionVersion: time.Unix(1, 0).UTC(), Endpoint: endpoint, ConfiguredModel: configured,
			ReportedModel: configured, Identity: identity, ResponseDigest: bytes.Repeat([]byte{0x56}, 32)}
	}
	completion := TaskValidationReviewCompletion{
		Claim: TaskValidationReviewClaim{OwnerID: "u1234567", InitiatorID: "u1234567", ConversationID: "c1234567", TaskID: "q1234567", TaskRevision: 4,
			ResultEventSequence: 9, ResultSHA256: bytes.Repeat([]byte{0x12}, 32), ClaimGeneration: 1, LeaseOwner: "review-worker"},
		SessionID:      "review_session_1234567890",
		ExecutionModel: model("p1234567", "https://api.openai.com/v1", "gpt-6-luna"),
		ReviewerModel:  model("p2234567", "https://api.deepseek.com", "deepseek-flash"),
		Report:         taskValidationReviewReportFixture(),
	}
	completion.ExecutionModel.SessionID = "exec_session_1234567890"
	completion.ReviewerModel.SessionID = completion.SessionID
	responseText, err := json.Marshal(completion.Report)
	if err != nil {
		t.Fatal(err)
	}
	completion.ResponseText = string(responseText)
	if err := ValidateTaskValidationReviewCompletion(completion, taskValidationReviewEvidenceFixture()); err != nil {
		t.Fatalf("valid independent review rejected: %v", err)
	}
	completion.ReviewerModel = model("p2234567", "https://api.openai.com/v1", "gpt-6.2-sol")
	if err := ValidateTaskValidationReviewCompletion(completion, taskValidationReviewEvidenceFixture()); err == nil {
		t.Fatal("same-family reviewer accepted as independent")
	}
	completion.ReviewerModel = model("p2234567", "https://api.deepseek.com", "deepseek-flash")
	completion.ReviewerModel.Identity.Family = "openai-gpt6"
	if err := ValidateTaskValidationReviewCompletion(completion, taskValidationReviewEvidenceFixture()); err == nil {
		t.Fatal("caller-supplied reviewer family bypassed the catalog")
	}
}

func TestTaskValidationReviewModelResponseIsEncryptedAndFenceBound(t *testing.T) {
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	fence := TaskValidationReviewModelFence{Claim: TaskValidationReviewClaim{OwnerID: "u1234567", InitiatorID: "u1234567", ConversationID: "c1234567", TaskID: "q1234567",
		TaskRevision: 4, ResultEventSequence: 9, ResultSHA256: bytes.Repeat([]byte{0x12}, 32), ClaimGeneration: 3,
		LeaseOwner: "review-worker"}, InitiatorID: "u1234567", SessionID: "review_session_1234567890", ThroughSequence: 0}
	callID, err := DSHTaskValidationReviewModelCallID(fence)
	if err != nil {
		t.Fatal(err)
	}
	request := []byte(`{"instructions":"Review only","input":[{"type":"message","role":"user","text":"Review this result"}],"tools":[],"max_output_tokens":512}`)
	requestDigest := sha256.Sum256(request)
	reservation := ModelCallReservation{Source: ModelCallReview, Review: &fence, OwnerID: fence.InitiatorID,
		ModelID: "p1234567", ProviderEndpoint: "https://api.deepseek.com", ProviderModel: "deepseek-flash", ConnectionVersion: time.Unix(100, 0).UTC(),
		CallID: callID, RequestDigest: requestDigest[:], ReservedTokens: 1024}
	want := ModelResponse{Text: `{"recommendation":"PASS"}`, StopReason: "stop",
		Usage: ModelUsage{InputTokens: 7, OutputTokens: 3, Known: true}, ProviderReportedModel: "deepseek-flash"}
	stored, err := SealTaskValidationReviewModelResponse(sealer, reservation, want)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored.Ciphertext, []byte(want.Text)) {
		t.Fatal("review model output was stored as plaintext")
	}
	got, err := OpenTaskValidationReviewModelResponse(sealer, stored, reservation)
	if err != nil || got.Text != want.Text || got.StopReason != want.StopReason || got.Usage != want.Usage ||
		got.ProviderReportedModel != want.ProviderReportedModel {
		t.Fatalf("OpenTaskValidationReviewModelResponse() = %+v, %v", got, err)
	}
	changedDigest := stored
	changedDigest.TextDigest = bytes.Repeat([]byte{0x41}, 32)
	if _, err := OpenTaskValidationReviewModelResponse(sealer, changedDigest, reservation); err == nil {
		t.Fatal("review model response opened with a mismatched exact-text digest")
	}
	changed := reservation
	changedFence := fence
	changedFence.Claim.ClaimGeneration++
	changed.Review = &changedFence
	changed.CallID, err = DSHTaskValidationReviewModelCallID(changedFence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTaskValidationReviewModelResponse(sealer, stored, changed); err == nil {
		t.Fatal("review model response opened under another claim generation")
	}
}

func jsonMarshalReview(report TaskValidationReviewReport) ([]byte, error) {
	return json.Marshal(report)
}
