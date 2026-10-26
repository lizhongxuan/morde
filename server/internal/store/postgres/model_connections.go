package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) CreateModelConnection(ctx context.Context, value app.ModelConnectionRecord) (app.ModelConnectionRecord, error) {
	row, err := s.q.CreateModelConnection(ctx, sqlcgen.CreateModelConnectionParams{
		ID: value.ID, OwnerID: nullableText(value.OwnerID), Name: value.Name, BaseUrl: value.BaseURL,
		ModelName: value.ModelName, ApiKeyCipher: value.APIKeyCipher,
	})
	if err != nil {
		var postgresErr *pgconn.PgError
		if errors.As(err, &postgresErr) && postgresErr.Code == "23505" {
			return app.ModelConnectionRecord{}, domain.NewError(domain.CodeConflict, "model connection name already exists")
		}
		return app.ModelConnectionRecord{}, fmt.Errorf("create model connection: %w", err)
	}
	return modelConnectionDomain(row), nil
}

func (s *Store) ListModelConnections(ctx context.Context, ownerID string) ([]app.ModelConnectionRecord, error) {
	rows, err := s.q.ListModelConnections(ctx, pgtype.Text{String: ownerID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("list model connections: %w", err)
	}
	return mapRows(rows, modelConnectionDomain), nil
}

func (s *Store) GetModelConnection(ctx context.Context, id string) (app.ModelConnectionRecord, error) {
	row, err := s.q.GetModelConnection(ctx, id)
	if err != nil {
		return app.ModelConnectionRecord{}, storeError("get model connection", "model connection", err)
	}
	return modelConnectionDomain(row), nil
}

func (s *Store) UpdateModelConnection(ctx context.Context, value app.ModelConnectionRecord) (app.ModelConnectionRecord, error) {
	row, err := s.q.UpdateModelConnection(ctx, sqlcgen.UpdateModelConnectionParams{
		ID: value.ID, Name: value.Name, BaseUrl: value.BaseURL, ModelName: value.ModelName,
		ApiKeyCipher: value.APIKeyCipher, CapabilityText: value.Capabilities.Text, CapabilityTools: value.Capabilities.Tools,
		CapabilityVision: value.Capabilities.Vision, CheckedAt: nullableTimestamp(value.CheckedAt), ExpectedUpdatedAt: timestamp(value.UpdatedAt),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if _, lookupErr := s.q.GetModelConnection(ctx, value.ID); lookupErr == nil {
				return app.ModelConnectionRecord{}, domain.NewError(domain.CodeConflict, "model connection changed during update")
			}
		}
		return app.ModelConnectionRecord{}, storeError("update model connection", "model connection", err)
	}
	return modelConnectionDomain(row), nil
}

func (s *Store) DeleteModelConnection(ctx context.Context, id string) error {
	if _, err := s.q.DeleteModelConnection(ctx, id); err != nil {
		return storeError("delete model connection", "model connection", err)
	}
	return nil
}

func modelConnectionDomain(row sqlcgen.ModelConnection) app.ModelConnectionRecord {
	return app.ModelConnectionRecord{
		ID: row.ID, OwnerID: row.OwnerID.String, Name: row.Name, BaseURL: row.BaseUrl, ModelName: row.ModelName,
		APIKeyCipher: append([]byte(nil), row.ApiKeyCipher...),
		Capabilities: app.ModelCapabilities{Text: row.CapabilityText, Tools: row.CapabilityTools, Vision: row.CapabilityVision, ModelID: row.ID},
		CheckedAt:    timePointer(row.CheckedAt), CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
	}
}

var _ app.ModelConnectionStore = (*Store)(nil)
