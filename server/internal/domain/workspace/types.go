package workspace

import "time"

type State string

const (
	StateReady    State = "ready"
	StateDeleting State = "deleting"
	StateDeleted  State = "deleted"
)

type Workspace struct {
	ID         string
	OwnerID    string
	Name       string
	State      State
	Revision   int64
	UsedBytes  int64
	QuotaBytes int64
	LastError  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	DeletedAt  *time.Time
}

// AdminSummary contains only workspace metadata and current Host associations.
// It intentionally has no filesystem paths, file manifest, or block data.
type AdminSummary struct {
	ID         string
	OwnerID    string
	OwnerName  string
	Name       string
	State      State
	Revision   int64
	UsedBytes  int64
	QuotaBytes int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Bindings   []AdminBinding
}

type AdminBinding struct {
	HostID          string
	HostName        string
	State           BindingState
	SyncState       BindingSyncState
	DesiredRevision int64
	AppliedRevision int64
}

type AdminPage struct {
	Items      []AdminSummary
	NextCursor string
}
