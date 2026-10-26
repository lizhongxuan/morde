package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func (s *Store) GetUserPreferences(ctx context.Context, ownerID string) (app.UserPreferences, error) {
	if !domain.ValidUserID(ownerID) {
		return app.UserPreferences{}, domain.InvalidArgument("user_id", "user id is invalid")
	}
	var value app.UserPreferences
	err := s.pool.QueryRow(ctx, `
SELECT user_id, assistant_name, selected_model_id, default_host_id, last_host_issue, updated_at
FROM user_preferences
WHERE user_id = $1`, ownerID).Scan(
		&value.OwnerID, &value.AssistantName, &value.SelectedModelID, &value.DefaultHostID, &value.LastHostIssue, &value.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.UserPreferences{OwnerID: ownerID}, nil
	}
	if err != nil {
		return app.UserPreferences{}, fmt.Errorf("get user preferences: %w", err)
	}
	return value, nil
}

func (s *Store) SaveUserPreferences(ctx context.Context, ownerID, assistantName, selectedModelID string) (app.UserPreferences, error) {
	if !domain.ValidUserID(ownerID) || len(assistantName) > 80 || (selectedModelID != "" && !domain.ValidModelConnectionID(selectedModelID)) {
		return app.UserPreferences{}, domain.InvalidArgument("preferences", "user preferences are invalid")
	}
	var value app.UserPreferences
	err := s.pool.QueryRow(ctx, `
INSERT INTO user_preferences (user_id, assistant_name, selected_model_id)
VALUES ($1, $2, $3)
ON CONFLICT (user_id) DO UPDATE SET
  assistant_name = EXCLUDED.assistant_name,
  selected_model_id = EXCLUDED.selected_model_id,
  updated_at = clock_timestamp()
RETURNING user_id, assistant_name, selected_model_id, default_host_id, last_host_issue, updated_at`,
		ownerID, assistantName, selectedModelID,
	).Scan(&value.OwnerID, &value.AssistantName, &value.SelectedModelID, &value.DefaultHostID, &value.LastHostIssue, &value.UpdatedAt)
	if err != nil {
		return app.UserPreferences{}, fmt.Errorf("save user preferences: %w", err)
	}
	return value, nil
}

func (s *Store) SetDefaultHost(ctx context.Context, ownerID, hostID, issue string) (app.UserPreferences, error) {
	if !domain.ValidUserID(ownerID) || (hostID != "" && !domain.ValidHostID(hostID)) ||
		(issue != "" && issue != "capacity_unavailable" && issue != "quota_exceeded") {
		return app.UserPreferences{}, domain.InvalidArgument("default_host", "default host selection is invalid")
	}
	var value app.UserPreferences
	err := s.pool.QueryRow(ctx, `
INSERT INTO user_preferences (user_id, default_host_id, last_host_issue)
VALUES ($1, $2, $3)
ON CONFLICT (user_id) DO UPDATE SET
  default_host_id = EXCLUDED.default_host_id,
  last_host_issue = EXCLUDED.last_host_issue,
  updated_at = clock_timestamp()
RETURNING user_id, assistant_name, selected_model_id, default_host_id, last_host_issue, updated_at`,
		ownerID, hostID, issue,
	).Scan(&value.OwnerID, &value.AssistantName, &value.SelectedModelID, &value.DefaultHostID, &value.LastHostIssue, &value.UpdatedAt)
	if err != nil {
		return app.UserPreferences{}, fmt.Errorf("set default host preference: %w", err)
	}
	return value, nil
}

func (s *Store) AcquireDefaultHostLock(ctx context.Context, ownerID string) (func() error, error) {
	if !domain.ValidUserID(ownerID) {
		return nil, domain.InvalidArgument("user_id", "user id is invalid")
	}
	// The lock uses a pinned PostgreSQL session while HostService performs its
	// own database work through the pool. Refuse a one-connection pool rather
	// than holding its only connection and deadlocking the request.
	if s.pool.Config().MaxConns < 2 {
		return nil, errors.New("default host lock requires at least two database connections")
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire default host lock connection: %w", err)
	}
	lockKey := "morde:default-host:" + ownerID
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1, 0))", lockKey); err != nil {
		closeHijackedConnection(conn)
		return nil, fmt.Errorf("lock default host creation: %w", err)
	}
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var unlocked bool
			if err := conn.QueryRow(releaseCtx, "SELECT pg_advisory_unlock(hashtextextended($1, 0))", lockKey).Scan(&unlocked); err != nil {
				releaseErr = fmt.Errorf("unlock default host creation: %w", err)
				closeHijackedConnection(conn)
				return
			}
			if !unlocked {
				releaseErr = errors.New("default host creation lock was not held")
				closeHijackedConnection(conn)
				return
			}
			conn.Release()
		})
		return releaseErr
	}, nil
}

func closeHijackedConnection(conn interface{ Hijack() *pgx.Conn }) {
	raw := conn.Hijack()
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = raw.Close(closeCtx)
}

var _ app.UserPreferencesStore = (*Store)(nil)
