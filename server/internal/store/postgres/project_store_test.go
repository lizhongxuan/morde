package postgres

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/project"
)

func TestProjectStoreIdempotencyKeyAndDigest(t *testing.T) {
	for _, value := range []string{"project-key-01", "Ab_12.~x"} {
		if !validProjectIdempotencyKey(value) {
			t.Errorf("valid project idempotency key %q was rejected", value)
		}
	}
	for _, value := range []string{"short", "project key with spaces", strings.Repeat("a", 129), "bad/key-123"} {
		if validProjectIdempotencyKey(value) {
			t.Errorf("invalid project idempotency key %q was accepted", value)
		}
	}

	type createRequest struct {
		Name        string
		Description string
	}
	first, err := projectRequestDigest(createRequest{Name: "Quarterly plan", Description: "Shared goals"})
	if err != nil {
		t.Fatal(err)
	}
	same, err := projectRequestDigest(createRequest{Name: "Quarterly plan", Description: "Shared goals"})
	if err != nil {
		t.Fatal(err)
	}
	different, err := projectRequestDigest(createRequest{Name: "Quarterly plan", Description: "Other goals"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 || !bytes.Equal(first, same) || bytes.Equal(first, different) {
		t.Fatalf("request digest equality/size is wrong: first=%x same=%x different=%x", first, same, different)
	}
}

func TestProjectResourceAccessRequiresIndependentPermission(t *testing.T) {
	if !hasHostLevelProjectPermission([]string{string(host.PermissionDesktopView)}) ||
		!hasHostLevelProjectPermission([]string{string(host.PermissionMCPControl)}) {
		t.Fatal("valid Host permissions were not recognized")
	}
	if hasHostLevelProjectPermission([]string{string(host.PermissionWorkspaceFileRead)}) {
		t.Fatal("Workspace file access was treated as Host access")
	}
	if !hasWorkspaceProjectPermission([]string{string(host.PermissionWorkspaceFileRead)}) ||
		hasWorkspaceProjectPermission([]string{string(host.PermissionDesktopView)}) {
		t.Fatal("Workspace access did not require its own file permission")
	}

	visibilityQuery := projectResourceSelect + " WHERE pr.project_id=$1 AND pr.active " + projectResourceAccessPredicate
	for _, fragment := range []string{
		"pr.active",
		"h.deleted_at IS NULL",
		"hm.revoked_at IS NULL",
		"hm.permissions && ARRAY['desktop:view','desktop:control','terminal:control','mcp:control']",
		"wb.state='active'",
		"hm.permissions @> ARRAY['workspace:file:read']",
	} {
		if !strings.Contains(visibilityQuery, fragment) {
			t.Errorf("project resource visibility predicate is missing %q", fragment)
		}
	}
}

func TestProjectStoreResourceAndContentValidation(t *testing.T) {
	if !validProjectResourceValue(project.Resource{ProjectID: "j1234567", Kind: project.ResourceHost, ResourceID: "h1234567", Version: 1}) {
		t.Fatal("valid Host resource was rejected")
	}
	if validProjectResourceValue(project.Resource{ProjectID: "j1234567", Kind: project.ResourceHost, ResourceID: "w1234567", Version: 1}) {
		t.Fatal("Workspace ID was accepted for a Host resource")
	}
	if validProjectResourceValue(project.Resource{ProjectID: "j1234567", Kind: project.ResourceWorkspace, ResourceID: "w1234567", Version: 0}) {
		t.Fatal("zero-version project resource was accepted")
	}

	valid := projectStoreRowStub{values: []any{int64(3), []byte(`{"content":"Shared context"}`), false, "u1234567"}}
	stored, err := scanStoredProjectContent(valid, "j1234567", project.ContentMemory, "m_0123abcd")
	if err != nil || stored.deleted || stored.content.Revision != 3 || string(stored.content.Body) != `{"content":"Shared context"}` {
		t.Fatalf("valid project content scan = %#v, %v", stored, err)
	}

	deleted := projectStoreRowStub{values: []any{int64(4), nil, true, "u1234567"}}
	stored, err = scanStoredProjectContent(deleted, "j1234567", project.ContentMemory, "m_0123abcd")
	if err != nil || !stored.deleted || stored.content.Revision != 4 || len(stored.content.Body) != 0 {
		t.Fatalf("deleted project content scan = %#v, %v", stored, err)
	}

	invalid := projectStoreRowStub{values: []any{int64(1), []byte(`{"content":"Shared","connector_secret":"secret"}`), false, "u1234567"}}
	if _, err := scanStoredProjectContent(invalid, "j1234567", project.ContentMemory, "m_0123abcd"); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("invalid stored project content error = %v, want internal", err)
	}
}

func TestProjectStoreSharedHelpersCheckMembershipAndExpectedVersion(t *testing.T) {
	if err := checkProjectExpectedVersion(2, 2); err != nil {
		t.Fatalf("matching project version rejected: %v", err)
	}
	if err := checkProjectExpectedVersion(1, 2); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale project version error = %v, want conflict", err)
	}

	tx := &projectStoreTxStub{row: projectStoreRowStub{values: []any{
		"j1234567", "u1234567", "Quarterly plan", "Shared goals", int64(4), "member", int64(2),
	}}}
	value, member, err := lockProjectForActorInTx(context.Background(), tx, "u1234567", "j1234567", project.RoleMember, false)
	if err != nil || value.ID != "j1234567" || member.Role != project.RoleMember || member.Version != 2 ||
		!strings.Contains(tx.query, "JOIN project_members") || !strings.Contains(tx.query, "status='approved'") ||
		!strings.Contains(tx.query, "FOR SHARE OF p,m,u") {
		t.Fatalf("locked project membership = %#v/%#v, %v; query=%q", value, member, err, tx.query)
	}

	viewerTx := &projectStoreTxStub{row: projectStoreRowStub{values: []any{
		"j1234567", "u1234567", "Quarterly plan", "Shared goals", int64(4), "viewer", int64(2),
	}}}
	if _, _, err := lockProjectForActorInTx(context.Background(), viewerTx, "u1234567", "j1234567", project.RoleMember, false); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("viewer mutation check = %v, want forbidden", err)
	}

	missingTx := &projectStoreTxStub{row: projectStoreRowStub{err: pgx.ErrNoRows}}
	if _, _, err := lockProjectForActorInTx(context.Background(), missingTx, "u1234567", "j1234567", project.RoleViewer, false); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("missing membership check = %v, want not found", err)
	}
}

type projectStoreTxStub struct {
	pgx.Tx
	query string
	row   pgx.Row
}

func (tx *projectStoreTxStub) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	tx.query = query
	return tx.row
}

type projectStoreRowStub struct {
	values []any
	err    error
}

func (row projectStoreRowStub) Scan(dest ...any) error {
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
