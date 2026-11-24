package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// withValidationEvidencePreview adds response-only evidence and an incomplete
// artifact proof for the actual-PC presentation check. It never writes these
// values to the database and is disabled unless the local acceptance test
// opts in.
func (a *browserAcceptanceActivity) withValidationEvidencePreview(ctx context.Context, taskID string, body []byte) ([]byte, bool, error) {
	if os.Getenv("MORDE_BROWSER_ACTIVITY_VALIDATION_PREVIEW") != "1" {
		return body, false, nil
	}
	a.mu.Lock()
	run := a.runs["search"]
	eligible := run != nil && run.stage == "completed" && run.current.ID == taskID
	a.mu.Unlock()
	if !eligible {
		return body, false, nil
	}

	var snapshot map[string]json.RawMessage
	if err := json.Unmarshal(body, &snapshot); err != nil {
		return nil, false, fmt.Errorf("decode Task snapshot for validation preview: %w", err)
	}
	var result string
	var status string
	var revision int64
	var eventSequence int64
	if err := json.Unmarshal(snapshot["result"], &result); err != nil || result == "" {
		return nil, false, errors.New("completed search Task has no result for validation preview")
	}
	if err := json.Unmarshal(snapshot["status"], &status); err != nil || status != "completed" {
		return nil, false, errors.New("validation preview requires a completed Task snapshot")
	}
	if err := json.Unmarshal(snapshot["current_revision"], &revision); err != nil || revision < 0 {
		return nil, false, errors.New("completed search Task has no current revision")
	}
	if err := json.Unmarshal(snapshot["event_sequence"], &eventSequence); err != nil || eventSequence < 1 {
		return nil, false, errors.New("completed search Task has no result event sequence")
	}

	const excerpt = "Morde 消息路由先持久接收消息，再异步分流。"
	startByte := strings.Index(result, excerpt)
	if startByte < 0 {
		return nil, false, errors.New("search result does not contain the evidence preview excerpt")
	}
	var sourceMessageID string
	var sourceSequence int64
	if err := a.store.pool.QueryRow(ctx, `SELECT id,sequence FROM conversation_messages
WHERE task_id=$1 AND role='user' AND delivery_status='applied' ORDER BY sequence DESC LIMIT 1`, taskID).
		Scan(&sourceMessageID, &sourceSequence); err != nil {
		return nil, false, fmt.Errorf("load current user source for validation preview: %w", err)
	}
	if sourceMessageID == "" || sourceSequence < 1 {
		return nil, false, errors.New("validation preview source message is invalid")
	}
	digest := sha256.Sum256([]byte(result))
	validation := json.RawMessage(`{"state":"FAIL"}`)
	detail, err := json.Marshal(map[string]any{
		"task_revision":  revision,
		"event_sequence": eventSequence,
		"result_sha256":  hex.EncodeToString(digest[:]),
		"claims": []any{
			map[string]any{
				"claim_id":   "local-ui-preview-claim",
				"start_byte": startByte,
				"end_byte":   startByte + len(excerpt),
				"evidence_refs": []any{map[string]any{
					"kind": "source", "id": sourceMessageID, "task_revision": revision, "sequence": sourceSequence,
				}},
			},
			map[string]any{
				"claim_id":   "local-ui-preview-unavailable-claim",
				"start_byte": startByte,
				"end_byte":   startByte + len(excerpt),
				"evidence_refs": []any{map[string]any{
					"kind": "side_effect", "id": "a0000002", "task_revision": revision, "sequence": 1,
				}},
			},
		},
	})
	if err != nil {
		return nil, false, fmt.Errorf("encode validation preview detail: %w", err)
	}
	snapshot["validation"] = validation
	snapshot["validation_detail"] = detail
	snapshot["artifacts"], err = json.Marshal([]any{map[string]any{
		"id": "a0000001", "step_id": "s0000001", "label": "local-preview-sensitive-label.txt",
		"path": "private/local-preview-sensitive-path.txt", "media_type": "text/plain", "workspace_id": nil,
		"workspace_revision": nil, "size_bytes": nil, "sha256": nil, "verified": false,
		"batch_item_id": nil, "batch_attempt": 1, "created_at": "2026-11-24T00:00:00Z",
	}})
	if err != nil {
		return nil, false, fmt.Errorf("encode response-only incomplete artifact proof: %w", err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return nil, false, fmt.Errorf("encode Task snapshot with validation preview: %w", err)
	}
	a.mu.Lock()
	a.validationPreviewInjected = true
	a.mu.Unlock()
	return encoded, true, nil
}
