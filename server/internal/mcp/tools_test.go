package mcp

import "testing"

func TestMatchesNullableObject(t *testing.T) {
	if !matchesJSONType(nil, "nullable-object") {
		t.Fatal("nullable-object must accept null")
	}
	if !matchesJSONType(map[string]any{}, "nullable-object") {
		t.Fatal("nullable-object must accept objects")
	}
	if matchesJSONType("null", "nullable-object") {
		t.Fatal("nullable-object must reject strings")
	}
	if matchesJSONType(nil, "object") {
		t.Fatal("object must continue to reject null")
	}
}
