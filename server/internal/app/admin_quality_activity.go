package app

import (
	"context"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type AdminQuality struct {
	Days              int    `json:"days"`
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

type AdminPlaces struct {
	Days              int    `json:"days"`
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

type AdminActivityDay struct {
	Date          string `json:"date"`
	HubOperations int64  `json:"hub_operations"`
	TaskEvents    int64  `json:"task_events"`
	AuditEvents   int64  `json:"audit_events"`
}

type AdminHubOperationCount struct {
	Action string `json:"action"`
	Status string `json:"status"`
	Count  int64  `json:"count"`
}

type AdminTaskEventCount struct {
	Type  string `json:"type"`
	Count int64  `json:"count"`
}

type AdminAuditEventCount struct {
	Action string `json:"action"`
	Count  int64  `json:"count"`
}

type AdminActivity struct {
	Days          int                      `json:"days"`
	From          time.Time                `json:"from"`
	To            time.Time                `json:"to"`
	ByDay         []AdminActivityDay       `json:"by_day"`
	HubOperations []AdminHubOperationCount `json:"hub_operations"`
	TaskEvents    []AdminTaskEventCount    `json:"task_events"`
	AuditEvents   []AdminAuditEventCount   `json:"audit_events"`
}

type AdminQualityActivityStore interface {
	GetAdminActivity(context.Context, time.Time, time.Time) (AdminActivity, error)
}

type AdminQualityActivityService struct {
	store AdminQualityActivityStore
}

func NewAdminQualityActivityService(store AdminQualityActivityStore) (*AdminQualityActivityService, error) {
	if store == nil {
		return nil, domain.InvalidArgument("admin_quality_activity", "admin quality activity store is required")
	}
	return &AdminQualityActivityService{store: store}, nil
}

func (s *AdminQualityActivityService) Quality(_ context.Context, principal identity.Principal, days int) (AdminQuality, error) {
	if err := authorizeAdmin(principal); err != nil {
		return AdminQuality{}, err
	}
	if err := validateAdminQualityActivityDays(days); err != nil {
		return AdminQuality{}, err
	}
	return AdminQuality{Days: days, UnavailableReason: "consented quality sample source is not configured"}, nil
}

func (s *AdminQualityActivityService) Activity(ctx context.Context, principal identity.Principal, days int) (AdminActivity, error) {
	if err := authorizeAdmin(principal); err != nil {
		return AdminActivity{}, err
	}
	if err := validateAdminQualityActivityDays(days); err != nil {
		return AdminActivity{}, err
	}
	to := time.Now().UTC()
	today := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	from := today.AddDate(0, 0, 1-days)
	activity, err := s.store.GetAdminActivity(ctx, from, to)
	if err != nil {
		return AdminActivity{}, err
	}
	activity.Days, activity.From, activity.To = days, from, to
	activity.ByDay = adminActivityDays(activity.ByDay, from, days)
	if activity.HubOperations == nil {
		activity.HubOperations = make([]AdminHubOperationCount, 0)
	}
	if activity.TaskEvents == nil {
		activity.TaskEvents = make([]AdminTaskEventCount, 0)
	}
	if activity.AuditEvents == nil {
		activity.AuditEvents = make([]AdminAuditEventCount, 0)
	}
	return activity, nil
}

func (s *AdminQualityActivityService) Places(_ context.Context, principal identity.Principal, days int) (AdminPlaces, error) {
	if err := authorizeAdmin(principal); err != nil {
		return AdminPlaces{}, err
	}
	if err := validateAdminQualityActivityDays(days); err != nil {
		return AdminPlaces{}, err
	}
	return AdminPlaces{Days: days, UnavailableReason: "no compliant aggregated location source is configured"}, nil
}

func validateAdminQualityActivityDays(days int) error {
	if days != 7 && days != 30 && days != 90 {
		return domain.InvalidArgument("days", "days must be 7, 30, or 90")
	}
	return nil
}

func adminActivityDays(rows []AdminActivityDay, from time.Time, days int) []AdminActivityDay {
	counts := make(map[string]AdminActivityDay, len(rows))
	for _, row := range rows {
		counts[row.Date] = row
	}
	result := make([]AdminActivityDay, 0, days)
	for i := 0; i < days; i++ {
		date := from.AddDate(0, 0, i).Format("2006-01-02")
		row := counts[date]
		row.Date = date
		result = append(result, row)
	}
	return result
}
