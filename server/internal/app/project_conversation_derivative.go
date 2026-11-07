package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

const ProjectConversationDerivativeMaxJSONBytes = int64(8 << 20)

// Content is encoded as base64 in JSON. The ordered set is the idempotency
// identity; a completed or skipped set can never be silently replaced.
type ProjectConversationDerivativeFile struct {
	Kind      string `json:"kind"`
	Page      int    `json:"page"`
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256"`
	Content   []byte `json:"content_base64"`
}

type ProjectConversationDerivatives struct {
	Outcome string                              `json:"outcome"`
	Files   []ProjectConversationDerivativeFile `json:"files"`
}

type projectConversationDerivativeStore interface {
	PutProjectConversationDerivatives(context.Context, identity.Principal, ProjectConversationAttachmentScope, ProjectConversationDerivatives) (ProjectConversationAttachmentStatus, error)
}

func (s *ProjectConversationAttachmentService) DerivativesReady() bool {
	if s == nil {
		return false
	}
	_, ok := s.store.(projectConversationDerivativeStore)
	return ok
}

func ValidProjectDerivativeMode(value string) bool {
	return value == "" || value == "none" || value == "pdf" || value == "image"
}

// ValidateProjectConversationDerivatives validates the actual bytes, not just
// declared metadata. Empty PDF text is allowed for scans and blank pages.
func ValidateProjectConversationDerivatives(mode string, value ProjectConversationDerivatives) ([]byte, int64, error) {
	invalid := func() ([]byte, int64, error) {
		return nil, 0, domain.InvalidArgument("derivatives", "project attachment derivative set is invalid")
	}
	if (mode != "pdf" && mode != "image") || value.Files == nil || len(value.Files) > 11 ||
		(value.Outcome != "completed" && value.Outcome != "skipped") {
		return invalid()
	}
	if value.Outcome == "skipped" && len(value.Files) != 0 {
		return invalid()
	}
	textCount, imageCount := 0, 0
	var total int64
	type position struct {
		kind string
		page int
	}
	seen := make(map[position]bool, len(value.Files))
	for _, file := range value.Files {
		metadata := ProjectConversationAttachmentMetadata{AttachmentID: "pca_00000000000000000000000000000000", Name: file.Name,
			MediaType: file.MediaType, SizeBytes: int64(len(file.Content)), SHA256: file.SHA256}
		if file.Content == nil || ValidateProjectConversationAttachmentMetadata(metadata) != nil || file.Page < 0 || file.Page > 10 {
			return invalid()
		}
		digest := sha256.Sum256(file.Content)
		if hex.EncodeToString(digest[:]) != file.SHA256 {
			return invalid()
		}
		key := position{kind: file.Kind, page: file.Page}
		if seen[key] {
			return invalid()
		}
		seen[key] = true
		switch {
		case mode == "pdf" && file.Kind == "extracted_text" && file.Page == 0 && file.MediaType == "text/plain":
			if len(file.Content) > 48<<10 || !utf8.Valid(file.Content) {
				return invalid()
			}
			textCount++
		case mode == "pdf" && file.Kind == "page_preview" && file.Page >= 1 && file.MediaType == "image/jpeg":
			if len(file.Content) > 512<<10 || !validCloudDSHTaskAttachmentImage(file.MediaType, file.Content) {
				return invalid()
			}
			imageCount++
		case mode == "image" && file.Kind == "image_conversion" && file.Page == 0 && file.MediaType == "image/jpeg":
			if len(file.Content) > 512<<10 || !validCloudDSHTaskAttachmentImage(file.MediaType, file.Content) {
				return invalid()
			}
			imageCount++
		default:
			return invalid()
		}
		total += int64(len(file.Content))
	}
	if value.Outcome == "completed" && ((mode == "pdf" && (textCount != 1 || imageCount > 10)) ||
		(mode == "image" && (textCount != 0 || imageCount != 1))) {
		return invalid()
	}
	encoded, err := json.Marshal(value)
	if err != nil || int64(len(encoded)) > ProjectConversationDerivativeMaxJSONBytes {
		return invalid()
	}
	fingerprint := sha256.Sum256(encoded)
	return fingerprint[:], total, nil
}

func (s *ProjectConversationAttachmentService) PutDerivatives(ctx context.Context, principal identity.Principal,
	scope ProjectConversationAttachmentScope, value ProjectConversationDerivatives) (ProjectConversationAttachmentStatus, error) {
	if err := ValidateProjectConversationAttachmentScope(principal, scope); err != nil {
		return ProjectConversationAttachmentStatus{}, err
	}
	store, ok := s.store.(projectConversationDerivativeStore)
	if !ok {
		return ProjectConversationAttachmentStatus{}, domain.NewError(domain.CodeInternal, "project attachment derivatives are unavailable")
	}
	return store.PutProjectConversationDerivatives(ctx, principal, scope, value)
}
