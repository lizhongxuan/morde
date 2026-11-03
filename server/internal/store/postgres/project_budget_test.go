package postgres

import (
	"context"
	"reflect"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
)

func TestProjectBudgetLedgerCursorIsStableAndProjectScoped(t *testing.T) {
	createdAt := time.Date(2026, 11, 2, 3, 4, 5, 123456000, time.UTC)
	want := projectBudgetLedgerCursor{
		ProjectID: "j7654321", CreatedAt: createdAt, Source: string(app.ModelCallChat), CallID: "chat-call-001",
	}
	encoded, err := encodeProjectBudgetLedgerCursor(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeProjectBudgetLedgerCursor(encoded, want.ProjectID)
	if err != nil || got == nil || *got != want {
		t.Fatalf("cursor round trip = %#v, %v; want %#v", got, err, want)
	}
	if _, err := decodeProjectBudgetLedgerCursor(encoded, "j0000000"); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("cross-project cursor error = %v, want invalid argument", err)
	}
	if _, err := decodeProjectBudgetLedgerCursor("!invalid!", want.ProjectID); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("malformed cursor error = %v, want invalid argument", err)
	}
	if _, err := encodeProjectBudgetLedgerCursor(projectBudgetLedgerCursor{
		ProjectID: want.ProjectID, CreatedAt: createdAt, Source: "probe", CallID: want.CallID,
	}); err == nil {
		t.Fatal("cursor encoded a source that is not stored in project ledger")
	}
}

func TestProjectBudgetTransitionProtectsCommittedUsageAndCurrency(t *testing.T) {
	current := project.Budget{Currency: "USD", LimitMicros: 10_000, Version: 1}
	tests := []struct {
		name   string
		next   project.Budget
		totals projectBudgetTotals
		want   domain.ErrorCode
	}{
		{
			name:   "spent and reserved fit",
			next:   project.Budget{Currency: "USD", LimitMicros: 3_000, Version: 2},
			totals: projectBudgetTotals{SpentMicros: 2_000, ReservedMicros: 1_000, LedgerCount: 2, LedgerCurrency: "USD"},
		},
		{
			name:   "limit below committed and reserved usage",
			next:   project.Budget{Currency: "USD", LimitMicros: 2_999, Version: 2},
			totals: projectBudgetTotals{SpentMicros: 2_000, ReservedMicros: 1_000, LedgerCount: 2, LedgerCurrency: "USD"},
			want:   domain.CodeConflict,
		},
		{
			name:   "currency frozen by ledger",
			next:   project.Budget{Currency: "EUR", LimitMicros: 10_000, Version: 2},
			totals: projectBudgetTotals{LedgerCount: 1, LedgerCurrency: "USD"},
			want:   domain.CodeConflict,
		},
		{
			name:   "unknown usage blocks budget changes",
			next:   project.Budget{Currency: "USD", LimitMicros: 20_000, Version: 2},
			totals: projectBudgetTotals{HasUnknown: true, LedgerCount: 1, LedgerCurrency: "USD"},
			want:   domain.CodeInvalidState,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateProjectBudgetTransition(current, true, test.next, test.totals)
			if test.want == "" && err == nil {
				return
			}
			if got := domain.CodeOf(err); got != test.want {
				t.Fatalf("transition error = %v, want %s", err, test.want)
			}
		})
	}
	if err := validateProjectBudgetTransition(current, true,
		project.Budget{Currency: "USD", LimitMicros: 1_000, Version: 2},
		projectBudgetTotals{SpentMicros: 2_000, LedgerCount: 1, LedgerCurrency: "USD"}); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("limit below spent usage error = %v, want conflict", err)
	}
}

func TestProjectModelReservationRequiresAtomicAccountCall(t *testing.T) {
	store := &Store{}
	reservation, replay, err := store.ReserveProjectModelCall(context.Background(), app.ProjectModelReservation{
		ProjectID: "j7654321", ActorID: "u1234567", Source: app.ModelCallClient, CallID: "client-call-01",
	})
	if domain.CodeOf(err) != domain.CodeInvalidArgument || replay || !reflect.DeepEqual(reservation, app.ProjectModelReservation{}) {
		t.Fatalf("incomplete reservation = %#v replay=%t err=%v", reservation, replay, err)
	}
	if err := store.SettleProjectModelCall(context.Background(), app.SettleProjectModelCallCommand{
		ProjectID: "j7654321", Source: app.ModelCallClient,
	}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("incomplete settlement error = %v", err)
	}
}
