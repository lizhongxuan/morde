package app

import (
	"context"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/workspace"
)

type ProjectWorkspaceFile struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	SizeBytes int64  `json:"size_bytes"`
	Mode      uint32 `json:"mode"`
}

type ProjectWorkspaceFilePage struct {
	ProjectID        string                 `json:"project_id"`
	WorkspaceID      string                 `json:"workspace_id"`
	Revision         int64                  `json:"revision"`
	Files            []ProjectWorkspaceFile `json:"files"`
	EmptyDirectories []string               `json:"empty_directories"`
	NextCursor       *string                `json:"next_cursor"`
}

type projectWorkspaceManifestStore interface {
	GetProjectWorkspaceManifestPage(context.Context, string, string, string, int64, string, string, int32) (workspace.ManifestPage, error)
}

func (s *ProjectService) GetWorkspaceManifest(ctx context.Context, principal identity.Principal, projectID, workspaceID string,
	revision int64, prefix, cursor string, limit int32) (ProjectWorkspaceFilePage, error) {
	if _, err := s.RequireAccess(ctx, principal, projectID, project.RoleViewer); err != nil {
		return ProjectWorkspaceFilePage{}, err
	}
	if !domain.ValidWorkspaceID(workspaceID) {
		return ProjectWorkspaceFilePage{}, domain.NewError(domain.CodeNotFound, "project Workspace not found")
	}
	if revision < 0 || len(prefix) > 1024 {
		return ProjectWorkspaceFilePage{}, domain.InvalidArgument("manifest", "manifest revision or prefix is invalid")
	}
	if len(cursor) > 512 {
		return ProjectWorkspaceFilePage{}, domain.InvalidArgument("cursor", "manifest cursor is invalid")
	}
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix != "" {
		if err := workspace.ValidateRelativePath(prefix); err != nil {
			return ProjectWorkspaceFilePage{}, domain.InvalidArgument("prefix", "manifest prefix must be a normalized relative path")
		}
		if WorkspaceFileOperationPathIsSensitive(prefix) {
			return ProjectWorkspaceFilePage{}, domain.NewError(domain.CodeForbidden, "sensitive Workspace paths are not available in project file views")
		}
	}
	if limit == 0 {
		limit = manifestDefaultLimit
	}
	if limit < 1 || limit > manifestMaxLimit {
		return ProjectWorkspaceFilePage{}, domain.InvalidArgument("limit", "manifest page limit is out of range")
	}
	afterPath := ""
	if cursor != "" {
		parsed, err := workspace.DecodeManifestCursor(cursor)
		if err != nil || parsed.After == "" || WorkspaceFileOperationPathIsSensitive(parsed.After) {
			return ProjectWorkspaceFilePage{}, domain.InvalidArgument("cursor", "manifest cursor is invalid")
		}
		if revision == 0 {
			revision = parsed.Revision
		}
		if parsed.Revision != revision || parsed.Prefix != prefix {
			return ProjectWorkspaceFilePage{}, domain.InvalidArgument("cursor", "manifest cursor does not match the requested snapshot")
		}
		afterPath = parsed.After
	}
	store, ok := s.store.(projectWorkspaceManifestStore)
	if !ok {
		return ProjectWorkspaceFilePage{}, domain.NewError(domain.CodeWorkspaceUnavailable, "project Workspace file views are unavailable")
	}
	manifest, err := store.GetProjectWorkspaceManifestPage(ctx, principal.UserID, projectID, workspaceID, revision, prefix, afterPath, limit)
	if err != nil {
		return ProjectWorkspaceFilePage{}, err
	}
	if manifest.WorkspaceID != workspaceID || manifest.Revision < 0 || revision > 0 && manifest.Revision != revision {
		return ProjectWorkspaceFilePage{}, domain.NewError(domain.CodeInternal, "project Workspace manifest receipt is invalid")
	}
	result := ProjectWorkspaceFilePage{
		ProjectID: projectID, WorkspaceID: workspaceID, Revision: manifest.Revision,
		Files: make([]ProjectWorkspaceFile, 0, len(manifest.Files)), EmptyDirectories: make([]string, 0, len(manifest.EmptyDirectories)),
		NextCursor: manifest.NextCursor,
	}
	for _, file := range manifest.Files {
		if workspace.ValidateFile(file) != nil || file.Mode > 0777 || WorkspaceFileOperationPathIsSensitive(file.Path) {
			return ProjectWorkspaceFilePage{}, domain.NewError(domain.CodeInternal, "project Workspace manifest contains an invalid or restricted path")
		}
		kind := file.Kind
		if kind == "" {
			kind = "file"
		}
		result.Files = append(result.Files, ProjectWorkspaceFile{Path: file.Path, Kind: kind, SizeBytes: file.SizeBytes, Mode: file.Mode})
	}
	for _, path := range manifest.EmptyDirectories {
		if workspace.ValidateRelativePath(path) != nil || WorkspaceFileOperationPathIsSensitive(path) {
			return ProjectWorkspaceFilePage{}, domain.NewError(domain.CodeInternal, "project Workspace manifest contains an invalid or restricted path")
		}
		result.EmptyDirectories = append(result.EmptyDirectories, path)
	}
	if result.NextCursor != nil {
		parsed, err := workspace.DecodeManifestCursor(*result.NextCursor)
		if err != nil || parsed.Revision != result.Revision || parsed.Prefix != prefix || parsed.After == "" || WorkspaceFileOperationPathIsSensitive(parsed.After) {
			return ProjectWorkspaceFilePage{}, domain.NewError(domain.CodeInternal, "project Workspace manifest cursor is invalid")
		}
	}
	return result, nil
}
