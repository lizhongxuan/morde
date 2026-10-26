package postgres

import (
	"testing"
	"time"
)

func TestPageCursorRoundTripAndUsesLastReturnedItem(t *testing.T) {
	createdAt := time.Date(2026, 10, 21, 12, 13, 14, 123456000, time.FixedZone("UTC+8", 8*60*60))
	cursor := encodePageCursor(createdAt, "q1234567")
	pageSize, cursorAt, cursorID, err := pagination(2, cursor)
	if err != nil || pageSize != 2 || cursorID != "q1234567" || !cursorAt.Valid || !cursorAt.Time.Equal(createdAt) {
		t.Fatalf("pagination cursor = limit:%d at:%v id:%q err:%v", pageSize, cursorAt, cursorID, err)
	}

	items := []struct {
		id string
		at time.Time
	}{{"q0000001", createdAt}, {"q0000002", createdAt.Add(-time.Minute)}, {"q0000003", createdAt.Add(-2 * time.Minute)}}
	next := nextPageCursor(items, 2, func(item struct {
		id string
		at time.Time
	}) (string, time.Time) {
		return item.id, item.at
	})
	decoded, err := decodePageCursor(next)
	if err != nil || decoded.ID != items[1].id || !decoded.CreatedAt.Equal(items[1].at) {
		t.Fatalf("next cursor skipped or repeated a row: decoded=%#v err=%v", decoded, err)
	}
}

func TestPaginationRejectsMalformedBounds(t *testing.T) {
	if _, _, _, err := pagination(101, ""); err == nil {
		t.Fatal("oversized page limit was accepted")
	}
	if _, _, _, err := pagination(20, "not-a-cursor"); err == nil {
		t.Fatal("malformed cursor was accepted")
	}
}

func TestBatchItemTransitionsAndArtifactPaths(t *testing.T) {
	if !canTransitionBatchItem("queued", "running") || !canTransitionBatchItem("unknown", "succeeded") {
		t.Fatal("expected supported batch item transitions to be allowed")
	}
	if canTransitionBatchItem("succeeded", "running") || canTransitionBatchItem("failed", "succeeded") {
		t.Fatal("terminal batch item transition was allowed")
	}
	for _, value := range []string{"artifacts/report.pdf", "exports/2026-10-21.json"} {
		if !validWorkspaceRelativeArtifactPath(value) {
			t.Errorf("valid relative artifact path %q was rejected", value)
		}
	}
	for _, value := range []string{"", ".", "../secret", "artifacts/../../secret", "/workspace/report.pdf", `artifacts\\report.pdf`} {
		if validWorkspaceRelativeArtifactPath(value) {
			t.Errorf("invalid relative artifact path %q was accepted", value)
		}
	}
}
