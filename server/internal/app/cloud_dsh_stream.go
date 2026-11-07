package app

import (
	"encoding/base64"
	"sort"

	"morde/server/internal/domain"
	"morde/server/internal/domain/personal"
)

// CloudDSHStreamPersonalProof is the minimum personal-data evidence needed
// to fence an incremental reply in the same transaction as its persistence.
type CloudDSHStreamPersonalProof struct {
	Enabled         bool
	SettingRevision int64
	Objects         []CloudDSHMemoryVersion
}

type CloudDSHTextDelta struct {
	Fence         ModelTaskFence
	CallID        string
	MessageID     string
	TurnID        string
	Text          string
	PersonalProof *CloudDSHStreamPersonalProof
}

// DecodeCloudDSHStreamPersonalProof applies the same strict version, scope and
// revision rules as personal-context validation. Only broker-confirmed writes
// may be supplied as additional versions.
func DecodeCloudDSHStreamPersonalProof(encoded string, additional []CloudDSHMemoryVersion) (CloudDSHStreamPersonalProof, error) {
	invalid := func() (CloudDSHStreamPersonalProof, error) {
		return CloudDSHStreamPersonalProof{}, domain.NewError(domain.CodeInvalidState, "personal context cannot be verified")
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(data) > 8192 {
		return invalid()
	}
	var proof cloudDSHMemoryProof
	if decodeStrictRuntimeResult(data, &proof) != nil || proof.Version != 2 || proof.Objects == nil || proof.Revision < 0 ||
		len(proof.Items) > cloudDSHPersonalContextItems || len(proof.Objects) > cloudDSHPersonalContextItems+20 || len(additional) > 100 ||
		(!proof.Enabled && len(proof.Items) != 0) {
		return invalid()
	}
	type key struct{ objectType, id string }
	versions := make(map[key]int64)
	register := func(value CloudDSHMemoryVersion, expected string) bool {
		objectType := value.ObjectType
		if objectType == "" {
			objectType = personal.ObjectTypeMemory
		}
		if expected != "" && objectType != expected {
			return false
		}
		switch objectType {
		case personal.ObjectTypeMemory, personal.ObjectTypeProfile, personal.ObjectTypeSoul, personal.ObjectTypeGoal:
		default:
			return false
		}
		if value.Revision < 1 || personal.ValidateObjectIdentifier(objectType, value.ID) != nil ||
			objectType == personal.ObjectTypeMemory && !proof.Enabled {
			return false
		}
		identity := key{objectType: objectType, id: value.ID}
		if value.Revision > versions[identity] {
			versions[identity] = value.Revision
		}
		return true
	}
	for _, item := range proof.Items {
		if !register(item, personal.ObjectTypeMemory) {
			return invalid()
		}
	}
	for _, item := range proof.Objects {
		if item.ObjectType == "" || item.ObjectType == personal.ObjectTypeMemory || !register(item, "") {
			return invalid()
		}
	}
	for _, item := range additional {
		if !register(item, "") {
			return invalid()
		}
	}
	result := CloudDSHStreamPersonalProof{Enabled: proof.Enabled, SettingRevision: proof.Revision,
		Objects: make([]CloudDSHMemoryVersion, 0, len(versions))}
	for identity, revision := range versions {
		result.Objects = append(result.Objects, CloudDSHMemoryVersion{ObjectType: identity.objectType, ID: identity.id, Revision: revision})
	}
	sort.Slice(result.Objects, func(i, j int) bool {
		if result.Objects[i].ObjectType != result.Objects[j].ObjectType {
			return result.Objects[i].ObjectType < result.Objects[j].ObjectType
		}
		return result.Objects[i].ID < result.Objects[j].ID
	})
	return result, nil
}
