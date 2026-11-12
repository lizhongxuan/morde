package app

import (
	"encoding/base64"
	"time"
)

// EncodePageCursor encodes an exclusive cursor using the shared persisted page
// cursor format. Services use it when they must advance by a visible record.
func EncodePageCursor(createdAt time.Time, id string) string {
	value := createdAt.UTC().Format(time.RFC3339Nano) + "\n" + id
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}
