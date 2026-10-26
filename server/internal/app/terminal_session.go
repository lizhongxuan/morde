package app

import (
	"context"
	"time"
)

const (
	TerminalSessionCreating    = "creating"
	TerminalSessionReady       = "ready"
	TerminalSessionUnknown     = "unknown"
	TerminalSessionInterrupted = "interrupted"
	TerminalSessionAbandoned   = "abandoned"
)

type TerminalSession struct {
	ID                int64
	HostID            string
	OwnerID           string
	ActorID           string
	RuntimeGeneration int64
	ControlVersion    int64
	WorkspaceID       string
	AttemptID         string
	AIOSessionID      string
	State             string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type TerminalSessionStore interface {
	EnsureTerminalSession(context.Context, TerminalSession, bool) (TerminalSession, bool, error)
	CompleteTerminalSession(context.Context, TerminalSession, string) (bool, error)
	MarkTerminalSessionUnknown(context.Context, TerminalSession) error
	MarkTerminalSessionInterrupted(context.Context, string, string, int64, int64, string) (bool, error)
}
