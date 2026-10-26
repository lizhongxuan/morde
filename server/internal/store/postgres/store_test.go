package postgres

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

type fakeDBTX struct {
	err          error
	rowsAffected int64
	queryCapture *queryCapture
}

type queryCapture struct {
	query string
	args  []any
}

func (db fakeDBTX) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag(fmt.Sprintf("UPDATE %d", db.rowsAffected)), db.err
}

func (db fakeDBTX) Query(_ context.Context, query string, args ...any) (pgx.Rows, error) {
	if db.queryCapture != nil {
		db.queryCapture.query = query
		db.queryCapture.args = append([]any(nil), args...)
	}
	return fakeRows{err: db.err}, db.err
}

func (db fakeDBTX) QueryRow(context.Context, string, ...any) pgx.Row {
	return fakeRow{err: db.err}
}

type fakeRow struct{ err error }

func (row fakeRow) Scan(dest ...any) error {
	if row.err != nil {
		return row.err
	}
	for _, value := range dest {
		field := reflect.ValueOf(value)
		field.Elem().Set(reflect.Zero(field.Elem().Type()))
	}
	return nil
}

type fakeRows struct{ err error }

func (fakeRows) Close()                                       {}
func (rows fakeRows) Err() error                              { return rows.err }
func (fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (fakeRows) Next() bool                                   { return false }
func (fakeRows) Scan(...any) error                            { return nil }
func (fakeRows) Values() ([]any, error)                       { return nil, nil }
func (fakeRows) RawValues() [][]byte                          { return nil }
func (fakeRows) Conn() *pgx.Conn                              { return nil }
func (fakeRows) TypeMap() *pgtype.Map                         { return nil }

type fakeTx struct {
	pgx.Tx
	commitErr  error
	committed  bool
	rolledBack bool
}

func (tx *fakeTx) Commit(context.Context) error {
	tx.committed = true
	return tx.commitErr
}

func (tx *fakeTx) Rollback(context.Context) error {
	tx.rolledBack = true
	return nil
}

func TestWithTx(t *testing.T) {
	ctx := context.Background()
	t.Run("commit", func(t *testing.T) {
		fake := &fakeTx{}
		err := withTx(ctx, func(context.Context) (pgx.Tx, error) { return fake, nil }, func(tx pgx.Tx) error {
			if tx != fake {
				t.Fatal("transaction callback received a different transaction")
			}
			return nil
		})
		if err != nil || !fake.committed || !fake.rolledBack {
			t.Fatalf("transaction result err=%v committed=%t rollback=%t", err, fake.committed, fake.rolledBack)
		}
	})
	t.Run("callback error rolls back", func(t *testing.T) {
		fake := &fakeTx{}
		callbackErr := errors.New("callback failed")
		err := withTx(ctx, func(context.Context) (pgx.Tx, error) { return fake, nil }, func(pgx.Tx) error {
			return callbackErr
		})
		if !errors.Is(err, callbackErr) || fake.committed || !fake.rolledBack {
			t.Fatalf("transaction result err=%v committed=%t rollback=%t", err, fake.committed, fake.rolledBack)
		}
	})
	t.Run("begin error", func(t *testing.T) {
		beginErr := errors.New("begin failed")
		err := withTx(ctx, func(context.Context) (pgx.Tx, error) { return nil, beginErr }, func(pgx.Tx) error { return nil })
		if !errors.Is(err, beginErr) {
			t.Fatalf("begin error was not wrapped: %v", err)
		}
	})
	t.Run("commit error", func(t *testing.T) {
		fake := &fakeTx{commitErr: errors.New("commit failed")}
		err := withTx(ctx, func(context.Context) (pgx.Tx, error) { return fake, nil }, func(pgx.Tx) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "commit transaction") || !fake.rolledBack {
			t.Fatalf("commit error result err=%v rollback=%t", err, fake.rolledBack)
		}
	})
}

func TestStoreQueriesAndConversions(t *testing.T) {
	ctx := context.Background()
	s := &Store{q: sqlcgen.New(fakeDBTX{rowsAffected: 1})}

	if _, err := s.GetHost(ctx, "h1234567"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ListHostsByOwner(ctx, "u1234567"); err != nil || got == nil {
		t.Fatalf("empty host list = %#v, %v", got, err)
	}
	if _, err := s.GetWorkspace(ctx, "w1234567"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ListWorkspacesByOwner(ctx, "u1234567"); err != nil || got == nil {
		t.Fatalf("empty workspace list = %#v, %v", got, err)
	}
	if _, _, err := s.CreateWorkspace(ctx, workspace.Workspace{}, app.IdempotencyRecord{}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("invalid workspace creation error = %v", err)
	}
	if _, err := s.GetUserByID(ctx, "u1234567"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetUserByName(ctx, "operator"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ListUsers(ctx); err != nil || got == nil {
		t.Fatalf("empty user list = %#v, %v", got, err)
	}
	user := identity.User{
		ID: "u1234567", Name: "new-user", PasswordHash: "hash", Role: identity.RoleUser,
		Status: identity.StatusPending,
	}
	if _, err := s.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTokenByID(ctx, "t1234567"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ListTokensByOwner(ctx, "u1234567"); err != nil || got == nil {
		t.Fatalf("empty token list = %#v, %v", got, err)
	}
	if _, err := s.CreateToken(ctx, identity.Token{ID: "t1234567", OwnerID: "u1234567"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeToken(ctx, "t1234567", "u1234567"); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAudit(ctx, audit.Entry{Action: audit.ActionHostCreate}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ListAuditLogs(ctx, app.AuditFilter{Limit: 10}); err != nil || got == nil {
		t.Fatalf("empty audit list = %#v, %v", got, err)
	}

	if err := (&Store{}).WriteAudit(ctx, audit.Entry{Detail: map[string]any{"bad": make(chan int)}}); err == nil {
		t.Fatal("audit JSON errors must be returned before database access")
	}
}

func TestListAuditLogsPassesFiltersAndSortsNewestFirst(t *testing.T) {
	ctx := context.Background()
	since := time.Date(2026, 10, 20, 1, 2, 3, 0, time.UTC)
	capture := &queryCapture{}
	s := &Store{q: sqlcgen.New(fakeDBTX{queryCapture: capture})}
	_, err := s.ListAuditLogs(ctx, app.AuditFilter{
		Actor: "u1234567", Action: "host.create", Target: "h1234567", Since: &since, Limit: 25,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(capture.args) != 5 || capture.args[0] != "u1234567" || capture.args[1] != "host.create" || capture.args[2] != "h1234567" || capture.args[4] != int32(25) {
		t.Fatalf("audit query args = %#v", capture.args)
	}
	gotSince, ok := capture.args[3].(pgtype.Timestamptz)
	if !ok || !gotSince.Valid || !gotSince.Time.Equal(since) {
		t.Fatalf("audit since arg = %#v", capture.args[3])
	}
	if !strings.Contains(capture.query, "ORDER BY at DESC, id DESC") || !strings.Contains(capture.query, "at >= $4") {
		t.Fatalf("audit query does not filter and sort as required: %s", capture.query)
	}
}

func TestStoreDatabaseErrors(t *testing.T) {
	ctx := context.Background()
	s := &Store{q: sqlcgen.New(fakeDBTX{err: errors.New("database unavailable")})}
	checks := []struct {
		name string
		call func() error
	}{
		{"get host", func() error { _, err := s.GetHost(ctx, "h1234567"); return err }},
		{"list hosts", func() error { _, err := s.ListHostsByOwner(ctx, "u1234567"); return err }},
		{"get workspace", func() error { _, err := s.GetWorkspace(ctx, "w1234567"); return err }},
		{"list workspaces", func() error { _, err := s.ListWorkspacesByOwner(ctx, "u1234567"); return err }},
		{"get user id", func() error { _, err := s.GetUserByID(ctx, "u1234567"); return err }},
		{"get user name", func() error { _, err := s.GetUserByName(ctx, "operator"); return err }},
		{"list users", func() error { _, err := s.ListUsers(ctx); return err }},
		{"create user", func() error { _, err := s.CreateUser(ctx, identity.User{}); return err }},
		{"get token", func() error { _, err := s.GetTokenByID(ctx, "t1234567"); return err }},
		{"list tokens", func() error { _, err := s.ListTokensByOwner(ctx, "u1234567"); return err }},
		{"create token", func() error { _, err := s.CreateToken(ctx, identity.Token{}); return err }},
		{"revoke token", func() error { return s.RevokeToken(ctx, "t1234567", "u1234567") }},
		{"write audit", func() error { return s.WriteAudit(ctx, audit.Entry{}) }},
		{"list audit", func() error { _, err := s.ListAuditLogs(ctx, app.AuditFilter{Limit: 10}); return err }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.call(); err == nil {
				t.Fatal("expected database error")
			}
		})
	}

	if err := (&Store{q: sqlcgen.New(fakeDBTX{rowsAffected: 0})}).RevokeToken(ctx, "t1234567", "u1234567"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("missing token error = %v", err)
	}
}
