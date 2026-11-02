package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
)

func (s *Store) ListAdminUsersPage(ctx context.Context, status *identity.AccountStatus, limit, offset int) ([]identity.User, int64, error) {
	statusFilter := ""
	if status != nil {
		statusFilter = string(*status)
	}
	var total int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE $1='' OR status=$1`, statusFilter).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count admin users: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT id,name,role,status,must_change_password,quota_hosts,quota_cpu_milli,quota_mem_mib,
default_host_cpu_milli,default_host_mem_mib,created_at,updated_at FROM users
WHERE $1='' OR status=$1 ORDER BY created_at DESC,id LIMIT $2 OFFSET $3`, statusFilter, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list admin users page: %w", err)
	}
	defer rows.Close()
	users := make([]identity.User, 0)
	for rows.Next() {
		var user identity.User
		var role, accountStatus string
		if err := rows.Scan(&user.ID, &user.Name, &role, &accountStatus, &user.MustChangePassword,
			&user.QuotaHosts, &user.QuotaCPUMilli, &user.QuotaMemMiB, &user.DefaultHostCPUMilli, &user.DefaultHostMemMiB,
			&user.CreatedAt, &user.UpdatedAt); err != nil {
			return nil, 0, fmt.Errorf("read admin user page: %w", err)
		}
		user.Role, user.Status = identity.Role(role), identity.AccountStatus(accountStatus)
		users = append(users, user)
	}
	return users, total, rows.Err()
}

func (s *Store) ListAdminAgentStates(ctx context.Context) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT state,count(*) FROM account_agent_runtimes
WHERE state IN ('stopped','failed') OR lease_expires_at>clock_timestamp() GROUP BY state ORDER BY state`)
	if err != nil {
		return nil, fmt.Errorf("list admin agent states: %w", err)
	}
	defer rows.Close()
	result := make(map[string]int64)
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			return nil, fmt.Errorf("read admin agent state: %w", err)
		}
		result[state] = count
	}
	return result, rows.Err()
}

func (s *Store) ListAdminInstallations(ctx context.Context, ownerID string, limit, offset int) (app.AdminPage[app.AdminInstallation], error) {
	page := app.AdminPage[app.AdminInstallation]{Items: make([]app.AdminInstallation, 0), Limit: limit, Offset: offset}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM client_installations WHERE owner_id=$1`, ownerID).Scan(&page.Total); err != nil {
		return page, fmt.Errorf("count admin client installations: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT id,display_name,platform,created_at,last_used_at,expires_at,revoked_at
FROM client_installations WHERE owner_id=$1 ORDER BY created_at DESC,id LIMIT $2 OFFSET $3`, ownerID, limit, offset)
	if err != nil {
		return page, fmt.Errorf("list admin client installations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item app.AdminInstallation
		var lastUsed, expires, revoked pgtype.Timestamptz
		if err := rows.Scan(&item.ID, &item.DisplayName, &item.Platform, &item.CreatedAt, &lastUsed, &expires, &revoked); err != nil {
			return page, fmt.Errorf("read admin client installation: %w", err)
		}
		item.LastUsedAt, item.ExpiresAt, item.RevokedAt = timePointer(lastUsed), timePointer(expires), timePointer(revoked)
		page.Items = append(page.Items, item)
	}
	return page, rows.Err()
}

func (s *Store) ListAdminAuthorizations(ctx context.Context, ownerID string, limit, offset int) (app.AdminPage[app.AdminAuthorization], error) {
	page := app.AdminPage[app.AdminAuthorization]{Items: make([]app.AdminAuthorization, 0), Limit: limit, Offset: offset}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM client_authorizations WHERE owner_id=$1`, ownerID).Scan(&page.Total); err != nil {
		return page, fmt.Errorf("count admin client authorizations: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT id,display_name,platform,state,COALESCE(installation_id,''),created_at,expires_at,claimed_at
FROM client_authorizations WHERE owner_id=$1 ORDER BY created_at DESC,id LIMIT $2 OFFSET $3`, ownerID, limit, offset)
	if err != nil {
		return page, fmt.Errorf("list admin client authorizations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item app.AdminAuthorization
		var claimed pgtype.Timestamptz
		if err := rows.Scan(&item.ID, &item.DisplayName, &item.Platform, &item.State, &item.InstallationID,
			&item.CreatedAt, &item.ExpiresAt, &claimed); err != nil {
			return page, fmt.Errorf("read admin client authorization: %w", err)
		}
		item.ClaimedAt = timePointer(claimed)
		page.Items = append(page.Items, item)
	}
	return page, rows.Err()
}

func (s *Store) ListAdminModelCalls(ctx context.Context, ownerID string, limit, offset int) (app.AdminPage[app.AdminModelCall], error) {
	page := app.AdminPage[app.AdminModelCall]{Items: make([]app.AdminModelCall, 0), Limit: limit, Offset: offset}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM model_call_ledger WHERE owner_id=$1`, ownerID).Scan(&page.Total); err != nil {
		return page, fmt.Errorf("count admin model calls: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT source,call_id,model_id,provider_model,COALESCE(task_id,''),reserved_tokens,state,
input_tokens,output_tokens,created_at,settled_at FROM model_call_ledger
WHERE owner_id=$1 ORDER BY created_at DESC,source,call_id LIMIT $2 OFFSET $3`, ownerID, limit, offset)
	if err != nil {
		return page, fmt.Errorf("list admin model calls: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item app.AdminModelCall
		var input, output pgtype.Int8
		var settled pgtype.Timestamptz
		if err := rows.Scan(&item.Source, &item.CallID, &item.ModelID, &item.ProviderModel, &item.TaskID, &item.ReservedTokens,
			&item.State, &input, &output, &item.CreatedAt, &settled); err != nil {
			return page, fmt.Errorf("read admin model call: %w", err)
		}
		if input.Valid {
			value := input.Int64
			item.InputTokens = &value
		}
		if output.Valid {
			value := output.Int64
			item.OutputTokens = &value
		}
		item.SettledAt = timePointer(settled)
		page.Items = append(page.Items, item)
	}
	return page, rows.Err()
}

func (s *Store) GetAdminUsage(ctx context.Context, from, to time.Time) (app.AdminUsage, error) {
	result := app.AdminUsage{From: from, To: to, ByDay: make([]app.AdminUsageDay, 0), ByModel: make([]app.AdminUsageModel, 0)}
	err := s.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE state='settled'),count(*) FILTER(WHERE state='unknown'),
COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0),count(DISTINCT owner_id)
FROM model_call_ledger WHERE created_at >= $1 AND created_at < $2`, from, to).Scan(
		&result.Summary.Calls, &result.Summary.SettledCalls, &result.Summary.UnknownCalls,
		&result.Summary.InputTokens, &result.Summary.OutputTokens, &result.Summary.ActiveAccounts)
	if err != nil {
		return result, fmt.Errorf("summarize admin model usage: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT to_char(created_at AT TIME ZONE 'UTC','YYYY-MM-DD'),count(*),
COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0),count(DISTINCT owner_id)
FROM model_call_ledger WHERE created_at >= $1 AND created_at < $2
GROUP BY 1 ORDER BY 1`, from, to)
	if err != nil {
		return result, fmt.Errorf("list admin model usage by day: %w", err)
	}
	for rows.Next() {
		var item app.AdminUsageDay
		if err := rows.Scan(&item.Date, &item.Calls, &item.InputTokens, &item.OutputTokens, &item.ActiveAccounts); err != nil {
			rows.Close()
			return result, fmt.Errorf("read admin daily model usage: %w", err)
		}
		result.ByDay = append(result.ByDay, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	rows.Close()
	rows, err = s.pool.Query(ctx, `SELECT model_id,count(*),COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0)
FROM model_call_ledger WHERE created_at >= $1 AND created_at < $2 GROUP BY model_id ORDER BY count(*) DESC,model_id`, from, to)
	if err != nil {
		return result, fmt.Errorf("list admin model usage by model: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item app.AdminUsageModel
		if err := rows.Scan(&item.ModelID, &item.Calls, &item.InputTokens, &item.OutputTokens); err != nil {
			return result, fmt.Errorf("read admin model usage by model: %w", err)
		}
		result.ByModel = append(result.ByModel, item)
	}
	return result, rows.Err()
}

func (s *Store) GetAdminStats(ctx context.Context, now time.Time) (app.AdminStats, error) {
	result := app.AdminStats{Platforms: make([]app.AdminPlatformStats, 0), Cohorts: make([]app.AdminCohortStats, 0)}
	if err := s.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE status='approved'),
count(*) FILTER(WHERE status='pending'),count(*) FILTER(WHERE status='rejected'),count(*) FILTER(WHERE status='disabled') FROM users`).Scan(
		&result.Users.Total, &result.Users.Approved, &result.Users.Pending, &result.Users.Rejected, &result.Users.Disabled); err != nil {
		return result, fmt.Errorf("count admin users: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT platform,count(*),count(*) FILTER(WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at>$2) AND last_used_at >= $1),
count(DISTINCT owner_id) FROM client_installations GROUP BY platform ORDER BY platform`, now.AddDate(0, 0, -30), now)
	if err != nil {
		return result, fmt.Errorf("list admin platform stats: %w", err)
	}
	for rows.Next() {
		var item app.AdminPlatformStats
		if err := rows.Scan(&item.Platform, &item.Installations, &item.ActiveInstallations, &item.Users); err != nil {
			rows.Close()
			return result, fmt.Errorf("read admin platform stats: %w", err)
		}
		result.Platforms = append(result.Platforms, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	rows.Close()
	rows, err = s.pool.Query(ctx, `SELECT to_char(date_trunc('week',u.created_at AT TIME ZONE 'UTC'),'IYYY-"W"IW'),count(*),
count(*) FILTER(WHERE EXISTS(SELECT 1 FROM client_installations i WHERE i.owner_id=u.id))
FROM users u WHERE u.created_at >= (date_trunc('week',$1::timestamptz AT TIME ZONE 'UTC')-interval '11 weeks') AT TIME ZONE 'UTC'
GROUP BY 1 ORDER BY 1`, now)
	if err != nil {
		return result, fmt.Errorf("list admin signup cohorts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item app.AdminCohortStats
		if err := rows.Scan(&item.Week, &item.Signups, &item.WithInstallation); err != nil {
			return result, fmt.Errorf("read admin signup cohort: %w", err)
		}
		result.Cohorts = append(result.Cohorts, item)
	}
	return result, rows.Err()
}

var _ app.AdminConsoleStore = (*Store)(nil)
