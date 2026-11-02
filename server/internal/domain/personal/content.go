package personal

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/schedule"
)

const (
	ObjectTypeMemory           = "memory"
	ObjectTypeSoul             = "soul"
	ObjectTypeDiary            = "diary"
	ObjectTypeGoal             = "goal"
	ObjectTypeSchedule         = "schedule"
	ObjectTypeFeedPost         = "feed_post"
	ObjectTypeFeedPreferences  = "feed_preferences"
	ObjectTypeIdeaMark         = "idea_mark"
	ObjectTypeLibraryItem      = "library_item"
	ObjectTypeSkillSetting     = "skill_setting"
	ObjectTypeConnectorSetting = "connector_setting"
)

var personalSyncObjectTypes = [...]string{
	ObjectTypeProfile,
	ObjectTypeMemory,
	ObjectTypeSoul,
	ObjectTypeDiary,
	ObjectTypeGoal,
	ObjectTypeSchedule,
	ObjectTypeFeedPost,
	ObjectTypeFeedPreferences,
	ObjectTypeIdeaMark,
	ObjectTypeLibraryItem,
	ObjectTypeSkillSetting,
	ObjectTypeConnectorSetting,
}

// PersonalSyncObjectTypes returns the complete allowlist, including profile,
// whose body is validated by ValidateMutation.
func PersonalSyncObjectTypes() []string {
	return append([]string(nil), personalSyncObjectTypes[:]...)
}

// IsPersonalSyncObjectType reports whether the type is included in personal sync.
func IsPersonalSyncObjectType(objectType string) bool {
	switch objectType {
	case ObjectTypeProfile, ObjectTypeMemory, ObjectTypeSoul, ObjectTypeDiary, ObjectTypeGoal, ObjectTypeSchedule,
		ObjectTypeFeedPost, ObjectTypeFeedPreferences, ObjectTypeIdeaMark,
		ObjectTypeLibraryItem, ObjectTypeSkillSetting, ObjectTypeConnectorSetting:
		return true
	default:
		return false
	}
}

// ValidateObjectIdentifier checks the personal sync type allowlist and the
// stable, path-free identifier contract for each type.
func ValidateObjectIdentifier(objectType, objectID string) error {
	if !IsPersonalSyncObjectType(objectType) {
		if !validObjectType(objectType) {
			return domain.InvalidArgument("object_type", "personal object type is invalid")
		}
		return domain.NewError(domain.CodeUnsupported, "personal object type is not enabled")
	}
	if !validOpaqueID(objectID) || objectID == "." || objectID == ".." {
		return domain.InvalidArgument("object_id", "personal object id is invalid")
	}
	if objectType == ObjectTypeProfile {
		if objectID != ProfileObjectID {
			return domain.InvalidArgument("object_id", "profile object id is invalid")
		}
		return nil
	}
	if objectType == ObjectTypeSchedule && !schedule.ValidID(objectID) {
		return domain.InvalidArgument("object_id", "schedule id is invalid")
	}

	valid := true
	switch objectType {
	case ObjectTypeMemory:
		valid = objectID == "global" || objectID == "user" || isMemoryEntryID(objectID)
	case ObjectTypeFeedPreferences:
		valid = objectID == "default"
	case ObjectTypeSkillSetting:
		valid = strings.HasPrefix(objectID, "builtin_") && len(objectID) > len("builtin_")
	case ObjectTypeConnectorSetting:
		valid = strings.HasPrefix(objectID, "catalogue_") && len(objectID) > len("catalogue_")
	}
	if !valid {
		return domain.InvalidArgument("object_id", "personal object id is invalid for its type")
	}
	return nil
}

// ValidateObjectBody validates the stored shape for an allowlisted object.
// Profile bodies use the existing profile mutation validator.
func ValidateObjectBody(objectType, objectID string, body []byte) error {
	if err := ValidateObjectIdentifier(objectType, objectID); err != nil {
		return err
	}
	if len(body) > MaxMutationBodyBytes {
		return domain.NewError(domain.CodeRequestTooLarge, "personal object body exceeds the size limit")
	}
	if !utf8.Valid(body) {
		return invalidPersonalBody()
	}
	if objectType == ObjectTypeSchedule {
		var value schedule.SyncDefinition
		if !decodePersonalObject(body, &value, "status", "host_id", "model_id", "objective", "timing", "budget", "misfire_policy", "notification_events", "observation") ||
			schedule.ValidateSyncDefinition(objectID, value) != nil {
			return invalidPersonalBody()
		}
		return nil
	}
	if objectType == ObjectTypeProfile {
		if !decodePersonalObject(body, &profileBody{}, "name", "avatar", "emoji", "color", "description", "style") {
			return invalidPersonalBody()
		}
		return validateProfileBody(body)
	}

	switch objectType {
	case ObjectTypeMemory:
		var value struct {
			Content  string  `json:"content"`
			Category *string `json:"category"`
		}
		if !decodePersonalObject(body, &value, "content", "category") || !validText(value.Content, 1, 65536) ||
			(value.Category != nil && runeLength(*value.Category) > 64) {
			return invalidPersonalBody()
		}
	case ObjectTypeSoul:
		var value struct {
			Name    string  `json:"name"`
			Emoji   *string `json:"emoji"`
			Style   *string `json:"style"`
			Lang    *string `json:"lang"`
			Content string  `json:"content"`
		}
		if !decodePersonalObject(body, &value, "name", "emoji", "style", "lang", "content") ||
			!validText(value.Name, 1, 80) || !validText(value.Content, 1, 65536) ||
			(value.Emoji != nil && runeLength(*value.Emoji) > 16) ||
			(value.Style != nil && runeLength(*value.Style) > 64) ||
			(value.Lang != nil && !validLanguageTag(*value.Lang)) {
			return invalidPersonalBody()
		}
	case ObjectTypeDiary:
		var value struct {
			Date    string `json:"date"`
			Content string `json:"content"`
		}
		if !decodePersonalObject(body, &value, "date", "content") || !validDate(value.Date) || !validText(value.Content, 1, 65536) {
			return invalidPersonalBody()
		}
	case ObjectTypeGoal:
		var value struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Status      string `json:"status"`
			Steps       []struct {
				ID     string `json:"id"`
				Title  string `json:"title"`
				Status string `json:"status"`
				Note   string `json:"note"`
			} `json:"steps"`
			RelatedMaterialIDs []string `json:"related_material_ids"`
		}
		if !decodePersonalObject(body, &value, "title", "description", "status", "steps", "related_material_ids") ||
			!validText(value.Title, 1, 200) || strings.TrimSpace(value.Title) != value.Title ||
			runeLength(value.Description) > 4096 || (value.Status != "active" && value.Status != "paused" && value.Status != "done" && value.Status != "cancelled") ||
			value.Steps == nil || len(value.Steps) > 100 || len(value.RelatedMaterialIDs) > 100 {
			return invalidPersonalBody()
		}
		stepIDs := make(map[string]struct{}, len(value.Steps))
		for _, step := range value.Steps {
			if !validOpaqueID(step.ID) || !validText(step.Title, 1, 200) || strings.TrimSpace(step.Title) != step.Title ||
				runeLength(step.Note) > 2000 ||
				(step.Status != "pending" && step.Status != "in_progress" && step.Status != "done" && step.Status != "blocked" && step.Status != "skipped") {
				return invalidPersonalBody()
			}
			if _, exists := stepIDs[step.ID]; exists {
				return invalidPersonalBody()
			}
			stepIDs[step.ID] = struct{}{}
		}
		materialIDs := make(map[string]struct{}, len(value.RelatedMaterialIDs))
		for _, materialID := range value.RelatedMaterialIDs {
			if !validOpaqueID(materialID) {
				return invalidPersonalBody()
			}
			if _, exists := materialIDs[materialID]; exists {
				return invalidPersonalBody()
			}
			materialIDs[materialID] = struct{}{}
		}
	case ObjectTypeFeedPost:
		var value struct {
			Title     string  `json:"title"`
			Type      string  `json:"type"`
			Emoji     *string `json:"emoji"`
			CreatedAt *string `json:"created_at"`
			Liked     *bool   `json:"liked"`
			Body      string  `json:"body"`
		}
		if !decodePersonalObject(body, &value, "title", "type", "emoji", "created_at", "liked", "body") ||
			!validText(value.Title, 1, 200) || !validText(value.Type, 1, 32) || !validText(value.Body, 1, 65536) ||
			(value.Emoji != nil && runeLength(*value.Emoji) > 16) ||
			(value.CreatedAt != nil && !validRFC3339(*value.CreatedAt)) {
			return invalidPersonalBody()
		}
	case ObjectTypeFeedPreferences:
		var value struct {
			Content string `json:"content"`
		}
		if !decodePersonalObject(body, &value, "content") || !validText(value.Content, 1, 8192) {
			return invalidPersonalBody()
		}
	case ObjectTypeIdeaMark:
		var value struct {
			State string `json:"state"`
		}
		if !decodePersonalObject(body, &value, "state") || (value.State != "saved" && value.State != "dismissed") {
			return invalidPersonalBody()
		}
	case ObjectTypeLibraryItem:
		var value struct {
			Name         string  `json:"name"`
			Kind         string  `json:"kind"`
			MIME         *string `json:"mime"`
			SizeBytes    *int64  `json:"size_bytes"`
			AttachmentID string  `json:"attachment_id"`
		}
		if !decodePersonalObject(body, &value, "name", "kind", "mime", "size_bytes", "attachment_id") ||
			!validLibraryName(value.Name) || (value.Kind != "artifact" && value.Kind != "media") ||
			(value.MIME != nil && runeLength(*value.MIME) > 128) || (value.SizeBytes != nil && *value.SizeBytes < 0) ||
			(value.AttachmentID != "" && !ValidPersonalAttachmentID(value.AttachmentID)) {
			return invalidPersonalBody()
		}
	case ObjectTypeSkillSetting:
		var value struct {
			Enabled *bool `json:"enabled"`
		}
		if !decodePersonalObject(body, &value, "enabled") || value.Enabled == nil {
			return invalidPersonalBody()
		}
	case ObjectTypeConnectorSetting:
		var value struct {
			Auth    string `json:"auth"`
			Enabled *bool  `json:"enabled"`
		}
		if !decodePersonalObject(body, &value, "auth", "enabled") || value.Enabled == nil ||
			(value.Auth != "open" && value.Auth != "oauth" && value.Auth != "key") {
			return invalidPersonalBody()
		}
	default:
		return domain.InvalidArgument("object_type", "personal object body type is invalid")
	}
	return nil
}

func isMemoryEntryID(value string) bool {
	if len(value) != len("m_")+8 || !strings.HasPrefix(value, "m_") {
		return false
	}
	for i := 2; i < len(value); i++ {
		c := value[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

func decodePersonalObject(body []byte, target any, allowedFields ...string) bool {
	var fields map[string]json.RawMessage
	if len(body) == 0 || json.Unmarshal(body, &fields) != nil || fields == nil {
		return false
	}
	allowed := make(map[string]struct{}, len(allowedFields))
	for _, field := range allowedFields {
		allowed[field] = struct{}{}
	}
	for field, raw := range fields {
		if _, ok := allowed[field]; !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return false
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		return false
	}
	return decoder.Decode(new(any)) == io.EOF
}

func validText(value string, min, max int) bool {
	length := runeLength(value)
	return length >= min && length <= max
}

func validLanguageTag(value string) bool {
	if runeLength(value) > 16 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func validDate(value string) bool {
	parsed, err := time.Parse(time.DateOnly, value)
	return err == nil && parsed.Format(time.DateOnly) == value
}

func validRFC3339(value string) bool {
	_, err := time.Parse(time.RFC3339, value)
	return err == nil
}

func validLibraryName(value string) bool {
	if !validText(value, 1, 255) || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if r == '/' || r == '\\' || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func runeLength(value string) int { return utf8.RuneCountInString(value) }

func invalidPersonalBody() error {
	return domain.InvalidArgument("body", "personal object body is invalid")
}
