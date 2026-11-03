package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

type hostWorkspaceBindingLister interface {
	GetWorkspaceBindingsByHost(context.Context, string) ([]workspace.Binding, error)
}

// ListHostWorkspaceBindings returns only bindings stored for the authenticated Host.
// The caller must authorize the Host cookie before invoking this method.
func (s *WorkspaceSyncService) ListHostWorkspaceBindings(ctx context.Context, actorID, hostID string) ([]workspace.Binding, error) {
	if s == nil || s.store == nil {
		return nil, domain.NewError(domain.CodeWorkspaceUnavailable, "workspace service is unavailable")
	}
	if !domain.ValidUserID(actorID) {
		return nil, domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	if !domain.ValidHostID(hostID) {
		return nil, domain.NewError(domain.CodeNotFound, "host not found")
	}
	store, ok := s.store.(hostWorkspaceBindingLister)
	if !ok {
		return nil, domain.NewError(domain.CodeWorkspaceUnavailable, "workspace service is unavailable")
	}
	bindings, err := store.GetWorkspaceBindingsByHost(ctx, hostID)
	if err != nil {
		return nil, hostWorkspaceReadFailure(err)
	}
	if bindings == nil {
		bindings = []workspace.Binding{}
	}
	for i := range bindings {
		binding := &bindings[i]
		if binding.HostID != hostID || !domain.ValidUserID(binding.OwnerID) ||
			!domain.ValidWorkspaceID(binding.Workspace.ID) || binding.Workspace.OwnerID != binding.OwnerID {
			return nil, domain.NewError(domain.CodeInternal, "workspace binding ownership is invalid")
		}
		binding.SyncConflicts = append([]string{}, binding.SyncConflicts...)
	}
	return bindings, nil
}

// GetHostWorkspaceManifest reads a page from the current applied Host revision.
// The caller must authorize the Host cookie and workspace:file:read permission.
func (s *WorkspaceSyncService) GetHostWorkspaceManifest(
	ctx context.Context, actorID, hostID, workspaceID string, revision int64, prefix, cursor string, limit int32,
) (workspace.ManifestPage, error) {
	if revision < 0 || len(prefix) > 1024 || len(cursor) > 8192 {
		return workspace.ManifestPage{}, domain.InvalidArgument("manifest", "manifest revision, prefix, or cursor is invalid")
	}
	if prefix != "" {
		prefix = strings.TrimSuffix(prefix, "/")
		if err := workspace.ValidateRelativePath(prefix); err != nil {
			return workspace.ManifestPage{}, domain.InvalidArgument("prefix", "manifest prefix must be a normalized relative path")
		}
	}
	if limit == 0 {
		limit = manifestDefaultLimit
	}
	if limit < 1 || limit > manifestMaxLimit {
		return workspace.ManifestPage{}, domain.InvalidArgument("limit", "manifest page limit is out of range")
	}

	var after string
	if cursor != "" {
		parsed, err := workspace.DecodeManifestCursor(cursor)
		if err != nil {
			return workspace.ManifestPage{}, domain.InvalidArgument("cursor", "manifest cursor is invalid")
		}
		if parsed.Prefix != prefix || parsed.Revision != revision {
			return workspace.ManifestPage{}, domain.InvalidArgument("cursor", "manifest cursor does not match the requested snapshot")
		}
		after = parsed.After
	}
	binding, appliedRevision, err := s.hostWorkspaceReadBinding(ctx, actorID, hostID, workspaceID)
	if err != nil {
		return workspace.ManifestPage{}, err
	}
	if revision != appliedRevision {
		return workspace.ManifestPage{}, domain.NewError(domain.CodeWorkspaceConflict, "requested revision is not the current applied Host revision")
	}
	page, err := s.store.GetWorkspaceManifestPage(ctx, workspaceID, revision, prefix, after, limit)
	if err != nil {
		return workspace.ManifestPage{}, hostWorkspaceManifestReadFailure(err)
	}
	if page.WorkspaceID != workspaceID || page.Revision != revision {
		return workspace.ManifestPage{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace manifest does not match the current Host revision")
	}
	if page.Files == nil {
		page.Files = []workspace.File{}
	}
	if page.EmptyDirectories == nil {
		page.EmptyDirectories = []string{}
	}
	previous := after
	for _, file := range page.Files {
		if workspace.ValidateFile(file) != nil || (prefix != "" && file.Path != prefix && !strings.HasPrefix(file.Path, prefix+"/")) || file.Path <= previous {
			return workspace.ManifestPage{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace manifest contains an invalid file entry")
		}
		previous = file.Path
	}
	for _, directory := range page.EmptyDirectories {
		if workspace.ValidateRelativePath(directory) != nil || (prefix != "" && directory != prefix && !strings.HasPrefix(directory, prefix+"/")) {
			return workspace.ManifestPage{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace manifest contains an invalid directory entry")
		}
	}
	if page.NextCursor != nil {
		parsed, err := workspace.DecodeManifestCursor(*page.NextCursor)
		if err != nil || parsed.Revision != revision || parsed.Prefix != prefix || parsed.After != previous || len(page.Files) == 0 {
			return workspace.ManifestPage{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace manifest returned an invalid cursor")
		}
	}
	// A binding can change while the manifest page is being fetched.
	current, _, err := s.hostWorkspaceReadBinding(ctx, actorID, hostID, workspaceID)
	if err != nil {
		return workspace.ManifestPage{}, err
	}
	if !sameHostWorkspaceReadBinding(binding, current) {
		return workspace.ManifestPage{}, domain.NewError(domain.CodeWorkspaceBusy, "Host workspace binding changed while reading the manifest")
	}
	return page, nil
}

// StreamHostWorkspaceFile validates the complete immutable file before writing
// any bytes, then rechecks the binding and caller authorization at each write.
func (s *WorkspaceSyncService) StreamHostWorkspaceFile(
	ctx context.Context, actorID, hostID, workspaceID string, revision int64, relativePath string,
	write func(workspace.File, []byte) error,
) (workspace.File, error) {
	if write == nil {
		return workspace.File{}, domain.InvalidArgument("stream", "workspace file stream is required")
	}
	if revision < 0 || workspace.ValidateRelativePath(relativePath) != nil {
		return workspace.File{}, domain.InvalidArgument("path", "workspace file path or revision is invalid")
	}
	binding, appliedRevision, err := s.hostWorkspaceReadBinding(ctx, actorID, hostID, workspaceID)
	if err != nil {
		return workspace.File{}, err
	}
	if revision != appliedRevision {
		return workspace.File{}, domain.NewError(domain.CodeWorkspaceConflict, "requested revision is not the current applied Host revision")
	}
	page, err := s.store.GetWorkspaceManifestPage(ctx, workspaceID, revision, relativePath, "", 1)
	if err != nil {
		return workspace.File{}, hostWorkspaceManifestReadFailure(err)
	}
	if page.WorkspaceID != workspaceID || page.Revision != revision {
		return workspace.File{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace manifest does not match the current Host revision")
	}
	if len(page.Files) == 0 || page.Files[0].Path != relativePath {
		return workspace.File{}, domain.NewError(domain.CodeNotFound, "workspace file not found")
	}
	file := page.Files[0]
	if file.SizeBytes > workspace.MaxSyncFileBytes || workspace.ValidateFile(file) != nil {
		return workspace.File{}, domain.NewError(domain.CodeWorkspaceConflict, "workspace file manifest is invalid")
	}
	if file.Kind == "symlink" {
		return workspace.File{}, domain.NewError(domain.CodeForbidden, "workspace symlinks cannot be streamed as regular file contents")
	}
	if err := verifyHostWorkspaceFile(ctx, s.blocks, workspaceID, file); err != nil {
		return workspace.File{}, err
	}
	current, _, err := s.hostWorkspaceReadBinding(ctx, actorID, hostID, workspaceID)
	if err != nil {
		return workspace.File{}, err
	}
	if !sameHostWorkspaceReadBinding(binding, current) {
		return workspace.File{}, domain.NewError(domain.CodeWorkspaceBusy, "Host workspace binding changed while validating the file")
	}
	if len(file.Chunks) == 0 {
		if err := write(file, []byte{}); err != nil {
			return workspace.File{}, err
		}
		return file, nil
	}
	for _, chunk := range file.Chunks {
		data, err := readVerifiedBlock(s.blocks, workspaceID, chunk)
		if err != nil {
			return workspace.File{}, hostWorkspaceBlockReadFailure(err)
		}
		current, _, err := s.hostWorkspaceReadBinding(ctx, actorID, hostID, workspaceID)
		if err != nil {
			return workspace.File{}, err
		}
		if !sameHostWorkspaceReadBinding(binding, current) {
			return workspace.File{}, domain.NewError(domain.CodeWorkspaceBusy, "Host workspace binding changed while streaming the file")
		}
		if err := write(file, data); err != nil {
			return workspace.File{}, err
		}
	}
	return file, nil
}

func (s *WorkspaceSyncService) hostWorkspaceReadBinding(ctx context.Context, actorID, hostID, workspaceID string) (workspace.Binding, int64, error) {
	if s == nil || s.store == nil || s.blocks == nil {
		return workspace.Binding{}, 0, domain.NewError(domain.CodeWorkspaceUnavailable, "workspace service is unavailable")
	}
	if !domain.ValidUserID(actorID) {
		return workspace.Binding{}, 0, domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	if !domain.ValidHostID(hostID) || !domain.ValidWorkspaceID(workspaceID) {
		return workspace.Binding{}, 0, domain.NewError(domain.CodeNotFound, "workspace binding not found")
	}
	binding, err := s.store.GetWorkspaceBinding(ctx, hostID, workspaceID)
	if err != nil {
		return workspace.Binding{}, 0, hostWorkspaceReadFailure(err)
	}
	if binding.HostID != hostID || binding.Workspace.ID != workspaceID || !domain.ValidUserID(binding.OwnerID) ||
		binding.Workspace.OwnerID != binding.OwnerID {
		return workspace.Binding{}, 0, domain.NewError(domain.CodeNotFound, "workspace binding not found")
	}
	if binding.Workspace.State != workspace.StateReady || binding.Workspace.DeletedAt != nil {
		return workspace.Binding{}, 0, domain.NewError(domain.CodeWorkspaceBusy, "workspace is not ready for Host file reads")
	}
	if binding.State == workspace.BindingFailed {
		return workspace.Binding{}, 0, domain.NewError(domain.CodeWorkspaceConflict, "Host workspace binding has failed")
	}
	if binding.State != workspace.BindingActive {
		return workspace.Binding{}, 0, domain.NewError(domain.CodeWorkspaceBusy, "Host workspace binding is not active")
	}
	if binding.SyncState == workspace.BindingSyncConflict || len(binding.SyncConflicts) > 0 {
		return workspace.Binding{}, 0, domain.NewError(domain.CodeWorkspaceConflict, "Host workspace has unresolved sync conflicts")
	}
	if binding.SyncState != workspace.BindingSyncHealthy {
		return workspace.Binding{}, 0, domain.NewError(domain.CodeWorkspaceBusy, "Host workspace sync state is unavailable")
	}
	if binding.Workspace.Revision < 0 || binding.DesiredRevision < 0 || binding.AppliedRevision < 0 ||
		binding.DesiredRevision != binding.Workspace.Revision || binding.AppliedRevision != binding.DesiredRevision {
		return workspace.Binding{}, 0, domain.NewError(domain.CodeWorkspaceBusy, "Host workspace revision is not fully applied")
	}
	return binding, binding.AppliedRevision, nil
}

func sameHostWorkspaceReadBinding(expected, current workspace.Binding) bool {
	return expected.ID == current.ID && expected.HostID == current.HostID && expected.OwnerID == current.OwnerID &&
		expected.Workspace.ID == current.Workspace.ID && expected.Workspace.OwnerID == current.Workspace.OwnerID &&
		expected.Workspace.State == current.Workspace.State && expected.Workspace.Revision == current.Workspace.Revision &&
		expected.BindingVersion == current.BindingVersion && expected.Directory == current.Directory &&
		expected.State == current.State && expected.SyncState == current.SyncState &&
		expected.DesiredRevision == current.DesiredRevision && expected.AppliedRevision == current.AppliedRevision &&
		len(current.SyncConflicts) == 0
}

func verifyHostWorkspaceFile(ctx context.Context, blocks WorkspaceBlockReader, workspaceID string, file workspace.File) error {
	hasher := sha256.New()
	var total int64
	for _, chunk := range file.Chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := readVerifiedBlock(blocks, workspaceID, chunk)
		if err != nil {
			return hostWorkspaceBlockReadFailure(err)
		}
		if total > file.SizeBytes-int64(len(data)) {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace file chunks exceed the manifest size")
		}
		_, _ = hasher.Write(data)
		total += int64(len(data))
	}
	if total != file.SizeBytes || hex.EncodeToString(hasher.Sum(nil)) != file.SHA256 {
		return domain.NewError(domain.CodeWorkspaceConflict, "workspace file digest does not match its manifest")
	}
	return nil
}

func hostWorkspaceReadFailure(err error) error {
	if err == nil || domain.CodeOf(err) != domain.CodeInternal {
		return err
	}
	unavailable := domain.NewError(domain.CodeWorkspaceUnavailable, "workspace service is unavailable")
	unavailable.Err = err
	return unavailable
}

func hostWorkspaceManifestReadFailure(err error) error {
	if domain.CodeOf(err) == domain.CodeNotFound {
		unavailable := domain.NewError(domain.CodeWorkspaceUnavailable, "workspace snapshot is unavailable")
		unavailable.Err = err
		return unavailable
	}
	return hostWorkspaceReadFailure(err)
}

func hostWorkspaceBlockReadFailure(err error) error {
	if err == nil {
		return nil
	}
	if domain.CodeOf(err) == domain.CodeNotFound {
		unavailable := domain.NewError(domain.CodeWorkspaceUnavailable, "workspace file content is unavailable")
		unavailable.Err = err
		return unavailable
	}
	return hostWorkspaceReadFailure(err)
}
