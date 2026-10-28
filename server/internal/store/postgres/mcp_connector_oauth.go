package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func (s *Store) CreateMCPConnectorOAuthFlow(ctx context.Context, flow app.MCPConnectorOAuthFlow) error {
	if flow.ID == "" || len(flow.ID) > 64 || !domain.ValidUserID(flow.OwnerID) || !domain.ValidMCPConnectorID(flow.ConnectorID) ||
		flow.ConnectorRevision < 1 || len(flow.StateHash) != 32 || len(flow.PayloadCipher) < 29 || len(flow.PayloadCipher) > 65536 ||
		!flow.ExpiresAt.After(time.Now()) || flow.ExpiresAt.After(time.Now().Add(11*time.Minute)) {
		return domain.InvalidArgument("oauth_flow", "MCP OAuth flow is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var revision int64
		if err := tx.QueryRow(ctx, `SELECT revision FROM mcp_connectors WHERE owner_id=$1 AND id=$2 FOR SHARE`, flow.OwnerID, flow.ConnectorID).Scan(&revision); err != nil {
			return storeError("create MCP OAuth flow", "MCP connector", err)
		}
		if revision != flow.ConnectorRevision {
			return domain.NewError(domain.CodeConflict, "MCP connector changed before OAuth authorization")
		}
		if _, err := tx.Exec(ctx, `DELETE FROM mcp_connector_oauth_flows WHERE expires_at <= clock_timestamp()`); err != nil {
			return fmt.Errorf("remove expired MCP OAuth flows: %w", err)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO mcp_connector_oauth_flows (id, owner_id, connector_id, connector_revision, state_hash, payload_cipher, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
		`, flow.ID, flow.OwnerID, flow.ConnectorID, flow.ConnectorRevision, flow.StateHash, flow.PayloadCipher, flow.ExpiresAt)
		if err != nil {
			return fmt.Errorf("create MCP OAuth flow: %w", err)
		}
		return nil
	})
}

func (s *Store) ConsumeMCPConnectorOAuthFlow(ctx context.Context, stateHash []byte) (app.MCPConnectorOAuthFlow, error) {
	if len(stateHash) != 32 {
		return app.MCPConnectorOAuthFlow{}, domain.InvalidArgument("state", "MCP OAuth state is invalid")
	}
	var flow app.MCPConnectorOAuthFlow
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			DELETE FROM mcp_connector_oauth_flows
			WHERE state_hash=$1 AND expires_at > clock_timestamp()
			RETURNING id, owner_id, connector_id, connector_revision, state_hash, payload_cipher, expires_at
		`, stateHash).Scan(&flow.ID, &flow.OwnerID, &flow.ConnectorID, &flow.ConnectorRevision,
			&flow.StateHash, &flow.PayloadCipher, &flow.ExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "MCP OAuth authorization expired or was already used")
		}
		if err != nil {
			return fmt.Errorf("consume MCP OAuth flow: %w", err)
		}
		return nil
	})
	if err != nil {
		return app.MCPConnectorOAuthFlow{}, err
	}
	return flow, nil
}

func (s *Store) ReplaceMCPConnectorOAuthCredential(ctx context.Context, ownerID, connectorID string, credentialVersion int64, expectedCipher, nextCipher []byte) (bool, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidMCPConnectorID(connectorID) || credentialVersion < 1 ||
		len(expectedCipher) < 29 || len(expectedCipher) > 32796 || len(nextCipher) < 29 || len(nextCipher) > 32796 {
		return false, domain.InvalidArgument("credential", "MCP OAuth credential refresh is invalid")
	}
	var updated bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE mcp_connectors SET credential_cipher=$1, updated_at=clock_timestamp()
			WHERE owner_id=$2 AND id=$3 AND credential_version=$4 AND credential_cipher=$5
		`, nextCipher, ownerID, connectorID, credentialVersion, expectedCipher)
		if err != nil {
			return fmt.Errorf("persist refreshed MCP OAuth credential: %w", err)
		}
		updated = tag.RowsAffected() == 1
		return nil
	})
	return updated, err
}

var _ app.MCPConnectorOAuthStore = (*Store)(nil)
