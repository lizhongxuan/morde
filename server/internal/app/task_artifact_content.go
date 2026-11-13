package app

import (
	"context"
	"io"
	"mime"
	"path"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type TaskArtifactReader interface {
	OpenCommittedArtifact(context.Context, TaskArtifactProof) (io.ReadSeekCloser, error)
}

type TaskArtifactContent struct {
	Filename  string
	MediaType string
	SizeBytes int64
	SHA256    string
	Body      io.ReadSeekCloser
}

func (s *TaskService) SetTaskArtifactReader(reader TaskArtifactReader) error {
	if s == nil || reader == nil {
		return domain.NewError(domain.CodeInvalidArgument, "task artifact reader is required")
	}
	if s.artifactReader != nil {
		return domain.NewError(domain.CodeInvalidState, "task artifact reader cannot be replaced")
	}
	s.artifactReader = reader
	return nil
}

func (s *TaskService) OpenTaskArtifactContent(ctx context.Context, principal identity.Principal, taskID, artifactID string) (TaskArtifactContent, error) {
	if ctx == nil || !domain.ValidTaskID(taskID) || !domain.ValidArtifactID(artifactID) {
		return TaskArtifactContent{}, domain.NewError(domain.CodeNotFound, "task artifact not found")
	}
	details, err := s.Get(ctx, principal, taskID)
	if err != nil {
		return TaskArtifactContent{}, err
	}
	for _, artifact := range details.Artifacts {
		if artifact.ID != artifactID {
			continue
		}
		if artifact.TaskID != taskID || !domain.ValidStepID(artifact.StepID) || !domain.ValidWorkspaceID(artifact.WorkspaceID) ||
			artifact.WorkspaceID != details.Task.WorkspaceID || artifact.Revision < 0 || artifact.SizeBytes < 0 ||
			artifact.SizeBytes > workspace.MaxSyncFileBytes || !validSyncDigest(artifact.SHA256) ||
			workspace.ValidateRelativePath(artifact.Path) != nil {
			return TaskArtifactContent{}, domain.NewError(domain.CodeInvalidState, "task artifact proof is invalid")
		}
		mediaType := strings.TrimSpace(artifact.MediaType)
		if mediaType == "" {
			return TaskArtifactContent{}, domain.NewError(domain.CodeInvalidState, "task artifact media type is invalid")
		}
		if _, _, err := mime.ParseMediaType(mediaType); err != nil {
			return TaskArtifactContent{}, domain.NewError(domain.CodeInvalidState, "task artifact media type is invalid")
		}
		if s.artifactReader == nil {
			return TaskArtifactContent{}, domain.NewError(domain.CodeUnsupported, "task artifact download is unavailable")
		}
		ownerID := details.Task.WorkspaceOwnerID
		if ownerID == "" {
			ownerID = details.Task.OwnerID
		}
		body, err := s.artifactReader.OpenCommittedArtifact(ctx, TaskArtifactProof{
			OwnerID: ownerID, WorkspaceID: artifact.WorkspaceID, Revision: artifact.Revision,
			Path: artifact.Path, SizeBytes: artifact.SizeBytes, SHA256: artifact.SHA256,
		})
		if err != nil {
			return TaskArtifactContent{}, err
		}
		if body == nil {
			return TaskArtifactContent{}, domain.NewError(domain.CodeInvalidState, "task artifact content is unavailable")
		}
		return TaskArtifactContent{Filename: path.Base(artifact.Path), MediaType: mediaType,
			SizeBytes: artifact.SizeBytes, SHA256: artifact.SHA256, Body: body}, nil
	}
	return TaskArtifactContent{}, domain.NewError(domain.CodeNotFound, "task artifact not found")
}
