package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
)

type UserPreferences struct {
	OwnerID         string
	AssistantName   string
	SelectedModelID string
	DefaultHostID   string
	LastHostIssue   string
	UpdatedAt       time.Time
}

type UserPreferencesStore interface {
	GetUserPreferences(context.Context, string) (UserPreferences, error)
	SaveUserPreferences(context.Context, string, string, string) (UserPreferences, error)
	SetDefaultHost(context.Context, string, string, string) (UserPreferences, error)
	AcquireDefaultHostLock(context.Context, string) (func() error, error)
}

type OnboardingModelCatalog interface {
	List(context.Context, identity.Principal) ([]ModelConnectionView, error)
}

type ComputerStatus string

const (
	ComputerNotCreated          ComputerStatus = "not_created"
	ComputerSelectionRequired   ComputerStatus = "selection_required"
	ComputerAvailable           ComputerStatus = "available"
	ComputerCreating            ComputerStatus = "creating"
	ComputerRestoring           ComputerStatus = "restoring"
	ComputerStopping            ComputerStatus = "stopping"
	ComputerReady               ComputerStatus = "ready"
	ComputerStopped             ComputerStatus = "stopped"
	ComputerCreateFailed        ComputerStatus = "create_failed"
	ComputerRestoreFailed       ComputerStatus = "restore_failed"
	ComputerCapacityUnavailable ComputerStatus = "capacity_unavailable"
	ComputerQuotaExceeded       ComputerStatus = "quota_exceeded"
	ComputerUnavailable         ComputerStatus = "unavailable"
)

type ModelStatus string

const (
	ModelReady                ModelStatus = "ready"
	ModelNotConfigured        ModelStatus = "not_configured"
	ModelSelectionRequired    ModelStatus = "selection_required"
	ModelSelectionUnavailable ModelStatus = "selected_unavailable"
)

type OnboardingModel struct {
	ID           string
	Name         string
	Capabilities ModelCapabilities
}

type OnboardingComputer struct {
	ID    string
	Name  string
	State host.State
}

type OnboardingSnapshot struct {
	Preferences     UserPreferences
	ModelStatus     ModelStatus
	AvailableModels []OnboardingModel
	ComputerStatus  ComputerStatus
	Computers       []OnboardingComputer
	CanSubmit       bool
	NextAction      string
}

type DefaultHostResult struct {
	Snapshot OnboardingSnapshot
	Host     *host.Host
	Created  bool
}

type SaveUserPreferencesCommand struct {
	AssistantName   string
	SelectedModelID string
}

type DefaultHostCommand struct {
	HostID string
}

type OnboardingService struct {
	preferences UserPreferencesStore
	hosts       HostService
	models      OnboardingModelCatalog
	templateID  string
}

func NewOnboardingService(preferences UserPreferencesStore, hosts HostService, models OnboardingModelCatalog, defaultTemplateID string) (*OnboardingService, error) {
	if preferences == nil || hosts == nil || models == nil || !domain.ValidTemplateID(defaultTemplateID) {
		return nil, errors.New("onboarding dependencies and default template are required")
	}
	return &OnboardingService{preferences: preferences, hosts: hosts, models: models, templateID: defaultTemplateID}, nil
}

func (s *OnboardingService) Preferences(ctx context.Context, principal identity.Principal) (UserPreferences, error) {
	if err := validateOnboardingPrincipal(principal); err != nil {
		return UserPreferences{}, err
	}
	value, err := s.preferences.GetUserPreferences(ctx, principal.UserID)
	if err != nil || value.DefaultHostID == "" {
		return value, err
	}
	selected, err := s.hosts.Get(ctx, principal, value.DefaultHostID)
	if err != nil {
		if domain.CodeOf(err) != domain.CodeNotFound {
			return UserPreferences{}, err
		}
		value.DefaultHostID = ""
		return value, nil
	}
	if selected.OwnerID != principal.UserID || selected.DeletedAt != nil || selected.State == host.StateDeleted {
		value.DefaultHostID = ""
	}
	return value, nil
}

func (s *OnboardingService) SavePreferences(ctx context.Context, principal identity.Principal, command SaveUserPreferencesCommand) (UserPreferences, error) {
	if err := validateOnboardingPrincipal(principal); err != nil {
		return UserPreferences{}, err
	}
	command.AssistantName = strings.TrimSpace(command.AssistantName)
	command.SelectedModelID = strings.TrimSpace(command.SelectedModelID)
	if len(command.AssistantName) > 80 {
		return UserPreferences{}, domain.InvalidArgument("assistant_name", "assistant name must be at most 80 bytes")
	}
	if command.SelectedModelID != "" {
		if !domain.ValidModelConnectionID(command.SelectedModelID) {
			return UserPreferences{}, domain.InvalidArgument("selected_model_id", "model id is invalid")
		}
		models, err := s.models.List(ctx, principal)
		if err != nil {
			return UserPreferences{}, err
		}
		if !containsUsableModel(models, command.SelectedModelID) {
			return UserPreferences{}, domain.InvalidArgument("selected_model_id", "model is not available for this user")
		}
	}
	return s.preferences.SaveUserPreferences(ctx, principal.UserID, command.AssistantName, command.SelectedModelID)
}

func (s *OnboardingService) Status(ctx context.Context, principal identity.Principal) (OnboardingSnapshot, error) {
	if err := validateOnboardingPrincipal(principal); err != nil {
		return OnboardingSnapshot{}, err
	}
	preferences, err := s.preferences.GetUserPreferences(ctx, principal.UserID)
	if err != nil {
		return OnboardingSnapshot{}, err
	}
	return s.snapshot(ctx, principal, preferences)
}

func (s *OnboardingService) EnsureDefaultHost(ctx context.Context, principal identity.Principal, command DefaultHostCommand) (result DefaultHostResult, returnErr error) {
	if err := validateOnboardingPrincipal(principal); err != nil {
		return DefaultHostResult{}, err
	}
	if command.HostID != "" {
		if !domain.ValidHostID(command.HostID) {
			return DefaultHostResult{}, domain.InvalidArgument("host_id", "host id is invalid")
		}
		value, err := s.ownedHost(ctx, principal, command.HostID)
		if err != nil {
			return DefaultHostResult{}, err
		}
		preferences, err := s.preferences.SetDefaultHost(ctx, principal.UserID, value.ID, "")
		if err != nil {
			return DefaultHostResult{}, err
		}
		snapshot, err := s.snapshot(ctx, principal, preferences)
		return DefaultHostResult{Snapshot: snapshot, Host: &value}, err
	}

	unlock, err := s.preferences.AcquireDefaultHostLock(ctx, principal.UserID)
	if err != nil {
		return DefaultHostResult{}, fmt.Errorf("acquire default host lock: %w", err)
	}
	if unlock == nil {
		return DefaultHostResult{}, errors.New("default host lock has no release function")
	}
	defer func() {
		if err := unlock(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("release default host lock: %w", err))
		}
	}()

	preferences, err := s.preferences.GetUserPreferences(ctx, principal.UserID)
	if err != nil {
		return DefaultHostResult{}, err
	}
	values, err := s.ownedHosts(ctx, principal)
	if err != nil {
		return DefaultHostResult{}, err
	}
	var value host.Host
	if preferences.DefaultHostID != "" {
		value = findOnboardingHost(values, preferences.DefaultHostID)
	}
	if value.ID == "" {
		switch len(values) {
		case 0:
			value, err = s.hosts.Create(ctx, principal, CreateHostCommand{TemplateID: s.templateID})
			if err != nil {
				if issue := defaultHostIssue(err); issue != "" {
					preferences, err = s.preferences.SetDefaultHost(ctx, principal.UserID, "", issue)
					if err != nil {
						return DefaultHostResult{}, err
					}
					snapshot, snapshotErr := s.snapshot(ctx, principal, preferences)
					return DefaultHostResult{Snapshot: snapshot}, snapshotErr
				}
				return DefaultHostResult{}, err
			}
			preferences, err = s.preferences.SetDefaultHost(ctx, principal.UserID, value.ID, "")
			if err != nil {
				return DefaultHostResult{}, err
			}
			result.Created = true
		case 1:
			value = values[0]
			preferences, err = s.preferences.SetDefaultHost(ctx, principal.UserID, value.ID, "")
			if err != nil {
				return DefaultHostResult{}, err
			}
		default:
			snapshot, snapshotErr := s.snapshot(ctx, principal, preferences)
			return DefaultHostResult{Snapshot: snapshot}, snapshotErr
		}
	}

	if value.State == host.StateStopped {
		stoppedHost := value
		value, err = s.hosts.Restore(ctx, principal, stoppedHost.ID)
		if err != nil {
			if issue := defaultHostIssue(err); issue != "" {
				preferences, err = s.preferences.SetDefaultHost(ctx, principal.UserID, stoppedHost.ID, issue)
				if err != nil {
					return DefaultHostResult{}, err
				}
				snapshot, snapshotErr := s.snapshot(ctx, principal, preferences)
				return DefaultHostResult{Snapshot: snapshot, Host: &stoppedHost, Created: result.Created}, snapshotErr
			}
			return DefaultHostResult{}, err
		}
	}
	if preferences.LastHostIssue != "" && value.State != host.StateStopped {
		preferences, err = s.preferences.SetDefaultHost(ctx, principal.UserID, value.ID, "")
		if err != nil {
			return DefaultHostResult{}, err
		}
	}
	snapshot, err := s.snapshot(ctx, principal, preferences)
	if err != nil {
		return DefaultHostResult{}, err
	}
	return DefaultHostResult{Snapshot: snapshot, Host: &value, Created: result.Created}, nil
}

func (s *OnboardingService) snapshot(ctx context.Context, principal identity.Principal, preferences UserPreferences) (OnboardingSnapshot, error) {
	models, err := s.models.List(ctx, principal)
	if err != nil {
		return OnboardingSnapshot{}, err
	}
	availableModels := make([]OnboardingModel, 0, len(models))
	for _, model := range models {
		if usableModel(model) {
			availableModels = append(availableModels, OnboardingModel{ID: model.ID, Name: model.Name, Capabilities: model.Capabilities})
		}
	}
	modelStatus := ModelNotConfigured
	selectedAvailable := preferences.SelectedModelID != "" && containsUsableModel(models, preferences.SelectedModelID)
	if preferences.SelectedModelID != "" && selectedAvailable {
		modelStatus = ModelReady
	} else if preferences.SelectedModelID != "" {
		modelStatus = ModelSelectionUnavailable
	} else if len(availableModels) > 0 {
		modelStatus = ModelSelectionRequired
	}

	hosts, err := s.ownedHosts(ctx, principal)
	if err != nil {
		return OnboardingSnapshot{}, err
	}
	computers := make([]OnboardingComputer, 0, len(hosts))
	for _, value := range hosts {
		computers = append(computers, OnboardingComputer{ID: value.ID, Name: value.Name, State: value.State})
	}
	computerStatus := ComputerNotCreated
	defaultHostID := preferences.DefaultHostID
	selected := findOnboardingHost(hosts, defaultHostID)
	if selected.ID == "" {
		defaultHostID = ""
		switch len(hosts) {
		case 0:
			switch preferences.LastHostIssue {
			case "capacity_unavailable":
				computerStatus = ComputerCapacityUnavailable
			case "quota_exceeded":
				computerStatus = ComputerQuotaExceeded
			}
		case 1:
			computerStatus = ComputerAvailable
		default:
			computerStatus = ComputerSelectionRequired
		}
	} else {
		switch selected.State {
		case host.StateCreating:
			computerStatus = ComputerCreating
			if selected.RuntimeGeneration > 1 {
				computerStatus = ComputerRestoring
			}
		case host.StateStopping:
			computerStatus = ComputerStopping
		case host.StateRunning:
			computerStatus = ComputerReady
		case host.StateStopped:
			computerStatus = ComputerStopped
			switch preferences.LastHostIssue {
			case "capacity_unavailable":
				computerStatus = ComputerCapacityUnavailable
			case "quota_exceeded":
				computerStatus = ComputerQuotaExceeded
			}
		case host.StateFailed:
			computerStatus = ComputerCreateFailed
			if selected.RuntimeGeneration > 1 {
				computerStatus = ComputerRestoreFailed
			}
		case host.StateDeleting, host.StateDeleted:
			computerStatus = ComputerUnavailable
		default:
			computerStatus = ComputerUnavailable
		}
	}
	canSubmit := modelStatus == ModelReady && preferences.SelectedModelID != "" && selectedAvailable && computerStatus == ComputerReady
	nextAction := nextOnboardingAction(modelStatus, computerStatus)
	preferences.DefaultHostID = defaultHostID
	return OnboardingSnapshot{
		Preferences: preferences, ModelStatus: modelStatus, AvailableModels: availableModels,
		ComputerStatus: computerStatus, Computers: computers, CanSubmit: canSubmit, NextAction: nextAction,
	}, nil
}

func (s *OnboardingService) ownedHost(ctx context.Context, principal identity.Principal, hostID string) (host.Host, error) {
	value, err := s.hosts.Get(ctx, principal, hostID)
	if err != nil {
		return host.Host{}, err
	}
	if value.OwnerID != principal.UserID || value.State == host.StateDeleted || value.DeletedAt != nil {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	return value, nil
}

func (s *OnboardingService) ownedHosts(ctx context.Context, principal identity.Principal) ([]host.Host, error) {
	values, err := s.hosts.List(ctx, principal)
	if err != nil {
		return nil, err
	}
	result := make([]host.Host, 0, len(values))
	for _, value := range values {
		if value.OwnerID == principal.UserID && value.State != host.StateDeleted && value.DeletedAt == nil {
			result = append(result, value)
		}
	}
	return result, nil
}

func validateOnboardingPrincipal(principal identity.Principal) error {
	if principal.Kind != identity.KindSession || !domain.ValidUserID(principal.UserID) ||
		(principal.Role != identity.RoleUser && !principal.Role.IsAdmin()) || principal.HostScope != "" ||
		len(principal.HostIDs) != 0 || len(principal.Scopes) != 0 {
		return domain.NewError(domain.CodeForbidden, "portal session required")
	}
	return nil
}

// Vision is an independent UI capability: text/tool models can submit
// non-visual tasks, while the Portal uses Vision to hide desktop capability.
func usableModel(model ModelConnectionView) bool {
	return model.CheckedAt != nil && model.Capabilities.Text && model.Capabilities.Tools
}

func containsUsableModel(models []ModelConnectionView, id string) bool {
	for _, model := range models {
		if model.ID == id && usableModel(model) {
			return true
		}
	}
	return false
}

func findOnboardingHost(hosts []host.Host, id string) host.Host {
	if id == "" {
		return host.Host{}
	}
	for _, value := range hosts {
		if value.ID == id {
			return value
		}
	}
	return host.Host{}
}

func defaultHostIssue(err error) string {
	switch domain.CodeOf(err) {
	case domain.CodeCapacityExceeded:
		return "capacity_unavailable"
	case domain.CodeQuotaExceeded:
		return "quota_exceeded"
	default:
		return ""
	}
}

func nextOnboardingAction(modelStatus ModelStatus, computerStatus ComputerStatus) string {
	if modelStatus != ModelReady {
		return "configure_model"
	}
	switch computerStatus {
	case ComputerNotCreated, ComputerCapacityUnavailable:
		return "create_or_retry_computer"
	case ComputerQuotaExceeded:
		return "contact_admin"
	case ComputerSelectionRequired, ComputerAvailable:
		return "choose_computer"
	case ComputerCreating, ComputerRestoring, ComputerStopping:
		return "wait_for_computer"
	case ComputerStopped:
		return "resume_computer"
	case ComputerCreateFailed, ComputerRestoreFailed, ComputerUnavailable:
		return "choose_another_computer"
	case ComputerReady:
		return "submit_task"
	default:
		return "review_setup"
	}
}
