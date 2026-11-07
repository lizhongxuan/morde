package postgres

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/project"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestProjectWorkItemCursorIsStableAndProjectScoped(t *testing.T) {
	createdAt := time.Date(2026, 11, 2, 3, 4, 5, 123456000, time.UTC)
	want := projectWorkItemCursor{ProjectID: "j7654321", CreatedAt: createdAt, ID: "j1234567", IncludeUnassigned: true}
	encoded, err := encodeProjectWorkItemCursor(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeProjectWorkItemCursor(encoded, want.ProjectID, true)
	if err != nil || got == nil || !got.CreatedAt.Equal(want.CreatedAt) || *got != want {
		t.Fatalf("cursor round trip = %#v, %v; want %#v", got, err, want)
	}
	if _, err := decodeProjectWorkItemCursor(encoded, want.ProjectID, false); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("cross-filter cursor error = %v, want invalid argument", err)
	}
	if _, err := decodeProjectWorkItemCursor(encoded, "j0000000", true); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("cross-project cursor error = %v, want invalid argument", err)
	}
	legacy := base64.RawURLEncoding.EncodeToString([]byte(`{"project_id":"j7654321","created_at":"2026-11-02T03:04:05.123456Z","id":"j1234567"}`))
	if _, err := decodeProjectWorkItemCursor(legacy, want.ProjectID, false); err != nil {
		t.Fatalf("legacy default-filter cursor error = %v, want success", err)
	}
	if _, err := decodeProjectWorkItemCursor(legacy, want.ProjectID, true); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("legacy cursor accepted for expanded filter: %v", err)
	}
	if _, err := decodeProjectWorkItemCursor("!invalid!", want.ProjectID, true); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("malformed cursor error = %v, want invalid argument", err)
	}
}

func TestProjectWorkItemIdempotencyDigestsUseLogicalRequest(t *testing.T) {
	first := project.WorkItem{
		ID: "j1234567", ProjectID: "j7654321", Title: "Deliver report", Description: "summary",
		AcceptanceCriteria: []string{"saved"}, AssigneeID: "u1234567", State: project.WorkItemOpen, Version: 1,
	}
	second := first
	second.ID = "j7654328"
	firstDigest, err := hashProjectWorkItemCreate(first)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := hashProjectWorkItemCreate(second)
	if err != nil {
		t.Fatal(err)
	}
	if !equalBytes(firstDigest, secondDigest) {
		t.Fatal("create digest included the generated work item ID")
	}
	second.Title = "Different request"
	changedDigest, err := hashProjectWorkItemCreate(second)
	if err != nil {
		t.Fatal(err)
	}
	if equalBytes(firstDigest, changedDigest) {
		t.Fatal("changed create payload retained the same digest")
	}
	second = first
	second.Category = project.WorkItemCategoryBug
	changedCategoryDigest, err := hashProjectWorkItemCreate(second)
	if err != nil || equalBytes(firstDigest, changedCategoryDigest) {
		t.Fatalf("category change retained create digest: %x, %x, %v", firstDigest, changedCategoryDigest, err)
	}

}

func TestGetProjectWorkItemMutationChecksAndReplaysSameDigest(t *testing.T) {
	ctx := context.Background()
	digest := []byte("01234567890123456789012345678901")
	mutation := app.ProjectWorkItemMutation{
		Operation: "update_work_item", WorkItemID: "j1234567", IdempotencyKey: "update-key-01", RequestDigest: digest,
	}
	t.Run("not found", func(t *testing.T) {
		tx := &projectWorkItemTxStub{rows: []pgx.Row{
			projectWorkItemRowStub{values: []any{"member"}},
			projectWorkItemRowStub{err: pgx.ErrNoRows},
		}}
		item, replayed, err := getProjectWorkItemMutationInTx(ctx, tx, "u1234567", "j7654321", mutation)
		if err != nil || replayed || item.ID != "" || len(tx.queries) != 2 {
			t.Fatalf("missing mutation = %#v replayed=%t err=%v queries=%#v", item, replayed, err, tx.queries)
		}
	})
	t.Run("exact replay", func(t *testing.T) {
		createdAt := time.Date(2026, 11, 2, 3, 4, 5, 0, time.UTC)
		tx := &projectWorkItemTxStub{rows: []pgx.Row{
			projectWorkItemRowStub{values: []any{"member"}},
			projectWorkItemRowStub{values: []any{"j7654321", "work_item", "j1234567", int64(2), digest}},
			projectWorkItemRowStub{values: []any{
				"j7654321", "j1234567", "Report", "summary", []byte(`[]`), "unclassified", pgtype.Text{String: "u1234567", Valid: true}, "blocked", "waiting",
				pgtype.Text{}, pgtype.Text{}, pgtype.Int8{}, int64(2), createdAt, []string{},
			}},
			projectWorkItemRowStub{values: []any{"member"}},
		}}
		item, replayed, err := getProjectWorkItemMutationInTx(ctx, tx, "u1234567", "j7654321", mutation)
		if err != nil || !replayed || item.ID != mutation.WorkItemID || item.State != project.WorkItemBlocked || item.Version != 2 {
			t.Fatalf("mutation replay = %#v replayed=%t err=%v", item, replayed, err)
		}
		if len(tx.queries) != 5 || !strings.Contains(tx.queries[1], "project_idempotency_keys") || !strings.Contains(tx.queries[1], "FOR SHARE") {
			t.Fatalf("mutation replay queries = %#v", tx.queries)
		}
	})
	t.Run("Task link replay rechecks Task initiator in the transaction", func(t *testing.T) {
		createdAt := time.Date(2026, 11, 2, 3, 4, 5, 0, time.UTC)
		tx := &projectWorkItemTxStub{
			rows: []pgx.Row{
				projectWorkItemRowStub{values: []any{"member"}},
				projectWorkItemRowStub{values: []any{"j7654321", "work_item", "j1234567", int64(2), digest}},
				projectWorkItemRowStub{values: []any{
					"j7654321", "j1234567", "Report", "summary", []byte(`[]`), "unclassified", pgtype.Text{String: "u1234567", Valid: true}, "open", "",
					pgtype.Text{}, pgtype.Text{}, pgtype.Int8{}, int64(2), createdAt, []string{"q1234567"},
				}},
				projectWorkItemRowStub{values: []any{"member"}},
				projectWorkItemRowStub{values: []any{
					pgtype.Text{String: "j7654321", Valid: true}, "u1234567", pgtype.Text{}, pgtype.Int8{},
				}},
			},
			resultSets: []pgx.Rows{&projectWorkItemRowsStub{records: [][]any{{"q1234567", "u1234567"}}}},
		}
		item, replayed, err := getProjectWorkItemMutationInTx(ctx, tx, "u1234567", "j7654321", mutation)
		if err != nil || !replayed || len(item.TaskIDs) != 1 || len(tx.queries) != 6 ||
			!strings.Contains(tx.queries[4], "project_work_item_tasks") || !strings.Contains(tx.queries[4], "FOR SHARE") ||
			!strings.Contains(tx.queries[5], "FROM tasks") || !strings.Contains(tx.queries[5], "FOR SHARE") {
			t.Fatalf("Task link replay = %#v replayed=%t err=%v queries=%#v", item, replayed, err, tx.queries)
		}
	})
	t.Run("mismatched digest conflicts", func(t *testing.T) {
		tx := &projectWorkItemTxStub{row: projectWorkItemRowStub{values: []any{"j7654321", "work_item", "j1234567", int64(2), []byte("different digest")}}}
		if _, _, err := findProjectWorkItemIdempotency(ctx, tx, "u1234567", "j7654321", mutation); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
			t.Fatalf("mismatched digest error = %v, want conflict", err)
		}
	})
}

func TestValidateProjectWorkItemMutationRejectsIncompleteIdentity(t *testing.T) {
	valid := app.ProjectWorkItemMutation{
		Operation: "update_work_item", WorkItemID: "j1234567", IdempotencyKey: "update-key-01", RequestDigest: make([]byte, 32),
	}
	if err := validateProjectWorkItemMutation(valid); err != nil {
		t.Fatalf("valid mutation rejected: %v", err)
	}
	invalid := valid
	invalid.RequestDigest = invalid.RequestDigest[:31]
	if domain.CodeOf(validateProjectWorkItemMutation(invalid)) != domain.CodeInvalidArgument {
		t.Fatalf("short digest = %v, want invalid argument", validateProjectWorkItemMutation(invalid))
	}
}

func TestRequireProjectWorkItemTaskUsesProjectAndInitiatorUnderLock(t *testing.T) {
	ctx := context.Background()
	t.Run("matching project initiator allowed", func(t *testing.T) {
		tx := &projectWorkItemTxStub{row: projectWorkItemRowStub{values: []any{
			pgtype.Text{String: "j7654321", Valid: true}, "u1234567", pgtype.Text{}, pgtype.Int8{},
		}}}
		if _, err := requireProjectWorkItemTaskInTx(ctx, tx, "j7654321", "u1234567", "q1234567"); err != nil {
			t.Fatalf("matching Task rejected: %v", err)
		}
		if !strings.Contains(tx.query, "project_id,initiator_id") || !strings.Contains(tx.query, "FOR SHARE") ||
			!reflect.DeepEqual(tx.args, []any{"q1234567"}) {
			t.Fatalf("Task authorization query = %q args=%#v", tx.query, tx.args)
		}
	})
	t.Run("different project denied", func(t *testing.T) {
		tx := &projectWorkItemTxStub{row: projectWorkItemRowStub{values: []any{
			pgtype.Text{String: "j0000000", Valid: true}, "u1234567", pgtype.Text{}, pgtype.Int8{},
		}}}
		if _, err := requireProjectWorkItemTaskInTx(ctx, tx, "j7654321", "u1234567", "q1234567"); domain.CodeOf(err) != domain.CodeForbidden {
			t.Fatalf("cross-project Task = %v, want forbidden", err)
		}
	})
	t.Run("different initiator denied", func(t *testing.T) {
		tx := &projectWorkItemTxStub{row: projectWorkItemRowStub{values: []any{
			pgtype.Text{String: "j7654321", Valid: true}, "u8765432", pgtype.Text{}, pgtype.Int8{},
		}}}
		if _, err := requireProjectWorkItemTaskInTx(ctx, tx, "j7654321", "u1234567", "q1234567"); domain.CodeOf(err) != domain.CodeForbidden {
			t.Fatalf("different Task initiator = %v, want forbidden", err)
		}
	})
}

func TestRequireProjectWorkItemDeliveryMatchesTaskCheckpoint(t *testing.T) {
	ctx := context.Background()
	evidence := project.WorkItemDelivery{TaskID: "q1234567", WorkspaceID: "w1234567", Revision: 4}
	matchingTask := []any{
		pgtype.Text{String: "j7654321", Valid: true}, "u1234567",
		pgtype.Text{String: "w1234567", Valid: true}, pgtype.Int8{Int64: 4, Valid: true},
	}
	matchingDeliveryTask := []any{
		pgtype.Text{String: "j7654321", Valid: true},
		pgtype.Text{String: "w1234567", Valid: true}, pgtype.Int8{Int64: 4, Valid: true},
	}
	t.Run("matching saved checkpoint", func(t *testing.T) {
		tx := &projectWorkItemTxStub{rows: []pgx.Row{
			projectWorkItemRowStub{values: []any{"u1234567"}},
			projectWorkItemRowStub{values: matchingTask},
			projectWorkItemRowStub{values: []any{true}},
			projectWorkItemRowStub{values: matchingDeliveryTask},
			projectWorkItemRowStub{values: []any{true}},
		}, resultSets: []pgx.Rows{&projectWorkItemRowsStub{records: [][]any{{1}}}}}
		if err := requireProjectWorkItemDeliveryInTx(ctx, tx, "j7654321", "j7654322", evidence); err != nil {
			t.Fatalf("matching saved checkpoint rejected: %v", err)
		}
		if len(tx.queries) != 6 || !strings.Contains(tx.queries[0], "project_work_item_tasks") || !strings.Contains(tx.queries[0], "FOR SHARE") ||
			!strings.Contains(tx.queries[1], "FOR SHARE") || !strings.Contains(tx.queries[2], "workspace_revisions") ||
			!strings.Contains(tx.queries[4], "project_resources") || !strings.Contains(tx.queries[5], "task_workspace_publish_receipts") {
			t.Fatalf("delivery authorization queries = %#v", tx.queries)
		}
	})
	t.Run("different checkpoint denied", func(t *testing.T) {
		wrongRevision := append([]any(nil), matchingTask...)
		wrongRevision[3] = pgtype.Int8{Int64: 5, Valid: true}
		tx := &projectWorkItemTxStub{rows: []pgx.Row{
			projectWorkItemRowStub{values: []any{"u1234567"}},
			projectWorkItemRowStub{values: wrongRevision},
		}}
		if err := requireProjectWorkItemDeliveryInTx(ctx, tx, "j7654321", "j7654322", evidence); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
			t.Fatalf("mismatched Task checkpoint = %v, want Workspace conflict", err)
		}
	})
	t.Run("missing revision denied", func(t *testing.T) {
		tx := &projectWorkItemTxStub{rows: []pgx.Row{
			projectWorkItemRowStub{values: []any{"u1234567"}},
			projectWorkItemRowStub{values: matchingTask},
			projectWorkItemRowStub{err: pgx.ErrNoRows},
		}}
		if err := requireProjectWorkItemDeliveryInTx(ctx, tx, "j7654321", "j7654322", evidence); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
			t.Fatalf("missing Workspace revision = %v, want Workspace conflict", err)
		}
	})
}

func TestRequireProjectWorkItemMemberUsesFreshLockedMembership(t *testing.T) {
	ctx := context.Background()
	t.Run("member allowed", func(t *testing.T) {
		tx := &projectWorkItemTxStub{row: projectWorkItemRowStub{values: []any{"member"}}}
		role, err := requireProjectWorkItemMember(ctx, tx, "u1234567", "j7654321", project.RoleMember)
		if err != nil || role != project.RoleMember || !strings.Contains(tx.query, "FROM project_members") ||
			!strings.Contains(tx.query, "FOR SHARE") || !reflect.DeepEqual(tx.args, []any{"j7654321", "u1234567"}) {
			t.Fatalf("membership check = %q, %v; query=%q args=%#v", role, err, tx.query, tx.args)
		}
	})
	t.Run("viewer cannot mutate", func(t *testing.T) {
		tx := &projectWorkItemTxStub{row: projectWorkItemRowStub{values: []any{"viewer"}}}
		if _, err := requireProjectWorkItemMember(ctx, tx, "u1234567", "j7654321", project.RoleMember); domain.CodeOf(err) != domain.CodeForbidden {
			t.Fatalf("viewer write check = %v, want forbidden", err)
		}
	})
	t.Run("revoked member hidden", func(t *testing.T) {
		tx := &projectWorkItemTxStub{row: projectWorkItemRowStub{err: pgx.ErrNoRows}}
		if _, err := requireProjectWorkItemMember(ctx, tx, "u1234567", "j7654321", project.RoleViewer); domain.CodeOf(err) != domain.CodeNotFound {
			t.Fatalf("revoked member read = %v, want not found", err)
		}
	})
}

func TestRequireProjectWorkItemAssigneeUsesActiveAssignableMembership(t *testing.T) {
	ctx := context.Background()
	t.Run("member allowed", func(t *testing.T) {
		tx := &projectWorkItemTxStub{row: projectWorkItemRowStub{values: []any{"member"}}}
		if err := requireProjectWorkItemAssignee(ctx, tx, "j7654321", "u1234567"); err != nil {
			t.Fatalf("active member assignee rejected: %v", err)
		}
		if !strings.Contains(tx.query, "project_members") || !strings.Contains(tx.query, "FOR SHARE") {
			t.Fatalf("assignee membership query was not freshly locked: %q", tx.query)
		}
	})
	t.Run("viewer cannot be assigned", func(t *testing.T) {
		tx := &projectWorkItemTxStub{row: projectWorkItemRowStub{values: []any{"viewer"}}}
		if err := requireProjectWorkItemAssignee(ctx, tx, "j7654321", "u1234567"); domain.CodeOf(err) != domain.CodeConflict {
			t.Fatalf("viewer assignee = %v, want conflict", err)
		}
	})
	t.Run("revoked member cannot be assigned", func(t *testing.T) {
		tx := &projectWorkItemTxStub{row: projectWorkItemRowStub{err: pgx.ErrNoRows}}
		if err := requireProjectWorkItemAssignee(ctx, tx, "j7654321", "u1234567"); domain.CodeOf(err) != domain.CodeConflict {
			t.Fatalf("revoked assignee = %v, want conflict", err)
		}
	})
}

func TestReserveProjectWorkItemIdempotencyReplayAndConflict(t *testing.T) {
	ctx := context.Background()
	digest := []byte("01234567890123456789012345678901")
	t.Run("new key", func(t *testing.T) {
		tx := &projectWorkItemTxStub{execTag: pgconn.NewCommandTag("INSERT 0 1")}
		replayed, resultID, err := reserveProjectWorkItemIdempotency(ctx, tx, "u1234567", "create_work_item",
			"create-key-01", "j7654321", "j1234567", 1, digest)
		if err != nil || replayed || resultID != "j1234567" {
			t.Fatalf("new key = replayed %t id %q err %v", replayed, resultID, err)
		}
	})
	t.Run("same request replay", func(t *testing.T) {
		tx := &projectWorkItemTxStub{
			execTag: pgconn.NewCommandTag("INSERT 0 0"),
			row:     projectWorkItemRowStub{values: []any{"j7654321", "work_item", "j1234567", int64(1), digest}},
		}
		replayed, resultID, err := reserveProjectWorkItemIdempotency(ctx, tx, "u1234567", "create_work_item",
			"create-key-01", "j7654321", "j8765432", 1, digest)
		if err != nil || !replayed || resultID != "j1234567" {
			t.Fatalf("replay = replayed %t id %q err %v", replayed, resultID, err)
		}
	})
	t.Run("different request conflicts", func(t *testing.T) {
		tx := &projectWorkItemTxStub{
			execTag: pgconn.NewCommandTag("INSERT 0 0"),
			row:     projectWorkItemRowStub{values: []any{"j7654321", "work_item", "j1234567", int64(1), []byte("different request digest bytes here")}},
		}
		if _, _, err := reserveProjectWorkItemIdempotency(ctx, tx, "u1234567", "create_work_item",
			"create-key-01", "j7654321", "j8765432", 1, digest); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
			t.Fatalf("different request = %v, want idempotency conflict", err)
		}
	})
}

func TestValidateProjectWorkItemUpdatePreservesTaskAndDeliveryEvidence(t *testing.T) {
	current := project.WorkItem{
		ID: "j1234567", ProjectID: "j7654321", Title: "Report", AssigneeID: "u1234567",
		State: project.WorkItemOpen, TaskIDs: []string{"q1234567"}, Version: 1,
	}
	updated := current
	updated.Version = 2
	updated.State = project.WorkItemBlocked
	updated.BlockedReason = "waiting for review"
	if err := validateProjectWorkItemUpdate(current, updated, 1); err != nil {
		t.Fatalf("valid block update rejected: %v", err)
	}
	if err := validateProjectWorkItemUpdate(current, updated, 2); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale expected version = %v, want conflict", err)
	}

	removedTask := updated
	removedTask.TaskIDs = nil
	if err := validateProjectWorkItemUpdate(current, removedTask, 1); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("task removal = %v, want invalid argument", err)
	}

	tooManyLinks := updated
	tooManyLinks.TaskIDs = []string{"q1234567", "q7654321", "q8888888"}
	if err := validateProjectWorkItemUpdate(current, tooManyLinks, 1); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("multiple task links = %v, want invalid argument", err)
	}

	delivered := current
	delivered.State = project.WorkItemDelivered
	delivered.Delivery = &project.WorkItemDelivery{TaskID: "q1234567", WorkspaceID: "w1234567", Revision: 4}
	delivered.Version = 2
	if err := validateProjectWorkItemUpdate(current, delivered, 1); err != nil {
		t.Fatalf("valid delivery update rejected: %v", err)
	}
	mutatedDelivery := delivered
	mutatedDelivery.Version = 3
	mutatedDelivery.Delivery = &project.WorkItemDelivery{TaskID: "q1234567", WorkspaceID: "w1234567", Revision: 5}
	if err := validateProjectWorkItemUpdate(delivered, mutatedDelivery, 2); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("delivered evidence mutation = %v, want conflict", err)
	}
	blocked := current
	blocked.State = project.WorkItemBlocked
	blocked.BlockedReason = "waiting"
	blocked.Version = 2
	blockedDelivery := blocked
	blockedDelivery.State = project.WorkItemDelivered
	blockedDelivery.BlockedReason = ""
	blockedDelivery.Delivery = &project.WorkItemDelivery{TaskID: "q1234567", WorkspaceID: "w1234567", Revision: 4}
	blockedDelivery.Version = 3
	if err := validateProjectWorkItemUpdate(blocked, blockedDelivery, 2); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("delivery without resume = %v, want conflict", err)
	}
}

func TestScanProjectWorkItemKeepsSavedDeliveryEvidence(t *testing.T) {
	createdAt := time.Date(2026, 11, 2, 3, 4, 5, 0, time.UTC)
	tx := projectWorkItemRowStub{values: []any{
		"j7654321", "j1234567", "Report", "summary", []byte(`[]`), "unclassified", pgtype.Text{String: "u1234567", Valid: true}, "delivered", "",
		pgtype.Text{String: "q1234567", Valid: true}, pgtype.Text{String: "w1234567", Valid: true},
		pgtype.Int8{Int64: 4, Valid: true}, int64(3), createdAt, []string{"q1234567"},
	}}
	got, err := scanProjectWorkItem(tx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Item.ID != "j1234567" || got.Item.State != project.WorkItemDelivered || got.Item.Delivery == nil ||
		*got.Item.Delivery != (project.WorkItemDelivery{TaskID: "q1234567", WorkspaceID: "w1234567", Revision: 4}) ||
		got.CreatedAt != createdAt || got.Item.Validate() != nil {
		t.Fatalf("scanned work item = %#v", got)
	}

	broken := tx
	broken.values = append([]any(nil), tx.values...)
	broken.values[9] = pgtype.Text{}
	if _, err := scanProjectWorkItem(broken); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("partial delivery columns = %v, want an error", err)
	}

	unassigned := projectWorkItemRowStub{values: []any{
		"j7654321", "j1234568", "Unclaimed", "", []byte(`[]`), "unclassified", pgtype.Text{}, "open", "",
		pgtype.Text{}, pgtype.Text{}, pgtype.Int8{}, int64(1), createdAt, []string{},
	}}
	got, err = scanProjectWorkItem(unassigned)
	if err != nil || got.Item.AssigneeID != "" || got.Item.Category != project.WorkItemCategoryUnclassified || got.Item.State != project.WorkItemOpen || got.Item.Validate() != nil {
		t.Fatalf("scanned unassigned work item = %#v, %v", got, err)
	}
}

type projectWorkItemTxStub struct {
	pgx.Tx
	query      string
	args       []any
	row        pgx.Row
	rows       []pgx.Row
	resultSets []pgx.Rows
	queries    []string
	execTag    pgconn.CommandTag
	execErr    error
	queryErr   error
}

func (tx *projectWorkItemTxStub) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	tx.query = query
	tx.queries = append(tx.queries, query)
	tx.args = append([]any(nil), args...)
	if len(tx.rows) != 0 {
		row := tx.rows[0]
		tx.rows = tx.rows[1:]
		return row
	}
	if tx.row != nil {
		return tx.row
	}
	return projectWorkItemRowStub{err: errors.New("unexpected QueryRow")}
}

func (tx *projectWorkItemTxStub) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return tx.execTag, tx.execErr
}

func (tx *projectWorkItemTxStub) Query(_ context.Context, query string, args ...any) (pgx.Rows, error) {
	tx.query = query
	tx.queries = append(tx.queries, query)
	tx.args = append([]any(nil), args...)
	if tx.queryErr != nil {
		return nil, tx.queryErr
	}
	if len(tx.resultSets) == 0 {
		return &projectWorkItemRowsStub{}, nil
	}
	rows := tx.resultSets[0]
	tx.resultSets = tx.resultSets[1:]
	return rows, nil
}

type projectWorkItemRowStub struct {
	values []any
	err    error
}

type projectWorkItemRowsStub struct {
	records [][]any
	index   int
	err     error
}

func (rows *projectWorkItemRowsStub) Close()                                       {}
func (rows *projectWorkItemRowsStub) Err() error                                   { return rows.err }
func (rows *projectWorkItemRowsStub) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (rows *projectWorkItemRowsStub) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (rows *projectWorkItemRowsStub) Next() bool {
	if rows.index >= len(rows.records) {
		return false
	}
	rows.index++
	return true
}
func (rows *projectWorkItemRowsStub) Scan(dest ...any) error {
	if rows.index == 0 || rows.index > len(rows.records) {
		return errors.New("project work item rows are not positioned on a record")
	}
	return projectWorkItemRowStub{values: rows.records[rows.index-1]}.Scan(dest...)
}
func (rows *projectWorkItemRowsStub) Values() ([]any, error) { return nil, nil }
func (rows *projectWorkItemRowsStub) RawValues() [][]byte    { return nil }
func (rows *projectWorkItemRowsStub) Conn() *pgx.Conn        { return nil }
func (rows *projectWorkItemRowsStub) TypeMap() *pgtype.Map   { return nil }

func (row projectWorkItemRowStub) Scan(dest ...any) error {
	if row.err != nil {
		return row.err
	}
	if len(dest) != len(row.values) {
		return errors.New("unexpected number of scan destinations")
	}
	for index, value := range row.values {
		field := reflect.ValueOf(dest[index])
		if field.Kind() != reflect.Pointer || field.IsNil() {
			return errors.New("scan destination is not a pointer")
		}
		field = field.Elem()
		if value == nil {
			field.Set(reflect.Zero(field.Type()))
			continue
		}
		valueField := reflect.ValueOf(value)
		if !valueField.Type().AssignableTo(field.Type()) {
			return errors.New("scan value has the wrong type")
		}
		field.Set(valueField)
	}
	return nil
}
