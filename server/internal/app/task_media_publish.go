package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

const taskMediaArtifactPrefix = "morde-media-"

func taskMediaSaveStepID(operationID string) string {
	if !domain.ValidOperationID(operationID) {
		return ""
	}
	return "s" + operationID[1:]
}

type taskMediaBlockLocation struct {
	offset int64
	size   int64
}

func (e *RuntimeToolExecutor) SaveApprovedTaskMedia(ctx context.Context, claimed task.Task, pending task.PendingRequest, stepID string) (result TaskMediaSavedArtifact, retErr error) {
	if ctx != nil && e != nil && claimed.ProjectID != "" {
		guard, ok := e.store.(projectTaskWorkspaceGuard)
		if !ok {
			return TaskMediaSavedArtifact{}, domain.NewError(domain.CodeForbidden, "project Workspace authorization is unavailable")
		}
		err := guard.WithProjectTaskWorkspaceAccess(ctx, claimed, true, func(guardedCtx context.Context) error {
			var err error
			result, err = e.saveApprovedTaskMedia(guardedCtx, claimed, pending, stepID)
			return err
		})
		return result, err
	}
	return e.saveApprovedTaskMedia(ctx, claimed, pending, stepID)
}

func (e *RuntimeToolExecutor) saveApprovedTaskMedia(ctx context.Context, claimed task.Task, pending task.PendingRequest, stepID string) (result TaskMediaSavedArtifact, retErr error) {
	if ctx == nil || e == nil || e.mediaSave == nil || e.mediaStore == nil || !domain.ValidTaskID(claimed.ID) ||
		!domain.ValidUserID(claimed.OwnerID) || !domain.ValidPendingRequestID(pending.ID) || !domain.ValidOperationID(pending.MediaPreviewOperationID) ||
		!domain.ValidStepID(stepID) {
		return TaskMediaSavedArtifact{}, domain.NewError(domain.CodeInvalidState, "approved Task media save is unavailable")
	}
	details, err := e.store.GetTask(ctx, claimed.ID, claimed.OwnerID)
	if err != nil {
		return TaskMediaSavedArtifact{}, err
	}
	current := details.Task
	if current.ID != claimed.ID || current.OwnerID != claimed.OwnerID || current.InitiatorID != claimed.InitiatorID ||
		current.HostID != claimed.HostID || current.Status != task.StatusRunning || current.Lease.Owner != claimed.Lease.Owner ||
		current.Lease.Version != claimed.Lease.Version || current.Lease.ExpiresAt == nil || !current.Lease.ExpiresAt.After(time.Now()) ||
		current.WorkspaceID == "" || current.InputRevision == nil || current.WorkspaceCheckpointRevision == nil || len(current.ContextDigest) != sha256.Size {
		return TaskMediaSavedArtifact{}, domain.NewError(domain.CodeInvalidState, "Task lease or Workspace binding changed before media save")
	}
	storedPending := details.PendingRequest
	if storedPending == nil || storedPending.TaskID != pending.TaskID || storedPending.ID != pending.ID || storedPending.Version != pending.Version ||
		storedPending.CallID != pending.CallID || storedPending.ToolName != pending.ToolName ||
		storedPending.MediaPreviewOperationID != pending.MediaPreviewOperationID || !bytes.Equal(storedPending.RequestHash, pending.RequestHash) ||
		storedPending.Status != task.PendingRequestApproved || storedPending.Decision != task.PendingDecisionApprove {
		return TaskMediaSavedArtifact{}, domain.NewError(domain.CodeConflict, "approved Task media request changed")
	}
	if err := e.authorize(ctx, current, identity.ScopeComputerOperate); err != nil {
		return TaskMediaSavedArtifact{}, err
	}
	step, found := taskStepByID(details.Steps, stepID)
	if !found || step.TaskID != current.ID || step.Status != task.StepRunning && step.Status != task.StepSucceeded {
		return TaskMediaSavedArtifact{}, domain.NewError(domain.CodeInvalidState, "Task media save step is not active")
	}
	operationID := pending.MediaPreviewOperationID
	artifactPathPrefix := taskMediaArtifactPrefix + operationID + "."
	artifact, artifactFound, err := taskMediaArtifactForStep(details.Artifacts, current, stepID, artifactPathPrefix)
	if err != nil {
		return TaskMediaSavedArtifact{}, err
	}
	if artifactFound {
		if !taskMediaPreviewTypeAllowed(artifact.MediaType) || artifact.SizeBytes < 1 || !validSyncDigest(artifact.SHA256) {
			return TaskMediaSavedArtifact{}, domain.NewError(domain.CodeInvalidState, "saved Task media Artifact metadata is invalid")
		}
		wantPath, pathErr := taskMediaArtifactPath(operationID, artifact.MediaType)
		if pathErr != nil || artifact.Path != wantPath {
			return TaskMediaSavedArtifact{}, domain.NewError(domain.CodeInvalidState, "saved Task media Artifact path is invalid")
		}
		if _, err := e.workspaces.VerifyCommittedArtifact(ctx, TaskArtifactProof{
			OwnerID: taskWorkspaceOwner(current), WorkspaceID: current.WorkspaceID, Revision: artifact.Revision,
			Path: artifact.Path, SizeBytes: artifact.SizeBytes, SHA256: artifact.SHA256,
		}); err != nil {
			return TaskMediaSavedArtifact{}, err
		}
		if err := e.markTaskMediaPublished(ctx, current, operationID); err != nil {
			return TaskMediaSavedArtifact{}, err
		}
		return taskMediaSavedArtifact(artifact), nil
	}
	if step.Status == task.StepSucceeded {
		return TaskMediaSavedArtifact{}, domain.NewError(domain.CodeInvalidState, "successful Task media save step has no Artifact")
	}
	staged, err := e.mediaStore.GetTaskMediaOperationDetails(ctx, current.ID, operationID)
	if err != nil {
		return TaskMediaSavedArtifact{}, err
	}
	if !taskMediaApprovalBindingMatches(current, pending, staged) ||
		staged.OperationStatus != string(task.OperationSucceeded) || staged.TokenState != "settled" || staged.InputTokens == nil || staged.OutputTokens == nil ||
		staged.ReservedTokens <= 0 || *staged.InputTokens < 0 || *staged.OutputTokens < 0 ||
		*staged.InputTokens > staged.ReservedTokens || *staged.OutputTokens > staged.ReservedTokens-*staged.InputTokens ||
		!taskMediaPreviewTypeAllowed(staged.MediaType) || staged.SizeBytes < 1 || staged.SizeBytes > workspace.MaxSyncFileBytes || len(staged.SHA256) != sha256.Size {
		return TaskMediaSavedArtifact{}, domain.NewError(domain.CodeInvalidState, "Task media is not ready for Workspace publication")
	}
	artifactPath, err := taskMediaArtifactPath(operationID, staged.MediaType)
	if err != nil {
		return TaskMediaSavedArtifact{}, err
	}
	operationKey := taskWorkspacePublishOperationKey(stepID, current.WorkspaceID)
	if operationKey == "" {
		return TaskMediaSavedArtifact{}, domain.NewError(domain.CodeInvalidState, "Task media publish identity is invalid")
	}
	if receipt, receiptErr := e.workspaces.FindTaskWorkspacePublishReceipt(ctx, current.OwnerID, current.ID, current.WorkspaceID, operationKey); receiptErr == nil {
		switch receipt.State {
		case TaskWorkspacePublishSucceeded:
			if staged.State != TaskMediaPreviewSaving && staged.State != TaskMediaPreviewSaved {
				return TaskMediaSavedArtifact{}, domain.NewError(domain.CodeInvalidState, "successful Task media publish has no matching staged state")
			}
			return e.recordTaskMediaArtifact(ctx, current, stepID, staged, artifactPath, receipt)
		case TaskWorkspacePublishConflict:
			return TaskMediaSavedArtifact{}, e.discardTaskMediaAfterPublishFailure(ctx, current.ID, operationID,
				domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed after this Task checkpoint was confirmed"))
		case TaskWorkspacePublishQuotaRejected:
			return TaskMediaSavedArtifact{}, e.discardTaskMediaAfterPublishFailure(ctx, current.ID, operationID,
				domain.NewError(domain.CodeQuotaExceeded, "Workspace storage quota would be exceeded"))
		case TaskWorkspacePublishPending:
			if staged.State != TaskMediaPreviewSaving || staged.CleanupState != TaskMediaCleanupPending {
				return TaskMediaSavedArtifact{}, domain.NewError(domain.CodeInvalidState, "pending Task media publish has no recoverable staged content")
			}
			if receipt.BaseRevision != *current.WorkspaceCheckpointRevision {
				return TaskMediaSavedArtifact{}, e.discardTaskMediaAfterPublishFailure(ctx, current.ID, operationID,
					domain.NewError(domain.CodeWorkspaceConflict, "Task Workspace checkpoint changed during media save recovery"))
			}
		default:
			return TaskMediaSavedArtifact{}, domain.NewError(domain.CodeInternal, "Task media Workspace receipt state is invalid")
		}
	} else if domain.CodeOf(receiptErr) != domain.CodeNotFound {
		return TaskMediaSavedArtifact{}, receiptErr
	} else if staged.State != TaskMediaPreviewSaving || staged.CleanupState != TaskMediaCleanupPending {
		return TaskMediaSavedArtifact{}, domain.NewError(domain.CodeInvalidState, "Task media is not ready for Workspace publication")
	}
	media, err := e.mediaSave.OpenApproved(ctx, current.OwnerID, current.ID, pending.ID, pending.Version, operationID)
	if err != nil {
		return TaskMediaSavedArtifact{}, err
	}
	if media.Body == nil || media.MediaType != staged.MediaType || media.SizeBytes != staged.SizeBytes || !bytes.Equal(media.SHA256, staged.SHA256) {
		if media.Body != nil {
			_ = media.Body.Close()
		}
		return TaskMediaSavedArtifact{}, domain.NewError(domain.CodeInvalidState, "approved Task media metadata changed")
	}
	defer func() {
		if closeErr := media.Body.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close approved Task media content: %w", closeErr))
		}
	}()

	expectedFence := AgentHostFence{RuntimeGeneration: pending.RuntimeGeneration, RuntimeInstanceID: pending.RuntimeInstanceID,
		ControlVersion: pending.ControlVersion, AIAllowed: true}
	fence, err := e.fences.ReadTaskFence(ctx, current.InitiatorID, current.HostID)
	if err != nil || !sameAgentControlFence(fence, expectedFence) || !fence.AIAllowed || fence.RuntimeGeneration < 1 || fence.RuntimeInstanceID == "" {
		return TaskMediaSavedArtifact{}, domain.NewError(domain.CodeInvalidState, "Host control fence changed before Task media save")
	}
	session, err := e.connect(ctx, current, fence)
	if err != nil {
		return TaskMediaSavedArtifact{}, err
	}
	defer session.Close()
	bindingVersion, viewID, err := session.SetTaskWorkspaceView(ctx, current, current.WorkspaceID, 0)
	if err != nil {
		return TaskMediaSavedArtifact{}, err
	}
	baseRevision := *current.WorkspaceCheckpointRevision
	files, emptyDirectories, directories, err := e.workspaces.readManifest(ctx, taskWorkspaceOwner(current), current.WorkspaceID, baseRevision, nil)
	if err != nil {
		return TaskMediaSavedArtifact{}, err
	}
	if taskMediaWorkspacePathExists(artifactPath, files, emptyDirectories, directories) {
		return TaskMediaSavedArtifact{}, domain.NewError(domain.CodeWorkspaceConflict, "generated media path already exists in the Task Workspace")
	}
	temp, err := os.CreateTemp("", "morde-task-media-*")
	if err != nil {
		return TaskMediaSavedArtifact{}, fmt.Errorf("create private Task media publish spool: %w", err)
	}
	tempPath := temp.Name()
	defer func() {
		if closeErr := temp.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close private Task media publish spool: %w", closeErr))
		}
		if removeErr := os.Remove(tempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			retErr = errors.Join(retErr, fmt.Errorf("remove private Task media publish spool: %w", removeErr))
		}
	}()
	mediaFile, locations, digest, err := writeTaskMediaChunks(ctx, temp, media.Body, media.SizeBytes, media.SHA256)
	if err != nil {
		return TaskMediaSavedArtifact{}, err
	}
	mediaFile.Path = artifactPath
	files = append(files, mediaFile)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	sort.Strings(emptyDirectories)
	manifestSHA, err := workspace.ManifestTreeWithMetadataSHA256(files, emptyDirectories, directories)
	if err != nil {
		return TaskMediaSavedArtifact{}, domain.NewError(domain.CodeWorkspaceConflict, "Task media Workspace manifest is invalid")
	}
	inputManifestSHA := hex.EncodeToString(current.ContextDigest)
	requestBody, err := json.Marshal(struct {
		OwnerID, TaskID, WorkspaceID, StepID, OperationID, PendingRequestID, Path, MediaType, SHA256, ViewID, InputManifestSHA256 string
		PendingRequestVersion, BindingVersion, InputRevision, BaseRevision, SizeBytes                                             int64
	}{
		OwnerID: current.OwnerID, TaskID: current.ID, WorkspaceID: current.WorkspaceID, StepID: stepID, OperationID: operationID,
		PendingRequestID: pending.ID, Path: artifactPath, MediaType: media.MediaType, SHA256: digest, ViewID: viewID,
		InputManifestSHA256: inputManifestSHA, PendingRequestVersion: pending.Version, BindingVersion: bindingVersion,
		InputRevision: *current.InputRevision, BaseRevision: baseRevision, SizeBytes: media.SizeBytes,
	})
	if err != nil {
		return TaskMediaSavedArtifact{}, fmt.Errorf("encode Task media Workspace publish identity: %w", err)
	}
	requestHash := sha256.Sum256(requestBody)
	command := TaskWorkspacePublishCommand{
		OwnerID: current.OwnerID, TaskID: current.ID, WorkspaceID: current.WorkspaceID, HostID: current.HostID,
		ProjectID: current.ProjectID, InitiatorID: current.InitiatorID, WorkspaceOwnerID: taskWorkspaceOwner(current),
		LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version, BindingVersion: bindingVersion,
		InputRevision: *current.InputRevision, BaseRevision: baseRevision, HostFence: fence, ViewID: viewID,
		InputManifestSHA256: inputManifestSHA, OperationKey: operationKey, RequestHash: requestHash[:],
	}
	receipt, err := e.workspaces.ReserveTaskWorkspacePublish(ctx, command)
	if err != nil {
		return TaskMediaSavedArtifact{}, err
	}
	switch receipt.State {
	case TaskWorkspacePublishSucceeded:
		return e.recordTaskMediaArtifact(ctx, current, stepID, staged, artifactPath, receipt)
	case TaskWorkspacePublishConflict:
		return TaskMediaSavedArtifact{}, e.discardTaskMediaAfterPublishFailure(ctx, current.ID, operationID,
			domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed after this Task checkpoint was confirmed"))
	case TaskWorkspacePublishQuotaRejected:
		return TaskMediaSavedArtifact{}, e.discardTaskMediaAfterPublishFailure(ctx, current.ID, operationID,
			domain.NewError(domain.CodeQuotaExceeded, "Workspace storage quota would be exceeded"))
	case TaskWorkspacePublishPending:
	default:
		return TaskMediaSavedArtifact{}, domain.NewError(domain.CodeInternal, "Task media Workspace reservation state is invalid")
	}
	receipt, err = e.workspaces.CommitTaskWorkspacePublish(ctx, command, files, emptyDirectories, directories, manifestSHA,
		func(_ context.Context, blockSHA string) ([]byte, error) {
			location, ok := locations[blockSHA]
			if !ok {
				return nil, domain.NewError(domain.CodeWorkspaceConflict, "Task media Workspace block is unavailable")
			}
			block := make([]byte, location.size)
			if _, err := temp.ReadAt(block, location.offset); err != nil {
				return nil, fmt.Errorf("read private Task media Workspace block: %w", err)
			}
			return block, nil
		})
	if err != nil {
		if domain.CodeOf(err) == domain.CodeWorkspaceConflict || domain.CodeOf(err) == domain.CodeQuotaExceeded {
			err = e.discardTaskMediaAfterPublishFailure(ctx, current.ID, operationID, err)
		}
		return TaskMediaSavedArtifact{}, err
	}
	return e.recordTaskMediaArtifact(ctx, current, stepID, staged, artifactPath, receipt)
}

func taskMediaApprovalBindingMatches(current task.Task, pending task.PendingRequest, staged TaskMediaStagedObject) bool {
	return pending.TaskID == current.ID && staged.TaskID == current.ID && staged.OwnerID == current.OwnerID &&
		staged.InitiatorID == current.InitiatorID && staged.OperationID == pending.MediaPreviewOperationID &&
		len(pending.RequestHash) == sha256.Size && staged.OperationKey == agentOperationKey(pending.CallID) &&
		bytes.Equal(staged.RequestHash, pending.RequestHash)
}

func (e *RuntimeToolExecutor) recordTaskMediaArtifact(ctx context.Context, current task.Task, stepID string, staged TaskMediaStagedObject,
	artifactPath string, receipt TaskWorkspacePublishReceipt) (TaskMediaSavedArtifact, error) {
	if receipt.State != TaskWorkspacePublishSucceeded || receipt.WorkspaceID != current.WorkspaceID || receipt.PublishedRevision < 0 ||
		!validSyncDigest(receipt.ManifestSHA256) {
		return TaskMediaSavedArtifact{}, domain.NewError(domain.CodeInternal, "Task media Workspace publish receipt is invalid")
	}
	proof, err := e.workspaces.VerifyCommittedArtifact(ctx, TaskArtifactProof{
		OwnerID: taskWorkspaceOwner(current), WorkspaceID: current.WorkspaceID, Revision: receipt.PublishedRevision,
		Path: artifactPath, SizeBytes: staged.SizeBytes, SHA256: hex.EncodeToString(staged.SHA256),
	})
	if err != nil {
		return TaskMediaSavedArtifact{}, err
	}
	artifactID, err := domain.NewArtifactID()
	if err != nil {
		return TaskMediaSavedArtifact{}, err
	}
	artifact := task.Artifact{ID: artifactID, TaskID: current.ID, StepID: stepID, WorkspaceID: current.WorkspaceID,
		Revision: proof.Revision, SizeBytes: staged.SizeBytes, SHA256: hex.EncodeToString(staged.SHA256),
		Label: path.Base(artifactPath), Path: artifactPath, MediaType: staged.MediaType}
	stored, err := e.store.RecordValidatedArtifact(ctx, current.OwnerID, artifact, current.Lease.Owner, current.Lease.Version)
	if err != nil {
		return TaskMediaSavedArtifact{}, err
	}
	if stored.TaskID != artifact.TaskID || stored.StepID != artifact.StepID || stored.WorkspaceID != artifact.WorkspaceID ||
		stored.Revision != proof.Revision || stored.Path != artifact.Path || stored.SizeBytes != artifact.SizeBytes ||
		stored.SHA256 != artifact.SHA256 || stored.MediaType != artifact.MediaType {
		return TaskMediaSavedArtifact{}, domain.NewError(domain.CodeInvalidState, "recorded Task media Artifact does not match its Workspace proof")
	}
	if err := e.markTaskMediaPublished(ctx, current, staged.OperationID); err != nil {
		return TaskMediaSavedArtifact{}, err
	}
	return taskMediaSavedArtifact(stored), nil
}

func (e *RuntimeToolExecutor) markTaskMediaPublished(ctx context.Context, current task.Task, operationID string) error {
	if e.mediaStore == nil {
		return domain.NewError(domain.CodeInvalidState, "Task media staged-object store is unavailable")
	}
	staged, err := e.mediaStore.GetTaskMediaOperationDetails(ctx, current.ID, operationID)
	if domain.CodeOf(err) == domain.CodeNotFound {
		return nil // Cleanup may remove staged metadata after the Artifact is durable.
	}
	if err != nil {
		return err
	}
	if staged.TaskID != current.ID || staged.OwnerID != current.OwnerID || staged.OperationID != operationID {
		return domain.NewError(domain.CodeInvalidState, "Task media staged-object identity changed")
	}
	if staged.State == TaskMediaPreviewSaved {
		return nil
	}
	if staged.State != TaskMediaPreviewSaving || staged.CleanupState != TaskMediaCleanupPending {
		return domain.NewError(domain.CodeInvalidState, "Task media is not in the saving state")
	}
	updated, err := e.mediaStore.SetTaskMediaStagedObjectState(ctx, current.ID, operationID, TaskMediaPreviewSaving, TaskMediaPreviewSaved)
	if err == nil && updated.TaskID == current.ID && updated.OwnerID == current.OwnerID && updated.OperationID == operationID && updated.State == TaskMediaPreviewSaved {
		return nil
	}
	latest, readErr := e.mediaStore.GetTaskMediaOperationDetails(ctx, current.ID, operationID)
	if readErr == nil && latest.OwnerID == current.OwnerID && latest.State == TaskMediaPreviewSaved {
		return nil
	}
	return errors.Join(err, readErr, domain.NewError(domain.CodeConflict, "Task media save state was not committed"))
}

func (e *RuntimeToolExecutor) discardTaskMediaAfterPublishFailure(ctx context.Context, taskID, operationID string, cause error) error {
	if e.mediaStore == nil {
		return cause
	}
	_, err := e.mediaStore.SetTaskMediaStagedObjectState(ctx, taskID, operationID, TaskMediaPreviewSaving, TaskMediaPreviewDiscard)
	if err != nil && domain.CodeOf(err) != domain.CodeConflict {
		return errors.Join(cause, err)
	}
	return cause
}

func taskMediaArtifactForStep(artifacts []task.Artifact, current task.Task, stepID, pathPrefix string) (task.Artifact, bool, error) {
	var result task.Artifact
	for _, artifact := range artifacts {
		if artifact.TaskID != current.ID || artifact.StepID != stepID {
			continue
		}
		if artifact.WorkspaceID != current.WorkspaceID || !strings.HasPrefix(artifact.Path, pathPrefix) {
			return task.Artifact{}, false, domain.NewError(domain.CodeInvalidState, "Task media Artifact does not match its save step")
		}
		if result.ID != "" {
			return task.Artifact{}, false, domain.NewError(domain.CodeInvalidState, "Task media save step has multiple Artifacts")
		}
		result = artifact
	}
	return result, result.ID != "", nil
}

func taskStepByID(steps []task.Step, stepID string) (task.Step, bool) {
	for _, step := range steps {
		if step.ID == stepID {
			return step, true
		}
	}
	return task.Step{}, false
}

func taskMediaSavedArtifact(value task.Artifact) TaskMediaSavedArtifact {
	return TaskMediaSavedArtifact{Path: value.Path, Revision: value.Revision, MediaType: value.MediaType, SizeBytes: value.SizeBytes, SHA256: value.SHA256}
}

func taskMediaArtifactPath(operationID, mediaType string) (string, error) {
	if !domain.ValidOperationID(operationID) {
		return "", domain.InvalidArgument("task_media_save", "Task media operation is invalid")
	}
	extension := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "video/mp4": ".mp4",
		"audio/mpeg": ".mp3", "audio/ogg": ".ogg", "audio/opus": ".opus", "audio/wav": ".wav", "audio/aac": ".aac"}[mediaType]
	if extension == "" {
		return "", domain.InvalidArgument("task_media_save", "Task media type is unsupported")
	}
	return taskMediaArtifactPrefix + operationID + extension, nil
}

func taskMediaWorkspacePathExists(filePath string, files []workspace.File, emptyDirectories []string, directories []workspace.Directory) bool {
	want := strings.ToLower(filePath)
	for _, file := range files {
		if strings.ToLower(file.Path) == want {
			return true
		}
	}
	for _, directory := range emptyDirectories {
		if strings.ToLower(directory) == want {
			return true
		}
	}
	for _, directory := range directories {
		if directory.Path != "." && strings.ToLower(directory.Path) == want {
			return true
		}
	}
	return false
}

func writeTaskMediaChunks(ctx context.Context, dst *os.File, src io.Reader, size int64, expectedSHA []byte) (workspace.File, map[string]taskMediaBlockLocation, string, error) {
	if ctx == nil || dst == nil || src == nil || size < 1 || size > workspace.MaxSyncFileBytes || len(expectedSHA) != sha256.Size {
		return workspace.File{}, nil, "", domain.NewError(domain.CodeInvalidState, "Task media content metadata is invalid")
	}
	locations := make(map[string]taskMediaBlockLocation)
	chunks := make([]workspace.Chunk, 0, (size+workspace.MaxSyncBlockBytes-1)/workspace.MaxSyncBlockBytes)
	contentHash := sha256.New()
	var offset int64
	for offset < size {
		if err := ctx.Err(); err != nil {
			return workspace.File{}, nil, "", err
		}
		chunkSize := min(size-offset, workspace.MaxSyncBlockBytes)
		data := make([]byte, int(chunkSize))
		if _, err := io.ReadFull(src, data); err != nil {
			return workspace.File{}, nil, "", fmt.Errorf("read verified Task media content: %w", err)
		}
		if _, err := dst.Write(data); err != nil {
			return workspace.File{}, nil, "", fmt.Errorf("spool verified Task media content: %w", err)
		}
		_, _ = contentHash.Write(data)
		chunkDigest := sha256.Sum256(data)
		digest := hex.EncodeToString(chunkDigest[:])
		if previous, ok := locations[digest]; ok && previous.size != chunkSize {
			return workspace.File{}, nil, "", domain.NewError(domain.CodeWorkspaceConflict, "Task media content blocks disagree")
		}
		if _, ok := locations[digest]; !ok {
			locations[digest] = taskMediaBlockLocation{offset: offset, size: chunkSize}
		}
		chunks = append(chunks, workspace.Chunk{SHA256: digest, SizeBytes: chunkSize})
		offset += chunkSize
	}
	var extra [1]byte
	if n, err := io.ReadFull(src, extra[:]); n != 0 || err != io.EOF {
		return workspace.File{}, nil, "", domain.NewError(domain.CodeInvalidState, "Task media content size changed while publishing")
	}
	contentDigest := contentHash.Sum(nil)
	if !bytes.Equal(contentDigest, expectedSHA) {
		return workspace.File{}, nil, "", domain.NewError(domain.CodeInvalidState, "Task media content digest changed while publishing")
	}
	digest := hex.EncodeToString(contentDigest)
	return workspace.File{Kind: workspace.FileKindRegular, SizeBytes: size, SHA256: digest, Mode: 0o644, Chunks: chunks}, locations, digest, nil
}
