package personal

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
)

const MaxMutationBodyBytes = 1 << 20

const (
	ObjectTypeProfile = "profile"
	ProfileObjectID   = "default"
)

type Mutation struct {
	OwnerID      string
	MutationID   string
	ObjectType   string
	ObjectID     string
	BaseRevision int64
	Body         json.RawMessage
	Delete       bool
	RequestHash  []byte
}

type MutationReceipt struct {
	ObjectType            string
	ObjectID              string
	ResultRevision        int64
	AccountChangeSequence int64
	Replay                bool
}

func ValidateMutation(value Mutation) error {
	if !domain.ValidUserID(value.OwnerID) {
		return domain.InvalidArgument("owner_id", "personal mutation owner is invalid")
	}
	if !validStableID(value.MutationID, 8) {
		return domain.InvalidArgument("mutation_id", "personal mutation id is invalid")
	}
	if !validObjectType(value.ObjectType) {
		return domain.InvalidArgument("object_type", "personal object type is invalid")
	}
	if err := ValidateObjectIdentifier(value.ObjectType, value.ObjectID); err != nil {
		return err
	}
	if value.BaseRevision < 0 {
		return domain.InvalidArgument("base_revision", "personal object base revision is invalid")
	}
	if value.Delete {
		if len(value.Body) != 0 {
			return domain.InvalidArgument("body", "personal object delete body must be empty")
		}
		return nil
	}
	if len(value.Body) > MaxMutationBodyBytes {
		return domain.NewError(domain.CodeRequestTooLarge, "personal object body exceeds the size limit")
	}
	return ValidateObjectBody(value.ObjectType, value.ObjectID, value.Body)
}

type profileBody struct {
	Name        string `json:"name"`
	Avatar      string `json:"avatar"`
	Emoji       string `json:"emoji"`
	Color       string `json:"color"`
	Description string `json:"description"`
	Style       string `json:"style"`
}

func validateProfileBody(body []byte) error {
	var profile profileBody
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&profile); err != nil {
		return domain.InvalidArgument("body", "profile contains an unsupported field or value")
	}
	if profile.Name == "" || strings.TrimSpace(profile.Name) != profile.Name || utf8.RuneCountInString(profile.Name) > 60 {
		return domain.InvalidArgument("body.name", "profile name must contain 1 to 60 characters")
	}
	if profile.Avatar != "dragon" && profile.Avatar != "emoji" {
		return domain.InvalidArgument("body.avatar", "profile avatar type is not enabled")
	}
	if utf8.RuneCountInString(profile.Emoji) > 16 || utf8.RuneCountInString(profile.Description) > 200 ||
		utf8.RuneCountInString(profile.Style) > 32 || !validProfileStyle(profile.Style) {
		return domain.InvalidArgument("body", "profile field exceeds its limit or has an unsupported value")
	}
	if profile.Avatar == "emoji" {
		if profile.Emoji == "" || !validProfileColor(profile.Color) {
			return domain.InvalidArgument("body", "emoji profile requires an emoji and a hex color")
		}
	} else if profile.Emoji != "" || profile.Color != "" {
		return domain.InvalidArgument("body", "dragon profile cannot include emoji or color")
	}
	return nil
}

func validProfileColor(value string) bool {
	if len(value) != 7 || value[0] != '#' {
		return false
	}
	for _, c := range value[1:] {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

func validProfileStyle(value string) bool {
	if value == "" {
		return true
	}
	switch value {
	case "muse", "flat", "clay", "watercolor", "pixel", "line", "sticker":
		return true
	default:
		return false
	}
}

// HashMutationContent excludes the stable mutation ID so the store can detect
// reuse of that ID with different object content.
func HashMutationContent(value Mutation) ([]byte, error) {
	encoded, err := json.Marshal(struct {
		ObjectType   string          `json:"object_type"`
		ObjectID     string          `json:"object_id"`
		BaseRevision int64           `json:"base_revision"`
		Body         json.RawMessage `json:"body"`
		Delete       bool            `json:"delete"`
	}{
		ObjectType: value.ObjectType, ObjectID: value.ObjectID, BaseRevision: value.BaseRevision,
		Body: value.Body, Delete: value.Delete,
	})
	if err != nil {
		return nil, fmt.Errorf("encode personal mutation content: %w", err)
	}
	hash := sha256.Sum256(encoded)
	return hash[:], nil
}

func validObjectType(value string) bool {
	if len(value) == 0 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	separator := false
	for i := 1; i < len(value); i++ {
		c := value[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			separator = false
			continue
		}
		if (c == '-' || c == '_') && !separator && i < len(value)-1 {
			separator = true
			continue
		}
		return false
	}
	return true
}

func validOpaqueID(value string) bool { return validStableID(value, 1) }

func validStableID(value string, minLength int) bool {
	if len(value) < minLength || len(value) > 128 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '.' || c == '_' || c == '~' || c == '-' {
			continue
		}
		return false
	}
	return true
}
