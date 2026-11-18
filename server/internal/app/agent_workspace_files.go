package app

import (
	"context"
	"errors"
	"os"
	"path"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type AgentWorkspaceEnsurer interface {
	EnsureAgentWorkspace(context.Context, string) (workspace.Workspace, error)
}

type AgentWorkspaceDirectoryReader interface {
	ReadDirectory(string, string, string) ([]workspace.DirectoryEntry, error)
	OpenRegularFile(string, string, string) (*os.File, os.FileInfo, error)
}

type AgentWorkspaceFileEntry struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	SizeBytes  int64  `json:"size_bytes"`
	ModifiedAt string `json:"modified_at"`
}

type AgentWorkspaceFilePage struct {
	WorkspaceID string                    `json:"workspace_id"`
	Path        string                    `json:"path"`
	Entries     []AgentWorkspaceFileEntry `json:"entries"`
	NextCursor  string                    `json:"next_cursor,omitempty"`
}

type AgentWorkspaceView struct {
	ID       string `json:"id"`
	RootPath string `json:"root_path"`
	State    string `json:"state"`
}

type AgentWorkspaceFilesService struct {
	workspaces AgentWorkspaceEnsurer
	current    AgentWorkspaceDirectoryReader
}

func NewAgentWorkspaceFilesService(workspaces AgentWorkspaceEnsurer, current AgentWorkspaceDirectoryReader) (*AgentWorkspaceFilesService, error) {
	if workspaces == nil || current == nil {
		return nil, errors.New("agent Workspace directory dependencies are required")
	}
	return &AgentWorkspaceFilesService{workspaces: workspaces, current: current}, nil
}

func (s *AgentWorkspaceFilesService) GetContext(ctx context.Context, principal identity.Principal) (AgentWorkspaceView, error) {
	value, err := s.resolve(ctx, principal)
	if err != nil {
		return AgentWorkspaceView{}, err
	}
	return AgentWorkspaceView{ID: value.ID, RootPath: "~/workspace", State: string(value.State)}, nil
}

func (s *AgentWorkspaceFilesService) List(ctx context.Context, principal identity.Principal, workspaceID, relativePath, cursor string, limit int) (AgentWorkspaceFilePage, error) {
	value, err := s.resolve(ctx, principal)
	if err != nil {
		return AgentWorkspaceFilePage{}, err
	}
	if value.ID != workspaceID {
		return AgentWorkspaceFilePage{}, domain.NewError(domain.CodeNotFound, "Agent Workspace not found")
	}
	if relativePath != "" && workspace.ValidateRelativePath(relativePath) != nil {
		return AgentWorkspaceFilePage{}, domain.InvalidArgument("path", "Workspace path is invalid")
	}
	if limit < 1 || limit > 200 || len(cursor) > 255 ||
		(cursor != "" && (strings.Contains(cursor, "/") || workspace.ValidateRelativePath(cursor) != nil)) {
		return AgentWorkspaceFilePage{}, domain.InvalidArgument("pagination", "Workspace directory pagination is invalid")
	}
	entries, err := s.current.ReadDirectory(principal.UserID, value.ID, relativePath)
	if err != nil {
		return AgentWorkspaceFilePage{}, err
	}
	result := AgentWorkspaceFilePage{WorkspaceID: value.ID, Path: relativePath, Entries: make([]AgentWorkspaceFileEntry, 0, min(limit, len(entries)))}
	for _, entry := range entries {
		if entry.Name <= cursor {
			continue
		}
		entryPath := entry.Name
		if relativePath != "" {
			entryPath = path.Join(relativePath, entry.Name)
		}
		result.Entries = append(result.Entries, AgentWorkspaceFileEntry{
			Name: entry.Name, Path: entryPath, Kind: entry.Kind, SizeBytes: entry.SizeBytes,
			ModifiedAt: entry.ModifiedAt.Format("2006-01-02T15:04:05.999999999Z07:00"),
		})
		if len(result.Entries) > limit {
			result.NextCursor = result.Entries[limit-1].Name
			result.Entries = result.Entries[:limit]
			break
		}
	}
	return result, nil
}

func (s *AgentWorkspaceFilesService) Open(ctx context.Context, principal identity.Principal, workspaceID, relativePath string) (*os.File, os.FileInfo, error) {
	value, err := s.resolve(ctx, principal)
	if err != nil {
		return nil, nil, err
	}
	if value.ID != workspaceID {
		return nil, nil, domain.NewError(domain.CodeNotFound, "Agent Workspace not found")
	}
	if workspace.ValidateRelativePath(relativePath) != nil {
		return nil, nil, domain.InvalidArgument("path", "Workspace file path is invalid")
	}
	return s.current.OpenRegularFile(principal.UserID, value.ID, relativePath)
}

func (s *AgentWorkspaceFilesService) resolve(ctx context.Context, principal identity.Principal) (workspace.Workspace, error) {
	if s == nil || ctx == nil || principal.Kind != identity.KindSession || !domain.ValidUserID(principal.UserID) {
		return workspace.Workspace{}, domain.NewError(domain.CodeForbidden, "portal session required")
	}
	value, err := s.workspaces.EnsureAgentWorkspace(ctx, principal.UserID)
	if err != nil {
		return workspace.Workspace{}, err
	}
	if !domain.ValidWorkspaceID(value.ID) || value.OwnerID != principal.UserID || value.State != workspace.StateReady || value.DeletedAt != nil {
		return workspace.Workspace{}, domain.NewError(domain.CodeWorkspaceConflict, "Agent Workspace is unavailable")
	}
	return value, nil
}
