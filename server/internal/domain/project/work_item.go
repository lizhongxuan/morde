package project

import (
	"errors"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
)

type WorkItemState string

const (
	WorkItemOpen      WorkItemState = "open"
	WorkItemBlocked   WorkItemState = "blocked"
	WorkItemDelivered WorkItemState = "delivered"
)

type WorkItemDelivery struct {
	TaskID      string
	WorkspaceID string
	Revision    int64
}

type WorkItem struct {
	ID                 string
	ProjectID          string
	Title              string
	Description        string
	AcceptanceCriteria []string
	AssigneeID         string
	State              WorkItemState
	BlockedReason      string
	TaskIDs            []string
	Delivery           *WorkItemDelivery
	Version            int64
}

func (item WorkItem) Validate() error {
	if !ValidID(item.ID) || !ValidID(item.ProjectID) || !domain.ValidUserID(item.AssigneeID) ||
		!validWorkItemText(item.Title, 1, 200) || !validWorkItemText(item.Description, 0, 4096) ||
		item.Version < 1 || !item.State.Valid() || len(item.AcceptanceCriteria) > 100 || len(item.TaskIDs) > 100 {
		return errors.New("project work item is invalid")
	}
	for _, criterion := range item.AcceptanceCriteria {
		if !validWorkItemText(criterion, 1, 1000) {
			return errors.New("project work item acceptance criterion is invalid")
		}
	}
	seenTasks := make(map[string]struct{}, len(item.TaskIDs))
	for _, taskID := range item.TaskIDs {
		if !domain.ValidTaskID(taskID) {
			return errors.New("project work item task id is invalid")
		}
		if _, exists := seenTasks[taskID]; exists {
			return errors.New("project work item task id is duplicated")
		}
		seenTasks[taskID] = struct{}{}
	}
	if item.State == WorkItemBlocked {
		if !validWorkItemText(item.BlockedReason, 1, 2000) || item.Delivery != nil {
			return errors.New("project work item blocker is invalid")
		}
	} else if item.BlockedReason != "" {
		return errors.New("project work item has an unexpected blocker")
	}
	if item.Delivery != nil {
		if item.State != WorkItemDelivered || !domain.ValidTaskID(item.Delivery.TaskID) ||
			!domain.ValidWorkspaceID(item.Delivery.WorkspaceID) || item.Delivery.Revision < 1 || !containsTaskID(item.TaskIDs, item.Delivery.TaskID) {
			return errors.New("project work item delivery evidence is invalid")
		}
	} else if item.State == WorkItemDelivered {
		return errors.New("delivered project work item requires saved workspace evidence")
	}
	return nil
}

func (state WorkItemState) Valid() bool {
	return state == WorkItemOpen || state == WorkItemBlocked || state == WorkItemDelivered
}

func (item WorkItem) Block(reason string) (WorkItem, error) {
	if item.State != WorkItemOpen || !validWorkItemText(reason, 1, 2000) {
		return WorkItem{}, errors.New("project work item cannot be blocked")
	}
	item.State = WorkItemBlocked
	item.BlockedReason = reason
	return item, nil
}

func (item WorkItem) Resume() (WorkItem, error) {
	if item.State != WorkItemBlocked {
		return WorkItem{}, errors.New("project work item is not blocked")
	}
	item.State = WorkItemOpen
	item.BlockedReason = ""
	return item, nil
}

func (item WorkItem) LinkTask(taskID string) (WorkItem, error) {
	if item.State == WorkItemDelivered || !domain.ValidTaskID(taskID) || len(item.TaskIDs) >= 100 {
		return WorkItem{}, errors.New("project work item cannot link this task")
	}
	if containsTaskID(item.TaskIDs, taskID) {
		return item, nil
	}
	item.TaskIDs = append(append([]string(nil), item.TaskIDs...), taskID)
	return item, nil
}

func (item WorkItem) Deliver(evidence WorkItemDelivery) (WorkItem, error) {
	if item.State != WorkItemOpen {
		return WorkItem{}, errors.New("project work item cannot be delivered")
	}
	item.State = WorkItemDelivered
	item.BlockedReason = ""
	item.Delivery = &evidence
	if err := item.Validate(); err != nil {
		return WorkItem{}, err
	}
	return item, nil
}

func containsTaskID(values []string, id string) bool {
	for _, value := range values {
		if value == id {
			return true
		}
	}
	return false
}

func validWorkItemText(value string, min, max int) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) >= min && utf8.RuneCountInString(value) <= max &&
		(strings.TrimSpace(value) == value || value == "")
}
