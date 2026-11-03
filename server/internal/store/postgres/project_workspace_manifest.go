package postgres

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/workspace"
)

func (s *Store) GetProjectWorkspaceManifestPage(ctx context.Context, actorID, projectID, workspaceID string,
	revision int64, prefix, afterPath string, limit int32) (workspace.ManifestPage, error) {
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) || !domain.ValidWorkspaceID(workspaceID) || revision < 0 ||
		limit < 1 || limit > maxWorkspaceSyncPage || len(prefix) > 1024 || app.WorkspaceFileOperationPathIsSensitive(afterPath) {
		return workspace.ManifestPage{}, domain.InvalidArgument("manifest", "project Workspace manifest query is invalid")
	}
	if afterPath != "" {
		if err := workspace.ValidateRelativePath(afterPath); err != nil {
			return workspace.ManifestPage{}, domain.InvalidArgument("cursor", "project Workspace manifest cursor is invalid")
		}
	}
	if prefix != "" {
		prefix = strings.TrimSuffix(prefix, "/")
		if err := workspace.ValidateRelativePath(prefix); err != nil {
			return workspace.ManifestPage{}, domain.InvalidArgument("prefix", "project Workspace manifest prefix is invalid")
		}
		if app.WorkspaceFileOperationPathIsSensitive(prefix) {
			return workspace.ManifestPage{}, domain.NewError(domain.CodeForbidden, "sensitive Workspace paths are not available in project file views")
		}
	}

	var result workspace.ManifestPage
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := lockProjectForActorInTx(ctx, tx, actorID, projectID, project.RoleViewer, false); err != nil {
			return err
		}
		if err := requireCurrentProjectWorkspaceResourceInTx(ctx, tx, projectID, workspaceID); err != nil {
			return err
		}
		if _, err := requireIndependentResourceAccessInTx(ctx, tx, actorID, project.ResourceWorkspace, workspaceID, true); err != nil {
			return err
		}

		var currentRevision int64
		if err := tx.QueryRow(ctx, "SELECT revision FROM workspaces WHERE id=$1", workspaceID).Scan(&currentRevision); err != nil {
			return fmt.Errorf("read project Workspace revision: %w", err)
		}
		if revision == 0 {
			revision = currentRevision
		}
		if revision > currentRevision {
			return domain.NewError(domain.CodeWorkspaceConflict, "requested project Workspace revision is not available")
		}

		type item struct {
			path string
			file *workspace.File
		}
		visible := make([]item, 0, limit+1)
		internalAfter := afterPath
		for int32(len(visible)) <= limit {
			raw, err := getWorkspaceManifestPageTx(ctx, tx, workspaceID, revision, prefix, internalAfter, maxWorkspaceSyncPage)
			if err != nil {
				return err
			}
			items := make([]item, 0, len(raw.Files)+len(raw.EmptyDirectories))
			for index := range raw.Files {
				file := raw.Files[index]
				items = append(items, item{path: file.Path, file: &file})
			}
			for _, path := range raw.EmptyDirectories {
				items = append(items, item{path: path})
			}
			sort.Slice(items, func(i, j int) bool { return items[i].path < items[j].path })
			for _, value := range items {
				if app.WorkspaceFileOperationPathIsSensitive(value.path) {
					continue
				}
				visible = append(visible, value)
				if int32(len(visible)) > limit {
					break
				}
			}
			if int32(len(visible)) > limit || raw.NextCursor == nil {
				break
			}
			next, err := workspace.DecodeManifestCursor(*raw.NextCursor)
			if err != nil || next.Revision != revision || next.Prefix != prefix || next.After <= internalAfter {
				return domain.NewError(domain.CodeInternal, "stored project Workspace manifest cursor is invalid")
			}
			internalAfter = next.After
		}

		result = workspace.ManifestPage{
			WorkspaceID: workspaceID, Revision: revision,
			Files:            make([]workspace.File, 0, min(len(visible), int(limit))),
			EmptyDirectories: make([]string, 0, min(len(visible), int(limit))),
		}
		pageItems := visible
		if int32(len(pageItems)) > limit {
			pageItems = pageItems[:limit]
			cursor := workspace.EncodeManifestCursor(workspace.ManifestCursor{Revision: revision, Prefix: prefix, After: pageItems[len(pageItems)-1].path})
			result.NextCursor = &cursor
		}
		for _, value := range pageItems {
			if value.file != nil {
				result.Files = append(result.Files, *value.file)
			} else {
				result.EmptyDirectories = append(result.EmptyDirectories, value.path)
			}
		}
		return nil
	})
	if err != nil {
		return workspace.ManifestPage{}, err
	}
	return result, nil
}
