package schedule

import (
	"errors"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

// SyncDefinition is the client-visible definition. Execution credentials and
// occurrence/runtime state stay server-side.
type SyncDefinition struct {
	Status             Status              `json:"status"`
	HostID             *string             `json:"host_id"`
	ModelID            string              `json:"model_id"`
	Title              string              `json:"title"`
	Objective          string              `json:"objective"`
	Timing             Timing              `json:"timing"`
	Budget             SyncBudget          `json:"budget"`
	MisfirePolicy      MisfirePolicy       `json:"misfire_policy"`
	NotificationEvents []NotificationEvent `json:"notification_events"`
	Observation        *ObservationSpec    `json:"observation,omitempty"`
}

type SyncBudget struct {
	ActiveSeconds int64 `json:"active_seconds"`
	ToolCalls     int64 `json:"tool_calls"`
	ModelTokens   int64 `json:"model_tokens"`
}

func NewSyncDefinition(value Schedule) SyncDefinition {
	var hostID *string
	if value.HostID != "" {
		hostID = &value.HostID
	}
	return SyncDefinition{
		Status: value.Status, HostID: hostID, ModelID: value.ModelID, Title: value.Title, Objective: value.Objective,
		Timing: value.Timing, Budget: SyncBudget{ActiveSeconds: value.Budget.ActiveSeconds,
			ToolCalls: value.Budget.ToolCalls, ModelTokens: value.Budget.ModelTokens},
		MisfirePolicy: value.MisfirePolicy, NotificationEvents: append([]NotificationEvent(nil), value.NotificationEvents...),
		Observation: value.Observation,
	}
}

func ValidateSyncDefinition(id string, value SyncDefinition) error {
	hostID := ""
	if value.HostID != nil {
		hostID = *value.HostID
	}
	if !ValidID(id) || !value.Status.Valid() || (hostID != "" && !domain.ValidHostID(hostID)) || value.ModelID == "" ||
		len(value.ModelID) > 200 || strings.TrimSpace(value.Title) == "" || len(strings.TrimSpace(value.Title)) > 32768 ||
		strings.TrimSpace(value.Objective) == "" || len(strings.TrimSpace(value.Objective)) > 32768 ||
		!value.MisfirePolicy.Valid() || value.Timing.Validate() != nil ||
		(task.Budget{ActiveSeconds: value.Budget.ActiveSeconds, ToolCalls: value.Budget.ToolCalls, ModelTokens: value.Budget.ModelTokens}).Validate() != nil {
		return errors.New("schedule sync definition is invalid")
	}
	if hostID == "" && value.Observation != nil {
		return errors.New("hostless schedule cannot configure computer observation")
	}
	seen := make(map[NotificationEvent]struct{}, len(value.NotificationEvents))
	for _, event := range value.NotificationEvents {
		if !event.Valid() {
			return errors.New("schedule notification event is invalid")
		}
		if _, ok := seen[event]; ok {
			return errors.New("schedule notification events must be unique")
		}
		seen[event] = struct{}{}
	}
	if value.NotificationEvents == nil {
		return errors.New("schedule notification events must be an array")
	}
	if value.Observation != nil {
		if err := value.Observation.Validate(); err != nil {
			return err
		}
	}
	return nil
}
