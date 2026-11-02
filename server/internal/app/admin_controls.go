package app

import (
	"context"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

const maxAdminControlNoteLength = 256

type AdminControlRule struct {
	ID            int64
	Threshold     int64
	Enabled       bool
	Note          string
	CreatedAt     time.Time
	LastFiredAt   *time.Time
	FiredAccounts int64
	Version       int64
}

type AdminControlNotification struct {
	Kind          string
	RuleID        *int64
	AccountsTotal int64
	CreatedAt     time.Time
}

type AdminControls struct {
	Rules            []AdminControlRule
	AccountsTotal    int64
	NextThreshold    *int64
	NotifyConfigured bool
	Notifications    []AdminControlNotification
}

type AdminControlEvaluation struct {
	AccountsTotal int64
	Fired         []AdminControlRule
}

type AdminControlNotifyTest struct {
	Sent       bool
	Configured bool
	CreatedAt  time.Time
}

type UpdateAdminControlRuleCommand struct {
	ID              int64
	ExpectedVersion int64
	Threshold       *int64
	Enabled         *bool
	Note            *string
	Rearm           bool
}

type AdminControlsStore interface {
	ListAdminControls(context.Context) (AdminControls, error)
	CreateAdminControlRule(context.Context, identity.Principal, int64, string) (AdminControlRule, error)
	UpdateAdminControlRule(context.Context, identity.Principal, UpdateAdminControlRuleCommand) (AdminControlRule, error)
	DeleteAdminControlRule(context.Context, identity.Principal, int64, int64) error
	EvaluateAdminControlRules(context.Context, identity.Principal) (AdminControlEvaluation, error)
	CreateAdminControlNotifyTest(context.Context, identity.Principal) (AdminControlNotifyTest, error)
}

type AdminControlsService struct {
	store AdminControlsStore
}

func NewAdminControlsService(store AdminControlsStore) (*AdminControlsService, error) {
	if store == nil {
		return nil, domain.InvalidArgument("admin_controls", "admin controls store is required")
	}
	return &AdminControlsService{store: store}, nil
}

func (s *AdminControlsService) List(ctx context.Context, principal identity.Principal) (AdminControls, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return AdminControls{}, err
	}
	value, err := s.store.ListAdminControls(ctx)
	if err != nil {
		return AdminControls{}, err
	}
	if err := validateAdminControls(value); err != nil {
		return AdminControls{}, domain.NewError(domain.CodeInternal, "admin controls receipt is invalid")
	}
	return value, nil
}

func (s *AdminControlsService) Create(ctx context.Context, principal identity.Principal, threshold int64, note string) (AdminControlRule, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return AdminControlRule{}, err
	}
	note, err := validateAdminControlRuleFields(threshold, note)
	if err != nil {
		return AdminControlRule{}, err
	}
	value, err := s.store.CreateAdminControlRule(ctx, principal, threshold, note)
	if err != nil {
		return AdminControlRule{}, err
	}
	if !validAdminControlRule(value) {
		return AdminControlRule{}, domain.NewError(domain.CodeInternal, "admin control rule receipt is invalid")
	}
	return value, nil
}

func (s *AdminControlsService) Update(ctx context.Context, principal identity.Principal, command UpdateAdminControlRuleCommand) (AdminControlRule, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return AdminControlRule{}, err
	}
	if command.ID < 1 || command.ExpectedVersion < 1 ||
		(command.Threshold == nil && command.Enabled == nil && command.Note == nil && !command.Rearm) {
		return AdminControlRule{}, domain.InvalidArgument("admin_control_rule", "rule id, version, and at least one change are required")
	}
	if command.Threshold != nil && *command.Threshold < 1 {
		return AdminControlRule{}, domain.InvalidArgument("threshold", "threshold must be positive")
	}
	if command.Note != nil {
		note := strings.TrimSpace(*command.Note)
		if err := validateAdminControlNote(note); err != nil {
			return AdminControlRule{}, err
		}
		command.Note = &note
	}
	value, err := s.store.UpdateAdminControlRule(ctx, principal, command)
	if err != nil {
		return AdminControlRule{}, err
	}
	if !validAdminControlRule(value) {
		return AdminControlRule{}, domain.NewError(domain.CodeInternal, "admin control rule receipt is invalid")
	}
	return value, nil
}

func (s *AdminControlsService) Delete(ctx context.Context, principal identity.Principal, id, expectedVersion int64) error {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return err
	}
	if id < 1 || expectedVersion < 1 {
		return domain.InvalidArgument("admin_control_rule", "rule id and expected version must be positive")
	}
	return s.store.DeleteAdminControlRule(ctx, principal, id, expectedVersion)
}

func (s *AdminControlsService) Evaluate(ctx context.Context, principal identity.Principal) (AdminControlEvaluation, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return AdminControlEvaluation{}, err
	}
	value, err := s.store.EvaluateAdminControlRules(ctx, principal)
	if err != nil {
		return AdminControlEvaluation{}, err
	}
	if value.AccountsTotal < 0 || value.Fired == nil {
		return AdminControlEvaluation{}, domain.NewError(domain.CodeInternal, "admin control evaluation receipt is invalid")
	}
	for _, rule := range value.Fired {
		if !validAdminControlRule(rule) || !rule.Enabled || rule.LastFiredAt == nil || rule.FiredAccounts != value.AccountsTotal {
			return AdminControlEvaluation{}, domain.NewError(domain.CodeInternal, "admin control evaluation receipt is invalid")
		}
	}
	return value, nil
}

func (s *AdminControlsService) NotifyTest(ctx context.Context, principal identity.Principal) (AdminControlNotifyTest, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return AdminControlNotifyTest{}, err
	}
	value, err := s.store.CreateAdminControlNotifyTest(ctx, principal)
	if err != nil {
		return AdminControlNotifyTest{}, err
	}
	if !value.Sent || !value.Configured || value.CreatedAt.IsZero() {
		return AdminControlNotifyTest{}, domain.NewError(domain.CodeInternal, "local notification test receipt is invalid")
	}
	return value, nil
}

func validateAdminControls(value AdminControls) error {
	if value.AccountsTotal < 0 || value.Rules == nil || value.Notifications == nil || !value.NotifyConfigured {
		return domain.InvalidArgument("admin_controls", "invalid admin controls receipt")
	}
	for _, rule := range value.Rules {
		if !validAdminControlRule(rule) {
			return domain.InvalidArgument("admin_controls", "invalid admin control rule")
		}
	}
	for _, notification := range value.Notifications {
		if (notification.Kind != "test" && notification.Kind != "rule") ||
			(notification.Kind == "rule") != (notification.RuleID != nil) ||
			(notification.RuleID != nil && *notification.RuleID < 1) || notification.AccountsTotal < 0 || notification.CreatedAt.IsZero() {
			return domain.InvalidArgument("admin_controls", "invalid local notification receipt")
		}
	}
	if value.NextThreshold != nil && *value.NextThreshold < 1 {
		return domain.InvalidArgument("admin_controls", "invalid next threshold")
	}
	return nil
}

func validAdminControlRule(value AdminControlRule) bool {
	return value.ID >= 1 && value.Threshold >= 1 && validAdminControlNote(value.Note) &&
		value.Version >= 1 && value.FiredAccounts >= 0 && !value.CreatedAt.IsZero() &&
		(value.LastFiredAt == nil || !value.LastFiredAt.IsZero())
}

func validateAdminControlRuleFields(threshold int64, note string) (string, error) {
	note = strings.TrimSpace(note)
	if threshold < 1 {
		return "", domain.InvalidArgument("threshold", "threshold must be positive")
	}
	if err := validateAdminControlNote(note); err != nil {
		return "", err
	}
	return note, nil
}

func validAdminControlNote(note string) bool {
	return utf8.ValidString(note) && len([]rune(note)) <= maxAdminControlNoteLength &&
		strings.TrimSpace(note) == note && strings.IndexFunc(note, unicode.IsControl) < 0
}

func validateAdminControlNote(note string) error {
	if !validAdminControlNote(note) {
		return domain.InvalidArgument("note", "note must be valid text of at most 256 characters")
	}
	return nil
}
