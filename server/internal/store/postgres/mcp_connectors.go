package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) CreateMCPConnector(ctx context.Context, value app.MCPConnectorRecord, entry audit.Entry) (app.MCPConnectorRecord, error) {
	if !domain.ValidMCPConnectorID(value.ID) || !domain.ValidUserID(value.OwnerID) || value.Name == "" || value.EndpointURL == "" ||
		value.Revision != 0 || value.Enabled || len(value.CredentialCipher) == 0 && value.CredentialVersion != 0 ||
		len(value.CredentialCipher) > 0 && (len(value.CredentialCipher) < 29 || value.CredentialVersion < 1 || len(value.CredentialCipher) > 32796) {
		return app.MCPConnectorRecord{}, domain.InvalidArgument("connector", "MCP connector fields are invalid")
	}
	auditEntry, err := mcpConnectorAuditParams(value.OwnerID, value.ID, entry)
	if err != nil {
		return app.MCPConnectorRecord{}, err
	}
	var created app.MCPConnectorRecord
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		queries := sqlcgen.New(tx)
		row, err := queries.CreateMCPConnector(ctx, sqlcgen.CreateMCPConnectorParams{
			ID: value.ID, OwnerID: value.OwnerID, Name: value.Name, EndpointUrl: value.EndpointURL,
			CredentialCipher: value.CredentialCipher, CredentialVersion: value.CredentialVersion,
		})
		if err != nil {
			return fmt.Errorf("create MCP connector: %w", err)
		}
		if _, err := queries.CreateAuditLog(ctx, auditEntry); err != nil {
			return fmt.Errorf("write MCP connector audit: %w", err)
		}
		created = mcpConnectorDomain(row)
		return nil
	})
	if err != nil {
		var postgresErr *pgconn.PgError
		if errors.As(err, &postgresErr) && postgresErr.Code == "23505" {
			return app.MCPConnectorRecord{}, domain.NewError(domain.CodeConflict, "MCP connector name already exists")
		}
		return app.MCPConnectorRecord{}, err
	}
	return created, nil
}

func (s *Store) ListMCPConnectors(ctx context.Context, ownerID string) ([]app.MCPConnectorRecord, error) {
	rows, err := s.q.ListMCPConnectors(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list MCP connectors: %w", err)
	}
	return mapRows(rows, mcpConnectorDomain), nil
}

func (s *Store) GetMCPConnector(ctx context.Context, ownerID, id string) (app.MCPConnectorRecord, error) {
	row, err := s.q.GetMCPConnector(ctx, sqlcgen.GetMCPConnectorParams{OwnerID: ownerID, ID: id})
	if err != nil {
		return app.MCPConnectorRecord{}, storeError("get MCP connector", "MCP connector", err)
	}
	return mcpConnectorDomain(row), nil
}

func (s *Store) UpdateMCPConnector(ctx context.Context, ownerID string, value app.MCPConnectorRecord, expectedRevision int64, entry audit.Entry) (app.MCPConnectorRecord, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidMCPConnectorID(value.ID) || ownerID != value.OwnerID || expectedRevision < 1 || value.Name == "" || value.EndpointURL == "" ||
		len(value.CredentialCipher) > 0 && (len(value.CredentialCipher) < 29 || value.CredentialVersion < 1 || len(value.CredentialCipher) > 32796) {
		return app.MCPConnectorRecord{}, domain.InvalidArgument("connector", "MCP connector update fields are invalid")
	}
	auditEntry, err := mcpConnectorAuditParams(ownerID, value.ID, entry)
	if err != nil {
		return app.MCPConnectorRecord{}, err
	}
	var updated app.MCPConnectorRecord
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		queries := sqlcgen.New(tx)
		row, err := queries.UpdateMCPConnector(ctx, sqlcgen.UpdateMCPConnectorParams{
			OwnerID: ownerID, ID: value.ID, Name: value.Name, EndpointUrl: value.EndpointURL, Enabled: value.Enabled,
			CredentialCipher: value.CredentialCipher, CredentialVersion: value.CredentialVersion, ExpectedRevision: expectedRevision,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				if _, lookupErr := queries.GetMCPConnector(ctx, sqlcgen.GetMCPConnectorParams{OwnerID: ownerID, ID: value.ID}); lookupErr == nil {
					return domain.NewError(domain.CodeConflict, "MCP connector changed during update")
				} else if errors.Is(lookupErr, pgx.ErrNoRows) {
					return domain.NewError(domain.CodeNotFound, "MCP connector not found")
				} else {
					return fmt.Errorf("check MCP connector update: %w", lookupErr)
				}
			}
			return fmt.Errorf("update MCP connector: %w", err)
		}
		if err := fenceMCPConnectorOperations(ctx, tx, ownerID, value.ID); err != nil {
			return err
		}
		if _, err := queries.CreateAuditLog(ctx, auditEntry); err != nil {
			return fmt.Errorf("write MCP connector audit: %w", err)
		}
		updated = mcpConnectorDomain(row)
		return nil
	})
	if err != nil {
		var postgresErr *pgconn.PgError
		if errors.As(err, &postgresErr) && postgresErr.Code == "23505" {
			return app.MCPConnectorRecord{}, domain.NewError(domain.CodeConflict, "MCP connector name already exists")
		}
		return app.MCPConnectorRecord{}, err
	}
	return updated, nil
}

func (s *Store) DeleteMCPConnector(ctx context.Context, ownerID, id string, expectedRevision int64, entry audit.Entry) error {
	if !domain.ValidUserID(ownerID) || !domain.ValidMCPConnectorID(id) || expectedRevision < 1 {
		return domain.InvalidArgument("connector", "MCP connector deletion fields are invalid")
	}
	auditEntry, err := mcpConnectorAuditParams(ownerID, id, entry)
	if err != nil {
		return err
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		queries := sqlcgen.New(tx)
		if _, err := queries.DeleteMCPConnector(ctx, sqlcgen.DeleteMCPConnectorParams{OwnerID: ownerID, ID: id, ExpectedRevision: expectedRevision}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				if _, lookupErr := queries.GetMCPConnector(ctx, sqlcgen.GetMCPConnectorParams{OwnerID: ownerID, ID: id}); lookupErr == nil {
					return domain.NewError(domain.CodeConflict, "MCP connector changed before deletion")
				} else if errors.Is(lookupErr, pgx.ErrNoRows) {
					return domain.NewError(domain.CodeNotFound, "MCP connector not found")
				} else {
					return fmt.Errorf("check MCP connector deletion: %w", lookupErr)
				}
			}
			return storeError("delete MCP connector", "MCP connector", err)
		}
		if err := fenceMCPConnectorOperations(ctx, tx, ownerID, id); err != nil {
			return err
		}
		if _, err := queries.CreateAuditLog(ctx, auditEntry); err != nil {
			return fmt.Errorf("write MCP connector audit: %w", err)
		}
		return nil
	})
}

func fenceMCPConnectorOperations(ctx context.Context, tx pgx.Tx, ownerID, connectorID string) error {
	if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations
SET status='cancelled', result='connector changed before dispatch', updated_at=clock_timestamp()
WHERE owner_id=$1 AND connector_id=$2 AND status='prepared'`, ownerID, connectorID); err != nil {
		return fmt.Errorf("cancel prepared MCP operations after connector change: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations
SET cancel_requested=true, result='connector changed while call was in flight', updated_at=clock_timestamp()
WHERE owner_id=$1 AND connector_id=$2 AND status='dispatched'`, ownerID, connectorID); err != nil {
		return fmt.Errorf("request cancellation of in-flight MCP operations after connector change: %w", err)
	}
	return nil
}

func mcpConnectorAuditParams(ownerID, id string, entry audit.Entry) (sqlcgen.CreateAuditLogParams, error) {
	if !domain.ValidUserID(ownerID) || entry.ActorID != ownerID || entry.ActorKind != audit.ActorSession || entry.Target != id ||
		entry.Action != audit.ActionMCPConnectorCreate && entry.Action != audit.ActionMCPConnectorUpdate &&
			entry.Action != audit.ActionMCPConnectorEnable && entry.Action != audit.ActionMCPConnectorDisable &&
			entry.Action != audit.ActionMCPConnectorCredentialRotate && entry.Action != audit.ActionMCPConnectorDelete {
		return sqlcgen.CreateAuditLogParams{}, domain.InvalidArgument("audit", "MCP connector audit entry is invalid")
	}
	return auditParams(entry)
}

func mcpConnectorDomain(row sqlcgen.McpConnector) app.MCPConnectorRecord {
	return app.MCPConnectorRecord{
		ID: row.ID, OwnerID: row.OwnerID, Name: row.Name, EndpointURL: row.EndpointUrl, Enabled: row.Enabled,
		CredentialCipher: append([]byte(nil), row.CredentialCipher...), CredentialVersion: row.CredentialVersion,
		Revision: row.Revision, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
	}
}

var _ app.MCPConnectorStore = (*Store)(nil)
