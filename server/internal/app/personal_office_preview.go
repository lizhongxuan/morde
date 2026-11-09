package app

import "morde/server/internal/domain"

// PersonalOfficePreviewMaxBytes is the maximum accepted compressed Office
// package size. It matches the DSH attachment reader's safety limit.
const PersonalOfficePreviewMaxBytes = maxCloudDSHOfficeAttachmentBytes

// PreviewPersonalOfficeText extracts bounded plain text from an in-memory
// Office document. It does not persist the bytes or create a task.
func PreviewPersonalOfficeText(mediaType string, content []byte) (string, error) {
	switch mediaType {
	case cloudDSHOfficeDOCXMediaType, cloudDSHOfficeXLSXMediaType, cloudDSHOfficePPTXMediaType:
	default:
		return "", domain.InvalidArgument("content_type", "Office preview content type is not supported")
	}
	if len(content) == 0 {
		return "", domain.InvalidArgument("document", "Office document is empty or malformed")
	}
	if len(content) > PersonalOfficePreviewMaxBytes {
		return "", domain.NewError(domain.CodeRequestTooLarge, "Office document exceeds the preview limit")
	}
	text, err := extractCloudDSHOfficeText(mediaType, content)
	if err != nil {
		return "", domain.InvalidArgument("document", "Office document is empty or malformed")
	}
	return text, nil
}
