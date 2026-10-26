package app

import (
	"context"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
)

type AuditService struct {
	store AuditStore
}

func NewAuditService(store AuditStore) (*AuditService, error) {
	if store == nil {
		return nil, domain.NewError(domain.CodeInvalidArgument, "audit store is required")
	}
	return &AuditService{store: store}, nil
}

func (s *AuditService) Write(ctx context.Context, entry audit.Entry) error {
	if !entry.Action.Valid() || !validAuditActor(entry.ActorKind) {
		return domain.InvalidArgument("audit", "audit action or actor kind is invalid")
	}
	entry.Detail = sanitizeAuditDetail(entry.Detail)
	return s.store.WriteAudit(ctx, entry)
}

func (s *AuditService) List(ctx context.Context, principal identity.Principal, filter AuditFilter) ([]AuditLog, error) {
	if err := authorizeAdmin(principal); err != nil {
		return nil, err
	}
	if filter.Limit == 0 {
		filter.Limit = 100
	}
	if filter.Limit < 1 || filter.Limit > 500 || len(filter.Actor) > 256 || len(filter.Target) > 256 {
		return nil, domain.InvalidArgument("audit", "audit filters are invalid")
	}
	if filter.Action != "" && !audit.Action(filter.Action).Valid() {
		return nil, domain.InvalidArgument("action", "audit action is invalid")
	}
	if filter.Since != nil {
		since := filter.Since.UTC()
		filter.Since = &since
	}
	return s.store.ListAuditLogs(ctx, filter)
}

func validAuditActor(kind audit.ActorKind) bool {
	switch kind {
	case audit.ActorSession, audit.ActorAPIKey, audit.ActorClient, audit.ActorHostCookie, audit.ActorSystem:
		return true
	default:
		return false
	}
}

func sanitizeAuditDetail(detail map[string]any) map[string]any {
	safe := make(map[string]any, len(detail))
	for key, value := range detail {
		if sensitiveAuditKey(key) {
			continue
		}
		safe[key] = sanitizeAuditValue(value)
	}
	return safe
}

func sanitizeAuditValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return sanitizeAuditDetail(value)
	case map[string]string:
		safe := make(map[string]string, len(value))
		for key, item := range value {
			if !sensitiveAuditKey(key) {
				safe[key] = item
			}
		}
		return safe
	case []any:
		safe := make([]any, len(value))
		for index, item := range value {
			safe[index] = sanitizeAuditValue(item)
		}
		return safe
	case []map[string]any:
		safe := make([]map[string]any, len(value))
		for index, item := range value {
			safe[index] = sanitizeAuditDetail(item)
		}
		return safe
	default:
		return value
	}
}

func sensitiveAuditKey(key string) bool {
	lower := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	return strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "token") || strings.Contains(lower, "key") || strings.Contains(lower, "cookie") || strings.Contains(lower, "authorization")
}
