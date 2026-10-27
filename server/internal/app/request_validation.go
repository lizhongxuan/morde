package app

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"

	"morde/server/internal/domain"
)

const (
	defaultPageSize int32 = 50
	maxPageSize     int32 = 100
)

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{8,128}$`)

func validateIdempotencyKey(value string) error {
	if !idempotencyKeyPattern.MatchString(value) {
		return domain.InvalidArgument("idempotency_key", "idempotency key is invalid")
	}
	return nil
}

func idempotencyRecord(ownerID, operation, key string, request any, resourceID, relatedID string) (IdempotencyRecord, error) {
	hash, err := hashIdempotencyRequest(request)
	if err != nil {
		return IdempotencyRecord{}, err
	}
	return IdempotencyRecord{OwnerID: ownerID, Operation: operation, Key: key, RequestHash: hash, ResourceID: resourceID, RelatedID: relatedID}, nil
}

func hashIdempotencyRequest(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode idempotency request: %w", err)
	}
	hash := sha256.Sum256(encoded)
	return hash[:], nil
}

func validatePageRequest(page PageRequest) error {
	if page.Limit == 0 {
		page.Limit = defaultPageSize
	}
	if page.Limit < 1 || page.Limit > maxPageSize || len(page.Cursor) > 1024 {
		return domain.InvalidArgument("limit", "page request is out of range")
	}
	return nil
}
