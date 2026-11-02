package host

import "time"

type Permission string

const (
	PermissionDesktopView     Permission = "desktop:view"
	PermissionDesktopControl  Permission = "desktop:control"
	PermissionTerminalControl Permission = "terminal:control"
	PermissionMCPControl      Permission = "mcp:control"
	PermissionWorkspaceFileRead Permission = "workspace:file:read"
)

func (p Permission) Valid() bool {
	switch p {
	case PermissionDesktopView, PermissionDesktopControl, PermissionTerminalControl, PermissionMCPControl, PermissionWorkspaceFileRead:
		return true
	default:
		return false
	}
}

type Member struct {
	HostID      string
	OwnerID     string
	UserID      string
	Permissions []Permission
	CreatedAt   time.Time
	UpdatedAt   time.Time
	RevokedAt   *time.Time
}

type SharedHost struct {
	HostID      string
	Name        string
	State       State
	Permissions []Permission
}

type ActorAccess struct {
	OwnerID     string
	Permissions []Permission
	Disabled    bool
}
