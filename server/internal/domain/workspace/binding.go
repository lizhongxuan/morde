package workspace

import (
	"errors"
	"time"

	"morde/server/internal/domain"
)

type BindingState string

type BindingSyncState string

const (
	BindingPreparing   BindingState = "preparing"
	BindingActive      BindingState = "active"
	BindingUnbinding   BindingState = "unbinding"
	BindingDetached    BindingState = "detached"
	BindingFailed      BindingState = "failed"
	MaxBindingsPerHost              = 64
)

const (
	BindingSyncHealthy          BindingSyncState = "healthy"
	BindingSyncConflict         BindingSyncState = "conflict"
	MaxBindingSyncConflictPaths                  = 100
)

type Binding struct {
	ID              string
	OwnerID         string
	HostID          string
	Workspace       Workspace
	Directory       string
	State           BindingState
	BindingVersion  int64
	DesiredRevision int64
	AppliedRevision int64
	SyncState       BindingSyncState
	SyncConflicts   []string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func ValidateBindingSyncStatus(state BindingSyncState, conflicts []string) error {
	if state != BindingSyncHealthy && state != BindingSyncConflict {
		return errors.New("workspace binding sync state is invalid")
	}
	if len(conflicts) > MaxBindingSyncConflictPaths || (state == BindingSyncHealthy && len(conflicts) != 0) {
		return errors.New("workspace binding sync conflicts are invalid")
	}
	seen := make(map[string]struct{}, len(conflicts))
	for _, path := range conflicts {
		if err := ValidateRelativePath(path); err != nil {
			return err
		}
		if _, exists := seen[path]; exists {
			return errors.New("workspace binding sync conflicts contain duplicate paths")
		}
		seen[path] = struct{}{}
	}
	return nil
}

func ValidateBindingTransition(from, to BindingState) error {
	valid := false
	switch from {
	case BindingPreparing:
		valid = to == BindingActive || to == BindingUnbinding || to == BindingFailed
	case BindingActive:
		valid = to == BindingUnbinding || to == BindingFailed
	case BindingUnbinding:
		valid = to == BindingDetached || to == BindingFailed
	case BindingDetached:
		valid = to == BindingPreparing
	case BindingFailed:
		valid = to == BindingPreparing || to == BindingUnbinding || to == BindingDetached
	}
	if valid {
		return nil
	}
	err := domain.NewError(domain.CodeInvalidState, "invalid workspace binding state transition")
	err.Details["state"] = string(from)
	return err
}
