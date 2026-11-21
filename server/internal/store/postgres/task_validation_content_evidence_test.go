package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"

	"github.com/jackc/pgx/v5"
)

func TestVerifiedTaskWebContentCoverage(t *testing.T) {
	result := "First claim. Second claim."
	first := app.TaskValidationEvidenceRefRecord{Kind: app.TaskValidationEvidenceAction, ID: "s1234567", TaskRevision: 4, Sequence: 2, CallID: "web-call-1"}
	second := app.TaskValidationEvidenceRefRecord{Kind: app.TaskValidationEvidenceAction, ID: "s2234567", TaskRevision: 4, Sequence: 3, CallID: "web-call-2"}
	claims := []app.TaskValidationEvidenceClaimRecord{
		{ID: "c1", StartByte: 0, EndByte: 12, EvidenceRefs: []app.TaskValidationEvidenceRefRecord{first}},
		{ID: "c2", StartByte: 13, EndByte: int32(len(result)), EvidenceRefs: []app.TaskValidationEvidenceRefRecord{second}},
	}
	finding := app.TaskValidationReviewFinding{ID: app.TaskHardCheckEvidence, Assessment: app.TaskValidationReviewSupports,
		EvidenceRefs: []app.TaskValidationEvidenceRefRecord{first, second}}
	eligible := map[app.TaskValidationEvidenceRefRecord]struct{}{first: {}, second: {}}
	if !verifiedTaskWebContentCoverage(result, claims, []app.TaskValidationReviewFinding{finding}, eligible) {
		t.Fatal("fully covered claims with successful content evidence should verify")
	}

	for name, mutate := range map[string]func([]app.TaskValidationEvidenceClaimRecord, *app.TaskValidationReviewFinding, map[app.TaskValidationEvidenceRefRecord]struct{}){
		"uncovered text": func(value []app.TaskValidationEvidenceClaimRecord, _ *app.TaskValidationReviewFinding, _ map[app.TaskValidationEvidenceRefRecord]struct{}) {
			value[1].StartByte++
		},
		"reviewer cites different evidence": func(_ []app.TaskValidationEvidenceClaimRecord, value *app.TaskValidationReviewFinding, _ map[app.TaskValidationEvidenceRefRecord]struct{}) {
			value.EvidenceRefs = []app.TaskValidationEvidenceRefRecord{first}
		},
		"unverified action": func(_ []app.TaskValidationEvidenceClaimRecord, _ *app.TaskValidationReviewFinding, value map[app.TaskValidationEvidenceRefRecord]struct{}) {
			delete(value, second)
		},
		"non-supporting finding": func(_ []app.TaskValidationEvidenceClaimRecord, value *app.TaskValidationReviewFinding, _ map[app.TaskValidationEvidenceRefRecord]struct{}) {
			value.Assessment = app.TaskValidationReviewInsufficient
		},
	} {
		t.Run(name, func(t *testing.T) {
			copiedClaims := append([]app.TaskValidationEvidenceClaimRecord(nil), claims...)
			copiedFinding := finding
			copiedEligible := make(map[app.TaskValidationEvidenceRefRecord]struct{}, len(eligible))
			for ref := range eligible {
				copiedEligible[ref] = struct{}{}
			}
			mutate(copiedClaims, &copiedFinding, copiedEligible)
			if verifiedTaskWebContentCoverage(result, copiedClaims, []app.TaskValidationReviewFinding{copiedFinding}, copiedEligible) {
				t.Fatal("invalid content coverage passed")
			}
		})
	}
}

func TestPostgresTaskValidationFactsBindWebContentAndExcludeWorkspaceBytes(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := f.service.Start(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	current, err := f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil || current.Task.CurrentRevision == nil {
		t.Fatalf("load running Task revision: task=%+v err=%v", current.Task, err)
	}
	var sequence int64
	if err := f.store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM task_steps WHERE task_id=$1`, current.Task.ID).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]app.TaskValidationEvidenceRefRecord, 0, 2)
	for index, toolName := range []string{"morde_web_read", "morde_workspace_read"} {
		stepID := mustID(t, domain.NewStepID)
		callID := []string{"web-content-evidence", "workspace-content-evidence"}[index]
		sourceText := []string{"Public source passage.", "Authorized Workspace passage."}[index]
		contentDigest := sha256.Sum256([]byte(sourceText))
		requestDigest := sha256.Sum256([]byte(toolName + ":validated-read-request"))
		document := app.TaskStepDetailDocument{Object: []string{"https://example.com", "notes/project.md"}[index],
			Input: "Read authorized source.", Result: "Read completed.",
			ContentEvidence: &app.TaskStepContentEvidence{SHA256: hex.EncodeToString(contentDigest[:]), Preview: sourceText}}
		plain, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		ciphertext, err := sealer.SealWithAAD(plain, aadForTaskStepDetail(current.Task.OwnerID, current.Task.ID, stepID, *current.Task.CurrentRevision))
		if err != nil {
			t.Fatal(err)
		}
		detailDigest := sha256.Sum256(plain)
		if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_steps
(id,task_id,sequence,kind,status,title,summary,agent_session_id,task_revision,tool_call_key,tool_name,request_digest,phase,detail_state)
VALUES ($1,$2,$3,'tool','succeeded','Read source','Read completed.',$4,$5,$6,$7,$8,'succeeded','available')`,
			stepID, current.Task.ID, sequence+int64(index), lease.Fence.AgentSessionID, *current.Task.CurrentRevision, callID, toolName, requestDigest[:]); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_step_details
(task_id,owner_id,step_id,task_revision,payload_ciphertext,payload_sha256,payload_size_bytes,content_evidence_sha256)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, current.Task.ID, current.Task.OwnerID, stepID, *current.Task.CurrentRevision,
			ciphertext, detailDigest[:], int32(len(plain)), contentDigest[:]); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, app.TaskValidationEvidenceRefRecord{Kind: app.TaskValidationEvidenceAction, ID: stepID,
			TaskRevision: *current.Task.CurrentRevision, Sequence: sequence + int64(index), CallID: callID})
	}
	result := "Public fact."
	evidence := &app.TaskValidationEvidenceRecord{TaskRevision: *current.Task.CurrentRevision,
		Claims: []app.TaskValidationEvidenceClaimRecord{{ID: "c1", StartByte: 0, EndByte: int32(len(result)),
			EvidenceRefs: []app.TaskValidationEvidenceRefRecord{refs[0]}}}}
	report := app.TaskValidationReviewReport{Findings: []app.TaskValidationReviewFinding{{ID: app.TaskHardCheckEvidence,
		Assessment: app.TaskValidationReviewSupports, EvidenceRefs: []app.TaskValidationEvidenceRefRecord{refs[0]}}}}
	readFacts := func() app.TaskValidationVerdictFacts {
		t.Helper()
		var facts app.TaskValidationVerdictFacts
		if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
			var factErr error
			facts, factErr = loadTaskValidationVerdictFacts(ctx, tx, current.Task.ID, result, evidence, report)
			return factErr
		}); err != nil {
			t.Fatal(err)
		}
		return facts
	}
	if facts := readFacts(); !facts.EvidenceContentVerified || facts.TaskExecution != app.TaskValidationFactPresent {
		t.Fatalf("facts with reviewed public web content = %+v", facts)
	}
	result = "Public fact. Workspace fact."
	workspaceStart := len("Public fact. ")
	evidence.Claims = []app.TaskValidationEvidenceClaimRecord{
		{ID: "c1", StartByte: 0, EndByte: int32(len("Public fact.")), EvidenceRefs: []app.TaskValidationEvidenceRefRecord{refs[0]}},
		{ID: "c2", StartByte: int32(workspaceStart), EndByte: int32(len(result)), EvidenceRefs: []app.TaskValidationEvidenceRefRecord{refs[1]}},
	}
	report.Findings[0].EvidenceRefs = refs
	if facts := readFacts(); facts.EvidenceContentVerified {
		t.Fatalf("synthetic Workspace bytes incorrectly unlocked H02: %+v", facts)
	}
	result = "Public fact."
	evidence.Claims = []app.TaskValidationEvidenceClaimRecord{{ID: "c1", StartByte: 0, EndByte: int32(len(result)),
		EvidenceRefs: []app.TaskValidationEvidenceRefRecord{refs[0]}}}
	report.Findings[0].EvidenceRefs = []app.TaskValidationEvidenceRefRecord{refs[0]}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_step_details SET task_revision=task_revision+1 WHERE task_id=$1 AND step_id=$2`,
		current.Task.ID, refs[0].ID); err != nil {
		t.Fatal(err)
	}
	if facts := readFacts(); facts.EvidenceContentVerified {
		t.Fatalf("facts with a stale web step-detail revision verified H02: %+v", facts)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_step_details SET task_revision=$3 WHERE task_id=$1 AND step_id=$2`,
		current.Task.ID, refs[0].ID, *current.Task.CurrentRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_step_details SET content_evidence_sha256=NULL WHERE task_id=$1 AND step_id=$2`, current.Task.ID, refs[0].ID); err != nil {
		t.Fatal(err)
	}
	if facts := readFacts(); facts.EvidenceContentVerified {
		t.Fatalf("facts without persisted web content hash verified H02: %+v", facts)
	}
}
