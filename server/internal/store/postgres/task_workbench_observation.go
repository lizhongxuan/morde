package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) CreateTaskWorkbenchObservationTicket(ctx context.Context, value app.TaskWorkbenchObservationRecord) error {
	if !validObservationPersistenceRecord(value, false) {
		return domain.InvalidArgument("ticket", "Task observation ticket is invalid")
	}
	if err := s.q.PruneExpiredTaskWorkbenchObservationTickets(ctx); err != nil {
		return fmt.Errorf("prune expired Task observation tickets: %w", err)
	}
	err := s.q.CreateTaskWorkbenchObservationTicket(ctx, sqlcgen.CreateTaskWorkbenchObservationTicketParams{
		TicketHash: value.TicketHash, NonceHash: value.Nonce, TaskID: value.TaskID, HostID: value.HostID,
		ActorUserID: value.ActorID, SessionTokenID: value.SessionTokenID, RuntimeGeneration: value.RuntimeGeneration,
		ReadScope: value.Scope, ExpiresAt: timestamp(value.ExpiresAt),
	})
	if err != nil {
		return fmt.Errorf("create Task observation ticket: %w", err)
	}
	return nil
}

func (s *Store) GetTaskWorkbenchObservationTicket(ctx context.Context, ticketHash []byte) (app.TaskWorkbenchObservationRecord, error) {
	if len(ticketHash) != 32 {
		return app.TaskWorkbenchObservationRecord{}, domain.NewError(domain.CodeNotFound, "Task observation ticket not found")
	}
	row, err := s.q.GetTaskWorkbenchObservationTicket(ctx, ticketHash)
	if err != nil {
		return app.TaskWorkbenchObservationRecord{}, observationStoreNotFound(err)
	}
	return observationRecordFromSQL(row), nil
}

func (s *Store) ConsumeTaskWorkbenchObservationTicket(ctx context.Context, ticketHash, sessionHash []byte, generation int64, sessionExpiresAt time.Time) (app.TaskWorkbenchObservationRecord, error) {
	if len(ticketHash) != 32 || len(sessionHash) != 32 || generation < 1 || !sessionExpiresAt.After(time.Now()) {
		return app.TaskWorkbenchObservationRecord{}, domain.InvalidArgument("ticket", "Task observation ticket exchange is invalid")
	}
	row, err := s.q.ConsumeTaskWorkbenchObservationTicket(ctx, sqlcgen.ConsumeTaskWorkbenchObservationTicketParams{
		TicketHash: ticketHash, SessionHash: sessionHash, RuntimeGeneration: generation,
		SessionExpiresAt: timestamp(sessionExpiresAt),
	})
	if err != nil {
		return app.TaskWorkbenchObservationRecord{}, observationStoreNotFound(err)
	}
	return observationRecordFromSQL(row), nil
}

func (s *Store) GetTaskWorkbenchObservationSession(ctx context.Context, sessionHash []byte, now time.Time) (app.TaskWorkbenchObservationRecord, error) {
	if len(sessionHash) != 32 || now.IsZero() {
		return app.TaskWorkbenchObservationRecord{}, domain.NewError(domain.CodeNotFound, "Task observation session not found")
	}
	row, err := s.q.GetTaskWorkbenchObservationSession(ctx, sqlcgen.GetTaskWorkbenchObservationSessionParams{
		SessionHash: sessionHash, SessionExpiresAt: timestamp(now),
	})
	if err != nil {
		return app.TaskWorkbenchObservationRecord{}, observationStoreNotFound(err)
	}
	return observationRecordFromSQL(row), nil
}

func observationRecordFromSQL(row sqlcgen.TaskWorkbenchObservationTicket) app.TaskWorkbenchObservationRecord {
	return app.TaskWorkbenchObservationRecord{
		TicketHash: row.TicketHash, Nonce: row.NonceHash, SessionHash: row.SessionHash,
		TaskID: row.TaskID, HostID: row.HostID, ActorID: row.ActorUserID, SessionTokenID: row.SessionTokenID,
		RuntimeGeneration: row.RuntimeGeneration, Scope: row.ReadScope, ExpiresAt: row.ExpiresAt.Time.UTC(),
		UsedAt: timePointer(row.UsedAt), SessionExpiresAt: row.SessionExpiresAt.Time.UTC(),
	}
}

func observationStoreNotFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "Task observation credential not found")
	}
	return fmt.Errorf("read Task observation credential: %w", err)
}

func validObservationPersistenceRecord(value app.TaskWorkbenchObservationRecord, consumed bool) bool {
	if len(value.TicketHash) != 32 || len(value.Nonce) != 32 || !domain.ValidTaskID(value.TaskID) || !domain.ValidHostID(value.HostID) ||
		!domain.ValidUserID(value.ActorID) || !domain.ValidTokenID(value.SessionTokenID) || value.RuntimeGeneration < 1 || value.Scope != "task:read" || value.ExpiresAt.IsZero() {
		return false
	}
	if consumed {
		return len(value.SessionHash) == 32 && value.UsedAt != nil && !value.SessionExpiresAt.IsZero()
	}
	return len(value.SessionHash) == 0 && value.UsedAt == nil && value.SessionExpiresAt.IsZero()
}
