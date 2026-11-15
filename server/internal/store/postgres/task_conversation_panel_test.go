package postgres

import (
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

func TestConversationTaskPanelCursorCarriesStatusRankAndUpdatedAt(t *testing.T) {
	updatedAt := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	items := []task.Task{
		{ID: "q1234567", Status: task.StatusRunning, UpdatedAt: updatedAt},
		{ID: "q7654321", Status: task.StatusCompleted, UpdatedAt: updatedAt},
	}
	cursor := nextConversationTaskPanelCursor(items, 1)
	limit, cursorAt, cursorID, cursorRank, err := conversationTaskPagination(app.PageRequest{Limit: 1, Cursor: cursor}, app.ConversationTaskViewPanel)
	if err != nil || limit != 1 || !cursorAt.Valid || !cursorAt.Time.Equal(updatedAt) || cursorID != "q1234567" || cursorRank != 0 {
		t.Fatalf("running panel cursor = %d %v %q %d err=%v", limit, cursorAt, cursorID, cursorRank, err)
	}

	cursor = nextConversationTaskPanelCursor([]task.Task{items[1], items[0]}, 1)
	_, cursorAt, cursorID, cursorRank, err = conversationTaskPagination(app.PageRequest{Limit: 1, Cursor: cursor}, app.ConversationTaskViewPanel)
	if err != nil || !cursorAt.Time.Equal(updatedAt) || cursorID != "q7654321" || cursorRank != 1 {
		t.Fatalf("non-running panel cursor = %v %q %d err=%v", cursorAt, cursorID, cursorRank, err)
	}
	if nextConversationTaskPanelCursor(items[:1], 1) != "" {
		t.Fatal("panel cursor returned when there is no next page")
	}
	bad := app.EncodePageCursor(updatedAt, "2:q1234567")
	if _, _, _, _, err := conversationTaskPagination(app.PageRequest{Limit: 1, Cursor: bad}, app.ConversationTaskViewPanel); err == nil {
		t.Fatal("invalid panel cursor rank was accepted")
	}
}
