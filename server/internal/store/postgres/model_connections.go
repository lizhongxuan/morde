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
		ModelName: value.ModelName, ContextWindowTokens: nullableInt4(value.ContextWindowTokens),
		ContextWindowOverrideTokens: nullableInt4(value.ContextWindowOverrideTokens), ContextWindowSource: string(modelContextWindowSource(value.ContextWindowSource)),
		ContextWindowResolvedAt: nullableTimestamp(value.ContextWindowResolvedAt), ContextWindowCatalogVersion: nullableTextPointer(value.ContextWindowCatalogVersion),
		ApiKeyCipher: value.APIKeyCipher, MaxOutputTokens: nullableInt4(value.MaxOutputTokens),
		MaxOutputOverrideTokens: nullableInt4(value.MaxOutputOverrideTokens), MaxOutputSource: string(modelMaxOutputSource(value.MaxOutputSource)),
		MaxOutputResolvedAt: nullableTimestamp(value.MaxOutputResolvedAt), MaxOutputCatalogVersion: nullableTextPointer(value.MaxOutputCatalogVersion),
		MaxOutputSourceUrl: nullableTextPointer(value.MaxOutputSourceURL),
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
	result := mapRows(rows, modelConnectionDomain)
	s.addModelTextStreamingCapabilities(ctx, result)
	return result, nil
}

func (s *Store) GetModelConnection(ctx context.Context, id string) (app.ModelConnectionRecord, error) {
	row, err := s.q.GetModelConnection(ctx, id)
	if err != nil {
		return app.ModelConnectionRecord{}, storeError("get model connection", "model connection", err)
	}
	result := []app.ModelConnectionRecord{modelConnectionDomain(row)}
	s.addModelTextStreamingCapabilities(ctx, result)
	return result[0], nil
}

// Streaming verification is optional. A lookup failure leaves it unknown and
// must not turn an otherwise configured text model into an unavailable one.
func (s *Store) addModelTextStreamingCapabilities(ctx context.Context, records []app.ModelConnectionRecord) {
	if len(records) == 0 {
		return
	}
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.ID)
	}
	rows, err := s.pool.Query(ctx, `SELECT model_id,connection_version FROM model_text_streaming_capabilities
WHERE model_id=ANY($1::text[]) AND state='verified'`, ids)
	if err != nil {
		return
	}
	defer rows.Close()
	versions := make(map[string]pgtype.Timestamptz)
	for rows.Next() {
		var id string
		var version pgtype.Timestamptz
		if err := rows.Scan(&id, &version); err != nil {
			return
		}
		versions[id] = version
	}
	if rows.Err() != nil {
		return
	}
	for i := range records {
		version, ok := versions[records[i].ID]
		records[i].Capabilities.TextStream = ok && version.Valid && records[i].CheckedAt != nil && records[i].Capabilities.Text &&
			version.Time.Equal(records[i].UpdatedAt)
	}
}

func (s *Store) UpdateModelConnection(ctx context.Context, value app.ModelConnectionRecord) (app.ModelConnectionRecord, error) {
	row, err := s.q.UpdateModelConnection(ctx, sqlcgen.UpdateModelConnectionParams{
		ID: value.ID, Name: value.Name, BaseUrl: value.BaseURL, ModelName: value.ModelName,
		ContextWindowTokens:             nullableInt4(value.ContextWindowTokens),
		ContextWindowOverrideTokens:     nullableInt4(value.ContextWindowOverrideTokens),
		ContextWindowSource:             string(modelContextWindowSource(value.ContextWindowSource)),
		ContextWindowResolvedAt:         nullableTimestamp(value.ContextWindowResolvedAt),
		ContextWindowCatalogVersion:     nullableTextPointer(value.ContextWindowCatalogVersion),
		MaxOutputTokens:                 nullableInt4(value.MaxOutputTokens),
		MaxOutputOverrideTokens:         nullableInt4(value.MaxOutputOverrideTokens),
		MaxOutputSource:                 string(modelMaxOutputSource(value.MaxOutputSource)),
		MaxOutputResolvedAt:             nullableTimestamp(value.MaxOutputResolvedAt),
		MaxOutputCatalogVersion:         nullableTextPointer(value.MaxOutputCatalogVersion),
		MaxOutputSourceUrl:              nullableTextPointer(value.MaxOutputSourceURL),
		ApiKeyCipher:                    value.APIKeyCipher,
		CapabilityText:                  value.Capabilities.Text,
		CapabilityTools:                 value.Capabilities.Tools,
		CapabilityVision:                value.Capabilities.Vision,
		CapabilityMediaImageGeneration:  pgtype.Bool{Bool: value.Capabilities.MediaGeneration.ImageGeneration, Valid: true},
		CapabilityMediaVideoGeneration:  pgtype.Bool{Bool: value.Capabilities.MediaGeneration.VideoGeneration, Valid: true},
		CapabilityMediaSpeechGeneration: pgtype.Bool{Bool: value.Capabilities.MediaGeneration.SpeechGeneration, Valid: true},
		CheckedAt:                       nullableTimestamp(value.CheckedAt), ExpectedUpdatedAt: timestamp(value.UpdatedAt),
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
		MaxOutputTokens: intPointer(row.MaxOutputTokens), MaxOutputOverrideTokens: intPointer(row.MaxOutputOverrideTokens),
		MaxOutputSource: app.ModelMaxOutputSource(row.MaxOutputSource), MaxOutputResolvedAt: timePointer(row.MaxOutputResolvedAt),
		MaxOutputCatalogVersion: textPointer(row.MaxOutputCatalogVersion), MaxOutputSourceURL: textPointer(row.MaxOutputSourceUrl),
		ContextWindowTokens: intPointer(row.ContextWindowTokens), ContextWindowOverrideTokens: intPointer(row.ContextWindowOverrideTokens),
		ContextWindowSource: app.ModelContextWindowSource(row.ContextWindowSource), ContextWindowResolvedAt: timePointer(row.ContextWindowResolvedAt),
		ContextWindowCatalogVersion: textPointer(row.ContextWindowCatalogVersion), APIKeyCipher: append([]byte(nil), row.ApiKeyCipher...),
		Capabilities: app.ModelCapabilities{
			Text: row.CapabilityText, Tools: row.CapabilityTools, Vision: row.CapabilityVision,
			MediaGeneration: app.ModelMediaGenerationCapabilities{
				ImageGeneration: row.CapabilityMediaImageGeneration, VideoGeneration: row.CapabilityMediaVideoGeneration,
				SpeechGeneration: row.CapabilityMediaSpeechGeneration,
			},
			ModelID: row.ID,
		},
		CheckedAt: timePointer(row.CheckedAt), CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
	}
}

func modelContextWindowSource(value app.ModelContextWindowSource) app.ModelContextWindowSource {
	if value == "" {
		return app.ModelContextWindowUnknown
	}
	return value
}

func modelMaxOutputSource(value app.ModelMaxOutputSource) app.ModelMaxOutputSource {
	if value == "" {
		return app.ModelMaxOutputUnknown
	}
	return value
}

func nullableTextPointer(value *string) pgtype.Text {
	if value == nil || *value == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *value, Valid: true}
}

func textPointer(value pgtype.Text) *string {
	if !value.Valid {
		return nil
	}
	result := value.String
	return &result
}

var _ app.ModelConnectionStore = (*Store)(nil)
