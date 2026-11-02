package identity

import (
	"errors"
	"time"

	"morde/server/internal/domain"
)

type Role string

const (
	RoleUser       Role = "user"
	RoleAdmin      Role = "admin"
	RoleSuperAdmin Role = "super_admin"
)

func (role Role) IsAdmin() bool {
	return role == RoleAdmin || role == RoleSuperAdmin
}

type AccountStatus string

const (
	StatusPending  AccountStatus = "pending"
	StatusRejected AccountStatus = "rejected"
	StatusApproved AccountStatus = "approved"
	StatusDisabled AccountStatus = "disabled"
)

func (status AccountStatus) Valid() bool {
	return status == StatusPending || status == StatusRejected || status == StatusApproved || status == StatusDisabled
}

func (status AccountStatus) CanTransitionTo(next AccountStatus) bool {
	switch status {
	case StatusPending:
		return next == StatusApproved || next == StatusRejected
	case StatusRejected, StatusDisabled:
		return next == StatusApproved
	case StatusApproved:
		return next == StatusDisabled
	default:
		return false
	}
}

type Kind string

const (
	KindSession Kind = "session"
	KindAPIKey  Kind = "api_key"
	KindDevice  Kind = "device"
	KindClient  Kind = "client"
	// KindScheduleTask is an internal authorization snapshot attached only by
	// the schedule occurrence transaction. It is never a client credential.
	KindScheduleTask Kind = "schedule_task"
	KindTicket       Kind = "ticket"
	KindHostCookie   Kind = "host_cookie"
	KindHostReplica  Kind = "host_replica"
	KindSystem       Kind = "system"
)

type Scope string

const (
	ScopeHostRead        Scope = "host:read"
	ScopeHostCreate      Scope = "host:create"
	ScopeHostManage      Scope = "host:manage"
	ScopeHostDeleteData  Scope = "host:delete_data"
	ScopeComputerObserve Scope = "computer:observe"
	ScopeComputerOperate Scope = "computer:operate"
	ScopeTaskRead        Scope = "task:read"
	ScopeTaskSubmit      Scope = "task:submit"
	ScopeTaskControl     Scope = "task:control"
	ScopeScheduleRead    Scope = "schedule:read"
	ScopeScheduleManage  Scope = "schedule:manage"
	ScopeMCPControl      Scope = "mcp:control"
	ScopeWorkspaceSync   Scope = "workspace:sync"
)

var apiKeyScopes = [...]Scope{
	ScopeHostRead, ScopeHostCreate, ScopeHostManage, ScopeHostDeleteData,
	ScopeComputerObserve, ScopeComputerOperate, ScopeTaskRead, ScopeTaskSubmit, ScopeTaskControl,
	ScopeScheduleRead, ScopeScheduleManage, ScopeWorkspaceSync,
}

func ValidScope(scope Scope) bool {
	if scope == ScopeWorkspaceSync || scope == ScopeMCPControl {
		return true
	}
	for _, allowed := range apiKeyScopes {
		if scope == allowed {
			return true
		}
	}
	return false
}

func (p Principal) HasScope(scope Scope) bool {
	if p.Kind == KindSession {
		return ValidScope(scope)
	}
	if p.Kind == KindClient {
		if len(p.Scopes) != 0 {
			for _, granted := range p.Scopes {
				if granted == scope {
					return true
				}
			}
			return false
		}
		switch scope {
		case ScopeHostRead, ScopeComputerObserve, ScopeTaskRead, ScopeTaskSubmit, ScopeTaskControl, ScopeScheduleRead:
			return true
		default:
			return false
		}
	}
	if p.Kind == KindScheduleTask {
		for _, granted := range p.Scopes {
			if granted == scope {
				return true
			}
		}
		return false
	}
	if p.Kind == KindAPIKey {
		switch scope {
		case ScopeHostRead, ScopeHostCreate, ScopeHostManage, ScopeHostDeleteData, ScopeComputerObserve, ScopeComputerOperate:
			return true
		default:
			return false
		}
	}
	switch p.Kind {
	case KindDevice:
		if scope != ScopeWorkspaceSync {
			return false
		}
	default:
		return false
	}
	for _, granted := range p.Scopes {
		if granted == scope {
			return true
		}
	}
	return false
}

// IsScheduleGrantScopeSet validates the exact public scope set carried by a
// short-lived iOS schedule-management credential. Scheduled Task execution
// scopes are kept on internal Task authorization snapshots, never this grant.
func IsScheduleGrantScopeSet(scopes []Scope) bool {
	if len(scopes) != 2 {
		return false
	}
	return scopes[0] == ScopeScheduleRead && scopes[1] == ScopeScheduleManage ||
		scopes[0] == ScopeScheduleManage && scopes[1] == ScopeScheduleRead
}

// ScheduleTaskExecutionScopes are internal Task scopes attached to a Task
// created by a validated scheduled occurrence. They are not schedule grant
// scopes and are never accepted by the API authentication middleware.
func ScheduleTaskExecutionScopes() []Scope {
	return []Scope{ScopeTaskRead, ScopeTaskSubmit, ScopeTaskControl, ScopeComputerObserve, ScopeComputerOperate}
}

func IsScheduleTaskExecutionScopeSet(scopes []Scope) bool {
	expected := ScheduleTaskExecutionScopes()
	if len(scopes) != len(expected) {
		return false
	}
	for _, scope := range expected {
		found := false
		for _, candidate := range scopes {
			if candidate == scope {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (p Principal) AllowsHost(hostID string) bool {
	if hostID == "" {
		return false
	}
	if p.Kind == KindSession {
		return true
	}
	if p.Kind == KindClient {
		return domain.ValidClientInstallationID(p.ClientInstallationID)
	}
	return p.Kind == KindAPIKey && domain.ValidHostID(hostID)
}

func (p Principal) AllowsWorkspace(workspaceID string) bool {
	if workspaceID == "" {
		return false
	}
	if p.Kind == KindSession {
		return true
	}
	return p.Kind == KindDevice && p.HasScope(ScopeWorkspaceSync) &&
		domain.ValidClientInstallationID(p.ClientInstallationID) && domain.ValidWorkspaceID(workspaceID)
}

func (p Principal) CanAccessHost(hostID string, scope Scope) bool {
	return p.HasScope(scope) && p.AllowsHost(hostID)
}

type Principal struct {
	UserID               string
	Role                 Role
	MustChangePassword   bool
	Kind                 Kind
	TokenID              string
	ClientInstallationID string
	HostScope            string
	HostGeneration       int64
	ReplicaWorkspaceID   string
	BindingVersion       int64
	HostIDs              []string
	WorkspaceIDs         []string
	Scopes               []Scope
}

var ErrInvalidCredential = errors.New("invalid credential")

type User struct {
	ID                  string
	Name                string
	PasswordHash        string
	Role                Role
	Status              AccountStatus
	MustChangePassword  bool
	QuotaHosts          int
	QuotaCPUMilli       int
	QuotaMemMiB         int
	DefaultHostCPUMilli int
	DefaultHostMemMiB   int
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type Token struct {
	ID                   string
	OwnerID              string
	ClientInstallationID string
	ActorUserID          string
	Kind                 Kind
	HostID               string
	PreviewPort          int
	HostIDs              []string
	WorkspaceIDs         []string
	Scopes               []Scope
	Name                 string
	SecretHash           []byte
	ExpiresAt            time.Time
	UsedAt               *time.Time
	LastUsedAt           *time.Time
	RevokedAt            *time.Time
	CreatedAt            time.Time
}

type ClientInstallation struct {
	ID          string
	OwnerID     string
	DisplayName string
	Platform    string
	ExpiresAt   time.Time
	CreatedAt   time.Time
	LastUsedAt  *time.Time
	RevokedAt   *time.Time
}
