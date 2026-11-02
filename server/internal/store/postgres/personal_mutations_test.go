package postgres

import (
	"testing"

	"morde/server/internal/domain"
)

func TestPersonalRevisionConflictIncludesCurrentRevision(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		err := personalRevisionConflict(5, 7, deleted)
		if err.Code != domain.CodeConflict || err.Details["reason"] != "personal_object_revision_changed" ||
			err.Details["base_revision"] != int64(5) || err.Details["current_revision"] != int64(7) || err.Details["deleted"] != deleted {
			t.Fatalf("revision conflict details = %#v", err)
		}
		if _, hasBody := err.Details["body"]; hasBody {
			t.Fatalf("revision conflict exposes object body: %#v", err.Details)
		}
	}
}
