package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacecurrent"
)

func (t *cloudDSHWorkspaceTools) validateLiveRevision(ctx context.Context, current task.Task, revision int64) error {
	if !cloudDSHWorkspaceToolsEligible(current) || revision < *current.InputRevision || current.WorkspaceCheckpointRevision == nil || revision > *current.WorkspaceCheckpointRevision {
		return domain.NewError(domain.CodeWorkspaceConflict, "requested Workspace checkpoint is outside this task")
	}
	value, err := t.workspaces.store.GetWorkspace(ctx, current.WorkspaceID)
	if err != nil {
		return err
	}
	if value.OwnerID != taskWorkspaceOwner(current) || value.State != workspace.StateReady || value.DeletedAt != nil || revision > value.Revision {
		return domain.NewError(domain.CodeNotFound, "Workspace not found")
	}
	return nil
}

func (t *cloudDSHWorkspaceTools) listLive(ctx context.Context, current task.Task, arguments cloudDSHWorkspaceListArguments) (AgentToolResult, error) {
	if err := t.validateLiveRevision(ctx, current, arguments.ExpectedRevision); err != nil {
		return cloudDSHWorkspaceFailure(err), nil
	}
	entries := make([]cloudDSHWorkspaceListEntry, 0)
	var walk func(string) error
	walk = func(relative string) error {
		values, err := t.workspaces.currentDirectory.ReadDirectory(taskWorkspaceOwner(current), current.WorkspaceID, relative)
		if err != nil {
			return domain.NewError(domain.CodeWorkspaceUnavailable, "live Workspace directory could not be read")
		}
		for _, value := range values {
			entryPath := value.Name
			if relative != "" {
				entryPath = relative + "/" + value.Name
			}
			if !cloudDSHWorkspacePathIsSensitive(entryPath) && workspacePathWithinPrefix(entryPath, arguments.Prefix) && entryPath > arguments.AfterPath {
				entries = append(entries, cloudDSHWorkspaceListEntry{Path: entryPath, Kind: value.Kind, SizeBytes: value.SizeBytes})
			}
			if value.Kind == "directory" && (arguments.Prefix == "" || workspacePathWithinPrefix(entryPath, arguments.Prefix) || strings.HasPrefix(arguments.Prefix, entryPath+"/")) {
				if err := walk(entryPath); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(""); err != nil {
		return cloudDSHWorkspaceFailure(err), nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	limit := min(arguments.Limit, cloudDSHWorkspaceListMaxItems)
	if limit < 1 {
		limit = 1
	}
	output := cloudDSHWorkspaceListOutput{WorkspaceID: current.WorkspaceID, Revision: arguments.ExpectedRevision,
		Entries: append([]cloudDSHWorkspaceListEntry(nil), entries[:min(limit, len(entries))]...)}
	if len(entries) > limit {
		output.NextAfterPath = entries[limit-1].Path
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return AgentToolResult{}, err
	}
	if len(encoded) > maxAgentToolOutputBytes {
		return AgentToolResult{}, domain.NewError(domain.CodeResultTooLarge, "Workspace list output exceeds the task limit")
	}
	return AgentToolResult{Status: AgentToolSucceeded, Output: string(encoded)}, nil
}

func (t *cloudDSHWorkspaceTools) readLive(ctx context.Context, current task.Task, arguments cloudDSHWorkspaceReadArguments) (AgentToolResult, error) {
	if err := t.validateLiveRevision(ctx, current, arguments.ExpectedRevision); err != nil {
		return cloudDSHWorkspaceFailure(err), nil
	}
	file, info, err := t.workspaces.currentDirectory.OpenRegularFile(taskWorkspaceOwner(current), current.WorkspaceID, arguments.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cloudDSHWorkspaceFailure(domain.NewError(domain.CodeNotFound, "Workspace file not found")), nil
		}
		return cloudDSHWorkspaceFailure(domain.NewError(domain.CodeWorkspaceUnavailable, "live Workspace file could not be opened")), nil
	}
	defer file.Close()
	if info.Size() < 0 || info.Size() > cloudDSHWorkspaceReadMaxBytes {
		return cloudDSHWorkspaceFailure(domain.NewError(domain.CodeRequestTooLarge, "Workspace file exceeds the read limit")), nil
	}
	content, err := io.ReadAll(io.LimitReader(file, cloudDSHWorkspaceReadMaxBytes+1))
	if err != nil {
		return cloudDSHWorkspaceFailure(domain.NewError(domain.CodeWorkspaceUnavailable, "live Workspace file could not be read")), nil
	}
	if int64(len(content)) > cloudDSHWorkspaceReadMaxBytes {
		return cloudDSHWorkspaceFailure(domain.NewError(domain.CodeRequestTooLarge, "Workspace file exceeds the read limit")), nil
	}
	if !utf8.Valid(content) {
		return AgentToolResult{}, domain.NewError(domain.CodeWorkspaceConflict, "Workspace file is not valid UTF-8 text")
	}
	digest := sha256.Sum256(content)
	encoded, err := json.Marshal(cloudDSHWorkspaceReadOutput{WorkspaceID: current.WorkspaceID, Revision: arguments.ExpectedRevision,
		Path: arguments.Path, SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:]), Content: string(content)})
	if err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Status: AgentToolSucceeded, Output: string(encoded)}, nil
}

func (t *cloudDSHWorkspaceTools) searchLive(ctx context.Context, current task.Task, arguments cloudDSHWorkspaceSearchArguments) (AgentToolResult, error) {
	if err := t.validateLiveRevision(ctx, current, arguments.ExpectedRevision); err != nil {
		return cloudDSHWorkspaceFailure(err), nil
	}
	output := cloudDSHWorkspaceSearchOutput{WorkspaceID: current.WorkspaceID, Revision: arguments.ExpectedRevision, Complete: true,
		Matches: make([]cloudDSHWorkspaceSearchMatch, 0)}
	query := strings.ToLower(strings.TrimSpace(arguments.Query))
	maxResults := arguments.MaxResults
	if maxResults == 0 {
		maxResults = 20
	}
	bytesScanned := int64(0)
	var walk func(string) error
	walk = func(relative string) error {
		values, err := t.workspaces.currentDirectory.ReadDirectory(taskWorkspaceOwner(current), current.WorkspaceID, relative)
		if err != nil {
			return domain.NewError(domain.CodeWorkspaceUnavailable, "live Workspace directory could not be read")
		}
		for _, value := range values {
			filePath := value.Name
			if relative != "" {
				filePath = relative + "/" + value.Name
			}
			if value.Kind == "directory" {
				if arguments.Prefix == "" || workspacePathWithinPrefix(filePath, arguments.Prefix) || strings.HasPrefix(arguments.Prefix, filePath+"/") {
					if err := walk(filePath); err != nil {
						return err
					}
				}
				continue
			}
			if value.Kind != "file" || cloudDSHWorkspacePathIsSensitive(filePath) || !workspacePathWithinPrefix(filePath, arguments.Prefix) {
				continue
			}
			if value.SizeBytes > cloudDSHWorkspaceReadMaxBytes || output.FilesScanned >= cloudDSHWorkspaceSearchMaxFiles || bytesScanned+value.SizeBytes > cloudDSHWorkspaceSearchMaxBytes {
				output.SkippedFiles++
				output.Complete = false
				if output.FilesScanned >= cloudDSHWorkspaceSearchMaxFiles || bytesScanned+value.SizeBytes > cloudDSHWorkspaceSearchMaxBytes {
					return errWorkspaceSearchLimit
				}
				continue
			}
			file, info, err := t.workspaces.currentDirectory.OpenRegularFile(taskWorkspaceOwner(current), current.WorkspaceID, filePath)
			if err != nil {
				output.SkippedFiles++
				output.Complete = false
				continue
			}
			content, readErr := io.ReadAll(io.LimitReader(file, cloudDSHWorkspaceReadMaxBytes+1))
			closeErr := file.Close()
			if readErr != nil || closeErr != nil || info.Size() != int64(len(content)) || !utf8.Valid(content) {
				output.SkippedFiles++
				output.Complete = false
				continue
			}
			output.FilesScanned++
			bytesScanned += int64(len(content))
			for lineNumber, line := range strings.Split(string(content), "\n") {
				if !strings.Contains(strings.ToLower(line), query) {
					continue
				}
				output.Matches = append(output.Matches, cloudDSHWorkspaceSearchMatch{Path: filePath, Line: lineNumber + 1,
					Text: truncateWorkspaceSearchSnippet(strings.TrimSpace(line), cloudDSHWorkspaceSearchMaxSnippetRunes)})
				if len(output.Matches) >= maxResults {
					output.Complete = false
					return errWorkspaceSearchLimit
				}
			}
		}
		return nil
	}
	if err := walk(""); err != nil && !errors.Is(err, errWorkspaceSearchLimit) {
		return cloudDSHWorkspaceFailure(err), nil
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return AgentToolResult{}, err
	}
	if len(encoded) > maxAgentToolOutputBytes {
		return AgentToolResult{}, domain.NewError(domain.CodeResultTooLarge, "Workspace search output exceeds the task limit")
	}
	return AgentToolResult{Status: AgentToolSucceeded, Output: string(encoded)}, nil
}

func (t *cloudDSHWorkspaceTools) writeLive(ctx context.Context, current task.Task, call ModelToolCall, rawArguments json.RawMessage,
	arguments cloudDSHWorkspaceWriteArguments) (AgentToolResult, error) {
	requestHash, err := agentOperationHash(call.Name, rawArguments)
	if err != nil {
		return AgentToolResult{}, err
	}
	operationKey := agentOperationKey(call.CallID)
	findReceipt := func() (TaskWorkspacePublishReceipt, bool, error) {
		receipt, err := t.workspaces.FindTaskWorkspacePublishReceipt(ctx, current.OwnerID, current.ID, current.WorkspaceID, operationKey)
		if domain.CodeOf(err) == domain.CodeNotFound {
			return TaskWorkspacePublishReceipt{}, false, nil
		}
		if err != nil {
			return TaskWorkspacePublishReceipt{}, false, err
		}
		if receipt.WorkspaceID != current.WorkspaceID || !receipt.DirectDirectorySource || receipt.RequestHash != hex.EncodeToString(requestHash[:]) || receipt.BaseRevision != arguments.ExpectedRevision {
			return TaskWorkspacePublishReceipt{}, false, domain.NewError(domain.CodeConflict, "Workspace operation key was reused with a different request")
		}
		return receipt, true, nil
	}
	pendingReceipt := false
	if receipt, found, err := findReceipt(); err != nil {
		return cloudDSHWorkspaceFailure(err), nil
	} else if found && receipt.State != TaskWorkspacePublishPending {
		return cloudDSHWorkspaceReceiptResult(receipt, operationKey), nil
	} else {
		pendingReceipt = found
	}
	command := TaskWorkspacePublishCommand{
		OwnerID: current.OwnerID, TaskID: current.ID, WorkspaceID: current.WorkspaceID, LeaseOwner: current.Lease.Owner,
		ProjectID: current.ProjectID, InitiatorID: current.InitiatorID, WorkspaceOwnerID: taskWorkspaceOwner(current),
		LeaseVersion: current.Lease.Version, InputRevision: *current.InputRevision, BaseRevision: arguments.ExpectedRevision,
		InputManifestSHA256: hex.EncodeToString(current.ContextDigest), OperationKey: operationKey, RequestHash: requestHash[:],
		DirectDirectorySource: true,
	}
	rejectConflict := func(message string) (AgentToolResult, error) {
		conflict := domain.NewError(domain.CodeWorkspaceConflict, message)
		if pendingReceipt {
			if rejectErr := t.workspaces.RejectTaskWorkspacePublishConflict(ctx, command); rejectErr != nil {
				return AgentToolResult{}, errors.Join(conflict, rejectErr)
			}
			if receipt, found, findErr := findReceipt(); findErr != nil {
				return AgentToolResult{}, findErr
			} else if found {
				return cloudDSHWorkspaceReceiptResult(receipt, operationKey), nil
			}
		}
		return cloudDSHWorkspaceFailure(conflict), nil
	}
	if current.WorkspaceCheckpointRevision == nil || arguments.ExpectedRevision != *current.WorkspaceCheckpointRevision {
		return rejectConflict("Workspace write checkpoint is stale")
	}
	baseline, err := t.readSnapshot(ctx, current, arguments.ExpectedRevision)
	if err != nil {
		return cloudDSHWorkspaceFailure(err), nil
	}
	workspaceValue, err := t.workspaces.store.GetWorkspace(ctx, current.WorkspaceID)
	if err != nil {
		return AgentToolResult{}, err
	}
	if workspaceValue.OwnerID != taskWorkspaceOwner(current) || workspaceValue.State != workspace.StateReady || workspaceValue.DeletedAt != nil ||
		workspaceValue.Revision != arguments.ExpectedRevision {
		return rejectConflict("Workspace changed since the task checkpoint")
	}
	live, err := readWorkspaceLiveSnapshot(ctx, t.workspaces.currentDirectory, taskWorkspaceOwner(current), current.WorkspaceID, t.workspaces.maxOwnerBytes)
	if err != nil {
		return cloudDSHWorkspaceFailure(err), nil
	}
	for blocked := range live.blockedPaths {
		if workspaceFilePathContains(blocked, arguments.Path) || workspaceFilePathContains(arguments.Path, blocked) {
			return rejectConflict("Workspace path contains an unsupported filesystem entry")
		}
	}
	baseFile, baseExists := workspaceFileByPath(baseline.files, arguments.Path)
	liveFile, liveExists := workspaceFileByPath(live.files, arguments.Path)
	desiredSHA := sha256Hex([]byte(arguments.Content))
	if liveExists && liveFile.SHA256 != desiredSHA && (!baseExists || liveFile.SHA256 != baseFile.SHA256) || baseExists && !liveExists {
		return rejectConflict("Workspace file changed since the task checkpoint")
	}
	cloudSnapshot := cloudDSHWorkspaceSnapshot{workspaceID: current.WorkspaceID, revision: arguments.ExpectedRevision,
		files: live.files, emptyDirectories: live.emptyDirectories, directories: live.directories, manifestSHA256: live.manifestSHA256}
	newFile, err := workspaceSnapshotWriteFile(cloudSnapshot, arguments.Path, []byte(arguments.Content))
	if err != nil {
		if domain.CodeOf(err) == domain.CodeWorkspaceConflict {
			return rejectConflict("Workspace file changed since the task checkpoint")
		}
		return cloudDSHWorkspaceFailure(err), nil
	}
	receipt, err := t.workspaces.ReserveTaskWorkspacePublish(ctx, command)
	if err != nil {
		if found, ok, findErr := findReceipt(); findErr == nil && ok && found.State != TaskWorkspacePublishPending {
			return cloudDSHWorkspaceReceiptResult(found, operationKey), nil
		}
		return cloudDSHWorkspaceFailure(err), nil
	}
	if receipt.State != TaskWorkspacePublishPending {
		return cloudDSHWorkspaceReceiptResult(receipt, operationKey), nil
	}
	pendingReceipt = true
	if !liveExists || liveFile.SHA256 != desiredSHA {
		mode := newFile.Mode
		expectedSHA := ""
		if liveExists {
			expectedSHA = liveFile.SHA256
		}
		if mode == 0 {
			mode = 0644
		}
		if err := t.workspaces.currentDirectory.WriteRegularFile(ctx, taskWorkspaceOwner(current), current.WorkspaceID,
			arguments.Path, []byte(arguments.Content), mode, expectedSHA); err != nil {
			if errors.Is(err, workspacecurrent.ErrWriteConflict) {
				return rejectConflict("Workspace file changed before write")
			}
			return cloudDSHWorkspaceFailure(domain.NewError(domain.CodeWorkspaceUnavailable, "live Workspace file could not be written")), nil
		}
	}
	updated, err := readWorkspaceLiveSnapshot(ctx, t.workspaces.currentDirectory, taskWorkspaceOwner(current), current.WorkspaceID, t.workspaces.maxOwnerBytes)
	if err != nil {
		return cloudDSHWorkspaceFailure(err), nil
	}
	receipt, err = t.workspaces.CommitTaskWorkspacePublishDirect(ctx, command, updated.files, updated.emptyDirectories, updated.directories,
		updated.manifestSHA256, func(_ context.Context, digest string) ([]byte, error) {
			return updated.readBlock(t.workspaces.currentDirectory, taskWorkspaceOwner(current), current.WorkspaceID, digest)
		})
	if err != nil {
		if found, ok, findErr := findReceipt(); findErr == nil && ok && found.State != TaskWorkspacePublishPending {
			return cloudDSHWorkspaceReceiptResult(found, operationKey), nil
		}
		return cloudDSHWorkspaceFailure(err), nil
	}
	return cloudDSHWorkspaceReceiptResult(receipt, operationKey), nil
}

var errWorkspaceSearchLimit = errors.New("Workspace search result limit reached")
