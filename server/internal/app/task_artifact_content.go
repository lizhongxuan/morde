package app

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"path"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
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

// OpenTaskValidationReviewArtifactContent exposes a committed artifact only
// when the current review claim's evidence record binds it to one successful
// source tool step.
func (s *TaskService) OpenTaskValidationReviewArtifactContent(ctx context.Context, claim TaskValidationReviewClaim, artifactID string) (TaskArtifactContent, error) {
	details, err := s.GetTaskValidationReviewSnapshot(ctx, claim)
	if err != nil {
		return TaskArtifactContent{}, err
	}
	if err := s.requireCurrentTaskValidationReviewClaim(ctx, claim); err != nil {
		return TaskArtifactContent{}, err
	}
	if !domain.ValidArtifactID(artifactID) {
		return TaskArtifactContent{}, domain.NewError(domain.CodeNotFound, "task artifact not found")
	}

	artifact, err := taskValidationReviewArtifact(details, claim, artifactID)
	if err != nil {
		return TaskArtifactContent{}, err
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
	if err := s.requireCurrentTaskValidationReviewClaim(ctx, claim); err != nil {
		return TaskArtifactContent{}, errors.Join(err, body.Close())
	}
	return TaskArtifactContent{Filename: path.Base(artifact.Path), MediaType: mediaType,
		SizeBytes: artifact.SizeBytes, SHA256: artifact.SHA256, Body: body}, nil
}

func (s *TaskService) requireCurrentTaskValidationReviewClaim(ctx context.Context, claim TaskValidationReviewClaim) error {
	verifier, ok := s.store.(TaskValidationReviewClaimVerifier)
	if !ok {
		return domain.NewError(domain.CodeUnsupported, "Task validation review claim verification is unavailable")
	}
	current, err := verifier.IsCurrentTaskValidationReviewClaim(ctx, claim)
	if err != nil {
		return err
	}
	if !current {
		return domain.NewError(domain.CodeConflict, "Task validation review claim is no longer current")
	}
	return nil
}

func taskValidationReviewArtifact(details TaskDetails, claim TaskValidationReviewClaim, artifactID string) (task.Artifact, error) {
	missing := func() (task.Artifact, error) {
		return task.Artifact{}, domain.NewError(domain.CodeNotFound, "task artifact not found")
	}
	invalid := func() (task.Artifact, error) {
		return task.Artifact{}, domain.NewError(domain.CodeInvalidState, "task artifact evidence is invalid")
	}

	if details.Task.ID != claim.TaskID || details.Task.OwnerID != claim.OwnerID ||
		details.Task.InitiatorID != claim.InitiatorID || details.Task.ConversationID != claim.ConversationID ||
		details.Task.Status != task.StatusCompleted ||
		details.Task.CurrentRevision == nil || *details.Task.CurrentRevision != claim.TaskRevision ||
		details.Task.DesiredRevision == nil || *details.Task.DesiredRevision != claim.TaskRevision ||
		details.Task.WorkspaceID == "" || !domain.ValidWorkspaceID(details.Task.WorkspaceID) ||
		details.ValidationEvidence == nil {
		return missing()
	}
	evidence := details.ValidationEvidence
	if evidence.TaskRevision != claim.TaskRevision || evidence.EventSequence != claim.ResultEventSequence ||
		evidence.ResultSHA256 != hex.EncodeToString(claim.ResultSHA256) {
		return invalid()
	}

	var artifact task.Artifact
	foundArtifact := false
	for _, candidate := range details.Artifacts {
		if candidate.ID != artifactID {
			continue
		}
		if foundArtifact {
			return invalid()
		}
		artifact, foundArtifact = candidate, true
	}
	if !foundArtifact {
		return missing()
	}

	var sourceRef *TaskValidationEvidenceRefRecord
	for _, resultClaim := range evidence.Claims {
		for index := range resultClaim.EvidenceRefs {
			ref := &resultClaim.EvidenceRefs[index]
			if ref.Kind != TaskValidationEvidenceSideEffect || ref.ID != artifactID {
				continue
			}
			if ref.TaskRevision != claim.TaskRevision || ref.Sequence < 1 || ref.CallID == "" ||
				SafeTaskValidationCallID(ref.CallID) != ref.CallID {
				return invalid()
			}
			if sourceRef != nil && (sourceRef.TaskRevision != ref.TaskRevision || sourceRef.Sequence != ref.Sequence ||
				sourceRef.CallID != ref.CallID) {
				return invalid()
			}
			sourceRef = ref
		}
	}
	if sourceRef == nil {
		return missing()
	}

	if artifact.TaskID != claim.TaskID || !domain.ValidStepID(artifact.StepID) ||
		artifact.WorkspaceID != details.Task.WorkspaceID || artifact.Revision < 0 || artifact.SizeBytes < 0 ||
		artifact.SizeBytes > workspace.MaxSyncFileBytes || !validSyncDigest(artifact.SHA256) ||
		workspace.ValidateRelativePath(artifact.Path) != nil || artifact.SourceCallID == "" ||
		SafeTaskValidationCallID(artifact.SourceCallID) != artifact.SourceCallID || artifact.SourceCallID != sourceRef.CallID {
		return invalid()
	}

	var sourceStep task.Step
	foundSourceStep := false
	for _, step := range details.Steps {
		if step.ID != artifact.StepID && step.ToolCallKey != sourceRef.CallID {
			continue
		}
		if step.ID != artifact.StepID || step.ToolCallKey != sourceRef.CallID || step.TaskID != claim.TaskID ||
			step.Kind != "tool" || step.Status != task.StepSucceeded || step.TaskRevision == nil ||
			*step.TaskRevision != claim.TaskRevision || step.Sequence != sourceRef.Sequence || strings.TrimSpace(step.ToolName) == "" {
			return invalid()
		}
		if foundSourceStep {
			return invalid()
		}
		sourceStep, foundSourceStep = step, true
	}
	if !foundSourceStep || sourceStep.ToolCallKey != artifact.SourceCallID {
		return invalid()
	}
	return artifact, nil
}
