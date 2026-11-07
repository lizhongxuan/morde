package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"path"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type WorkspaceFileOperationAction string

const (
	WorkspaceFileRename WorkspaceFileOperationAction = "rename"
	WorkspaceFileMove   WorkspaceFileOperationAction = "move"
	WorkspaceFileDelete WorkspaceFileOperationAction = "delete"
)

type WorkspaceFileOperationCommand struct {
	Action                  WorkspaceFileOperationAction
	SourcePath              string
	TargetPath              string
	ExpectedCurrentRevision int64
	WriteEnabled            bool
}

type WorkspaceFileOperationResult struct {
	WorkspaceID             string                       `json:"workspace_id"`
	Action                  WorkspaceFileOperationAction `json:"action"`
	SourcePath              string                       `json:"source_path"`
	TargetPath              string                       `json:"target_path"`
	ExpectedCurrentRevision int64                        `json:"expected_current_revision"`
	Revision                int64                        `json:"revision"`
	Replayed                bool                         `json:"replayed"`
}

type workspaceFileOperationStore interface {
	ApplyWorkspaceFileOperation(context.Context, string, string, WorkspaceFileOperationCommand, IdempotencyRecord) (WorkspaceFileOperationResult, error)
}

// ApplyFileOperation applies an explicit owner-session edit to the committed Workspace tree.
func (s *WorkspaceSyncService) ApplyFileOperation(ctx context.Context, principal identity.Principal, workspaceID string,
	command WorkspaceFileOperationCommand, idempotencyKey string) (WorkspaceFileOperationResult, error) {
	if s == nil || s.store == nil {
		return WorkspaceFileOperationResult{}, domain.NewError(domain.CodeWorkspaceUnavailable, "workspace service is unavailable")
	}
	if principal.Kind != identity.KindSession {
		return WorkspaceFileOperationResult{}, domain.NewError(domain.CodeForbidden, "Workspace file edits require a Portal session")
	}
	value, err := s.authorize(ctx, principal, workspaceID)
	if err != nil {
		return WorkspaceFileOperationResult{}, err
	}
	if value.ReadOnly {
		return WorkspaceFileOperationResult{}, domain.NewError(domain.CodeForbidden, "workspace is shared as read-only")
	}
	if !command.WriteEnabled {
		return WorkspaceFileOperationResult{}, domain.NewError(domain.CodeForbidden,
			"Workspace file editing is read-only unless this request explicitly sets write_enabled to true")
	}
	if !validWorkspaceFileOperation(command) || len(idempotencyKey) == 0 || len(idempotencyKey) > 128 {
		return WorkspaceFileOperationResult{}, domain.InvalidArgument("file_operation", "Workspace file operation is invalid")
	}
	if workspaceFileOperationTouchesSensitivePath(command) {
		return WorkspaceFileOperationResult{}, domain.NewError(domain.CodeForbidden,
			"file edits are disabled for credential paths (.ssh, .gnupg, .aws, .azure, .kube, .docker, .config/gcloud, .config/gh, .netrc, .git-credentials, and .env*); review these files on the Host")
	}
	if s.materializeCurrent == nil {
		return WorkspaceFileOperationResult{}, domain.NewError(domain.CodeWorkspaceUnavailable, "Workspace materializer is unavailable")
	}
	store, ok := s.store.(workspaceFileOperationStore)
	if !ok {
		return WorkspaceFileOperationResult{}, domain.NewError(domain.CodeWorkspaceUnavailable, "Workspace file operations are unavailable")
	}
	request, err := json.Marshal(struct {
		WorkspaceID             string
		Action                  WorkspaceFileOperationAction
		SourcePath              string
		TargetPath              string
		ExpectedCurrentRevision int64
		WriteEnabled            bool
	}{workspaceID, command.Action, command.SourcePath, command.TargetPath, command.ExpectedCurrentRevision, command.WriteEnabled})
	if err != nil {
		return WorkspaceFileOperationResult{}, domain.NewError(domain.CodeInternal, "Workspace file operation cannot be encoded")
	}
	digest := sha256.Sum256(request)
	result, err := store.ApplyWorkspaceFileOperation(ctx, principal.UserID, workspaceID, command, IdempotencyRecord{
		OwnerID: principal.UserID, Operation: "workspace.file_operation", Key: idempotencyKey,
		RequestHash: digest[:], ResourceID: workspaceID,
	})
	if err != nil {
		return WorkspaceFileOperationResult{}, err
	}
	if result.WorkspaceID != workspaceID || result.Action != command.Action || result.SourcePath != command.SourcePath ||
		result.TargetPath != command.TargetPath || result.ExpectedCurrentRevision != command.ExpectedCurrentRevision || result.Revision < 0 {
		return WorkspaceFileOperationResult{}, domain.NewError(domain.CodeWorkspaceConflict, "Workspace file operation receipt is invalid")
	}
	if err := s.materializeWorkspaceCurrent(ctx, workspaceID); err != nil {
		if ctx.Err() != nil {
			return WorkspaceFileOperationResult{}, ctx.Err()
		}
		return WorkspaceFileOperationResult{}, domain.NewError(domain.CodeWorkspaceBusy,
			"Workspace file edit is committed but materialization is pending; retry with the same idempotency key")
	}
	return result, nil
}

func validWorkspaceFileOperation(value WorkspaceFileOperationCommand) bool {
	if value.ExpectedCurrentRevision < 0 || workspace.ValidateRelativePath(value.SourcePath) != nil {
		return false
	}
	if value.Action == WorkspaceFileDelete {
		return value.TargetPath == ""
	}
	if value.Action != WorkspaceFileRename && value.Action != WorkspaceFileMove || workspace.ValidateRelativePath(value.TargetPath) != nil || value.SourcePath == value.TargetPath {
		return false
	}
	source, target := strings.ToLower(value.SourcePath), strings.ToLower(value.TargetPath)
	if source == target {
		return value.SourcePath != value.TargetPath
	}
	return !workspaceFilePathContains(source, target) && !workspaceFilePathContains(target, source)
}

func ValidateWorkspaceFileOperationMove(file workspace.File, targetPath string) error {
	if workspace.CanonicalFileKind(file.Kind) != workspace.FileKindSymlink {
		return nil
	}
	file.Path = targetPath
	if err := workspace.ValidateFile(file); err != nil {
		return domain.InvalidArgument("target_path", "Workspace move would make the symlink unsafe")
	}
	return nil
}

func workspaceFileOperationTouchesSensitivePath(value WorkspaceFileOperationCommand) bool {
	if WorkspaceFileOperationPathIsSensitive(value.SourcePath) {
		return true
	}
	return value.TargetPath != "" && WorkspaceFileOperationPathIsSensitive(value.TargetPath)
}

// WorkspaceFileOperationPathIsSensitive matches only the explicit credential
// path policy used for file operations; it does not inspect file contents.
func WorkspaceFileOperationPathIsSensitive(value string) bool {
	value = strings.ToLower(value)
	for _, protected := range []string{".ssh", ".gnupg", ".aws", ".azure", ".kube", ".docker", ".config/gcloud", ".config/gh"} {
		if value == protected || strings.HasPrefix(value, protected+"/") || strings.Contains(value, "/"+protected+"/") || strings.HasSuffix(value, "/"+protected) {
			return true
		}
	}
	base := path.Base(value)
	return base == ".netrc" || base == ".git-credentials" || base == ".env" || strings.HasPrefix(base, ".env.")
}

func workspaceFilePathContains(parent, child string) bool {
	return parent == child || strings.HasPrefix(child, parent+"/")
}
