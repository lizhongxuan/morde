package app

import (
	"crypto/sha256"
	"encoding/hex"
	"mime"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

const (
	MaxTaskValidationReviewArtifactPreviewBytes   = 4096
	MaxTaskValidationReviewArtifactReadBytes      = 8 << 20
	MaxTaskValidationReviewArtifactTotalReadBytes = 16 << 20
	MaxTaskValidationReviewArtifactProofs         = 64
	MaxTaskValidationReviewArtifactProofBytes     = 512 << 10
)

// ValidateTaskValidationReviewArtifactContentProofs binds readback facts to
// artifacts cited by this exact Task result. Proofs never contain full files.
func ValidateTaskValidationReviewArtifactContentProofs(proofs []TaskValidationReviewArtifactContentProof,
	evidence *TaskValidationEvidenceRecord) error {
	invalid := func() error {
		return domain.InvalidArgument("task_validation_review_artifact", "Task validation review artifact proof is invalid")
	}
	if len(proofs) > MaxTaskValidationReviewArtifactProofs || len(proofs) > 0 && evidence == nil {
		return invalid()
	}
	allowed := make(map[TaskValidationEvidenceRefRecord]struct{})
	if evidence != nil {
		for _, claim := range evidence.Claims {
			for _, ref := range claim.EvidenceRefs {
				if ref.Kind == TaskValidationEvidenceSideEffect {
					allowed[ref] = struct{}{}
				}
			}
		}
	}
	seen := make(map[TaskValidationEvidenceRefRecord]struct{}, len(proofs))
	for _, proof := range proofs {
		if _, ok := allowed[proof.Ref]; !ok {
			return invalid()
		}
		if _, duplicate := seen[proof.Ref]; duplicate {
			return invalid()
		}
		seen[proof.Ref] = struct{}{}
		if proof.Ref.Kind != TaskValidationEvidenceSideEffect || !domain.ValidWorkspaceID(proof.WorkspaceID) ||
			proof.Ref.TaskRevision != evidence.TaskRevision || proof.Ref.Sequence < 1 || proof.Ref.CallID == "" ||
			SafeTaskValidationCallID(proof.Ref.CallID) != proof.Ref.CallID || proof.Revision < 0 ||
			proof.SizeBytes < 0 || proof.SizeBytes > MaxTaskValidationReviewArtifactReadBytes || proof.SizeBytes > workspace.MaxSyncFileBytes ||
			workspace.ValidateRelativePath(proof.Path) != nil || !validTaskValidationReviewArtifactDigest(proof.SHA256) ||
			proof.ReadbackSHA256 != proof.SHA256 || !validTaskValidationReviewArtifactMediaType(proof.MediaType) ||
			proof.PreviewBytes < 0 || proof.PreviewBytes > MaxTaskValidationReviewArtifactPreviewBytes {
			return invalid()
		}
		if proof.TextContent {
			if !taskValidationReviewArtifactMediaTypeIsText(proof.MediaType) ||
				!validTaskValidationReviewArtifactDigest(proof.PreviewSHA256) || proof.PreviewBytes > proof.SizeBytes {
				return invalid()
			}
			if proof.ContentComplete {
				if proof.PreviewBytes != proof.SizeBytes || proof.PreviewSHA256 != proof.SHA256 {
					return invalid()
				}
			} else if proof.SizeBytes <= proof.PreviewBytes {
				return invalid()
			}
		} else if proof.ContentComplete || proof.PreviewBytes != 0 || proof.PreviewSHA256 != "" {
			return invalid()
		}
	}
	return nil
}

func validTaskValidationReviewArtifactDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	digest, err := hex.DecodeString(value)
	return err == nil && len(digest) == sha256.Size && hex.EncodeToString(digest) == value
}

func validTaskValidationReviewArtifactMediaType(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return false
	}
	_, _, err := mime.ParseMediaType(value)
	return err == nil
}

func taskValidationReviewArtifactMediaTypeIsText(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	mediaType = strings.ToLower(mediaType)
	return strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" || mediaType == "application/xml" ||
		mediaType == "application/yaml" || mediaType == "application/x-yaml" || mediaType == "application/javascript" ||
		strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml")
}
