package personal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

func TestSnapshotCursorRoundTripAndRejectsTampering(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	want := SnapshotCursor{
		Version: SnapshotCursorVersion, OwnerID: "u1234567", ServiceInstanceID: "instance-1",
		ObjectType: SnapshotScopeObjectType, ObjectID: SnapshotScopeObjectID, Watermark: 12,
		AfterObjectType: ObjectTypeMemory, AfterObjectID: "global",
	}
	encoded, err := EncodeSnapshotCursor(want, key)
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}
	got, err := DecodeSnapshotCursor(encoded, key)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded cursor = %+v, %v; want %+v", got, err, want)
	}
	if _, err := DecodeSnapshotCursor(encoded, []byte("short")); err == nil {
		t.Fatal("cursor decoded with an invalid key")
	}

	parts := strings.Split(encoded, ".")
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	signature[0] ^= 1
	parts[1] = base64.RawURLEncoding.EncodeToString(signature)
	if _, err := DecodeSnapshotCursor(strings.Join(parts, "."), key); err == nil {
		t.Fatal("tampered cursor signature was accepted")
	}
}

func TestSnapshotCursorRejectsMalformedTokensAndInvalidCursors(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	valid := SnapshotCursor{
		Version: SnapshotCursorVersion, OwnerID: "u1234567", ServiceInstanceID: "instance-1",
		ObjectType: SnapshotScopeObjectType, ObjectID: SnapshotScopeObjectID,
	}
	if _, err := EncodeSnapshotCursor(valid, []byte("short")); err == nil {
		t.Fatal("cursor encoded with an invalid key")
	}

	invalidCursors := []SnapshotCursor{
		{OwnerID: valid.OwnerID, ServiceInstanceID: valid.ServiceInstanceID, ObjectType: valid.ObjectType, ObjectID: valid.ObjectID},
		{Version: valid.Version, ServiceInstanceID: valid.ServiceInstanceID, ObjectType: valid.ObjectType, ObjectID: valid.ObjectID},
		{Version: valid.Version, OwnerID: strings.Repeat("u", 129), ServiceInstanceID: valid.ServiceInstanceID, ObjectType: valid.ObjectType, ObjectID: valid.ObjectID},
		{Version: valid.Version, OwnerID: valid.OwnerID, ServiceInstanceID: strings.Repeat("i", 65), ObjectType: valid.ObjectType, ObjectID: valid.ObjectID},
		{Version: valid.Version, OwnerID: valid.OwnerID, ServiceInstanceID: valid.ServiceInstanceID, ObjectType: "message", ObjectID: valid.ObjectID},
		{Version: valid.Version, OwnerID: valid.OwnerID, ServiceInstanceID: valid.ServiceInstanceID, ObjectType: valid.ObjectType, ObjectID: "other"},
		{Version: valid.Version, OwnerID: valid.OwnerID, ServiceInstanceID: valid.ServiceInstanceID, ObjectType: valid.ObjectType, ObjectID: valid.ObjectID, Watermark: -1},
		{Version: valid.Version, OwnerID: valid.OwnerID, ServiceInstanceID: valid.ServiceInstanceID, ObjectType: valid.ObjectType, ObjectID: valid.ObjectID, AfterObjectType: valid.ObjectType},
		{Version: valid.Version, OwnerID: valid.OwnerID, ServiceInstanceID: valid.ServiceInstanceID, ObjectType: valid.ObjectType, ObjectID: valid.ObjectID, AfterObjectID: valid.ObjectID},
		{Version: valid.Version, OwnerID: valid.OwnerID, ServiceInstanceID: valid.ServiceInstanceID, ObjectType: "message", AfterObjectID: "c1234567"},
		{Version: valid.Version, OwnerID: valid.OwnerID, ServiceInstanceID: valid.ServiceInstanceID, ObjectType: valid.ObjectType, ObjectID: valid.ObjectID, AfterObjectType: ObjectTypeMemory, AfterObjectID: "bad/id"},
	}
	for i, cursor := range invalidCursors {
		if _, err := EncodeSnapshotCursor(cursor, key); err == nil {
			t.Errorf("invalid cursor %d was accepted", i)
		}
	}

	badTokens := []string{"", strings.Repeat("a", maxSnapshotCursorBytes+1), "missing-separator", "%%.AA"}
	for _, token := range badTokens {
		if _, err := DecodeSnapshotCursor(token, key); err == nil {
			t.Errorf("malformed cursor %q was accepted", token)
		}
	}
	for _, token := range []string{"e30.AA", "e30.%%%", signSnapshotCursorTestPayload(t, []byte("{"), key),
		signSnapshotCursorTestPayload(t, []byte(`{"v":1,"extra":true}`), key),
		signSnapshotCursorTestPayload(t, []byte(`{} {}`), key),
		signSnapshotCursorTestPayload(t, []byte(`{"v":1}`), key),
	} {
		if _, err := DecodeSnapshotCursor(token, key); err == nil {
			t.Errorf("invalid signed cursor %q was accepted", token)
		}
	}
}

func TestMemoryHistoryCursorBindsOwnerAndObjectAndRejectsTampering(t *testing.T) {
	key := []byte(strings.Repeat("h", 32))
	want := MemoryHistoryCursor{
		Version: MemoryHistoryCursorVersion, OwnerID: "u1234567", ServiceInstanceID: "00000000-0000-4000-8000-000000000001",
		ObjectType: ObjectTypeMemory, ObjectID: "global", AfterRevision: 5,
	}
	encoded, err := EncodeMemoryHistoryCursor(want, key)
	if err != nil {
		t.Fatalf("encode memory history cursor: %v", err)
	}
	got, err := DecodeMemoryHistoryCursor(encoded, key)
	if err != nil || got != want {
		t.Fatalf("decoded memory history cursor = %+v, %v; want %+v", got, err, want)
	}
	if _, err := DecodeMemoryHistoryCursor(encoded, []byte("short")); err == nil {
		t.Fatal("cursor decoded with an invalid signing key")
	}
	parts := strings.Split(encoded, ".")
	parts[0] = base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"owner_id":"u7654321"}`))
	if _, err := DecodeMemoryHistoryCursor(strings.Join(parts, "."), key); err == nil {
		t.Fatal("cursor accepted a changed payload without a matching signature")
	}
	parts = strings.Split(encoded, ".")
	replacement := "A"
	if parts[1][len(parts[1])-1] == 'A' {
		replacement = "B"
	}
	parts[1] = parts[1][:len(parts[1])-1] + replacement
	if _, err := DecodeMemoryHistoryCursor(strings.Join(parts, "."), key); err == nil {
		t.Fatal("cursor accepted a changed signature")
	}
	if _, err := EncodeMemoryHistoryCursor(MemoryHistoryCursor{
		Version: MemoryHistoryCursorVersion, OwnerID: want.OwnerID, ServiceInstanceID: want.ServiceInstanceID,
		ObjectType: ObjectTypeMemory, ObjectID: want.ObjectID, AfterRevision: 0,
	}, key); err == nil {
		t.Fatal("cursor accepted an invalid revision boundary")
	}
}

func signSnapshotCursorTestPayload(t *testing.T, payload, dataKey []byte) string {
	t.Helper()
	key, err := deriveSnapshotCursorKey(dataKey)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
