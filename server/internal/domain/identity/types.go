package identity

import (
	"errors"
	"time"

	"morde/server/internal/domain"
)

type Role string

const (
	RoleUser  Role = "user"
	RoleAdmin Role = "admin"
)

type Kind string

const (
	KindSession     Kind = "session"
	KindAPIKey      Kind = "api_key"
	KindDevice      Kind = "device"
	KindClient      Kind = "client"
	KindTicket      Kind = "ticket"
	KindHostCookie  Kind = "host_cookie"
	KindHostReplica Kind = "host_replica"
	KindSystem      Kind = "system"
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
	ScopeWorkspaceSync   Scope = "workspace:sync"
)

var apiKeyScopes = [...]Scope{
	ScopeHostRead, ScopeHostCreate, ScopeHostManage, ScopeHostDeleteData,
	ScopeComputerObserve, ScopeComputerOperate, ScopeTaskRead, ScopeTaskSubmit, ScopeTaskControl,
	ScopeScheduleRead, ScopeScheduleManage, ScopeWorkspaceSync,
}

func ValidScope(scope Scope) bool {
	if scope == ScopeWorkspaceSync {
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
		switch scope {
		case ScopeHostRead, ScopeComputerObserve, ScopeTaskRead, ScopeTaskSubmit, ScopeTaskControl, ScopeScheduleRead:
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
	case KindAPIKey:
		if !ValidScope(scope) || scope == ScopeWorkspaceSync {
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
	if p.Kind != KindAPIKey {
		return false
	}
	for _, allowed := range p.HostIDs {
		if allowed == hostID {
			return true
		}
	}
	return false
}

func (p Principal) AllowsWorkspace(workspaceID string) bool {
	if workspaceID == "" {
		return false
	}
	if p.Kind == KindSession {
		return true
	}
	if p.Kind != KindDevice || !p.HasScope(ScopeWorkspaceSync) {
		return false
	}
	for _, allowed := range p.WorkspaceIDs {
		if allowed == workspaceID {
			return true
		}
	}
	return false
}

func (p Principal) CanAccessHost(hostID string, scope Scope) bool {
	return p.HasScope(scope) && p.AllowsHost(hostID)
}

type Principal struct {
	UserID               string
	Role                 Role
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
	ID            string
	Name          string
	PasswordHash  string
	Role          Role
	QuotaHosts    int
	QuotaCPUMilli int
	QuotaMemMiB   int
	Disabled      bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
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

type ClientAuthorization struct {
	ID                    string
	DisplayName           string
	Platform              string
	WantsWorkspaceSync    bool
	RenewalInstallationID string
	SecretHash            []byte
	UserCodeHash          []byte
	State                 string
	InstallationID        string
	OwnerID               string
	WorkspaceIDs          []string
	ExpiresAt             time.Time
	CreatedAt             time.Time
}

type ClientInstallation struct {
	ID           string
	OwnerID      string
	DisplayName  string
	Platform     string
	WorkspaceIDs []string
	ExpiresAt    time.Time
	CreatedAt    time.Time
	LastUsedAt   *time.Time
	RevokedAt    *time.Time
}
