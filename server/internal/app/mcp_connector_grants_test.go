package app

import (
	"crypto/sha256"
	"testing"

	"morde/server/internal/domain"
)

func TestNormalizeTaskMCPConnectorIDs(t *testing.T) {
	got, err := normalizeTaskMCPConnectorIDs([]string{"kzzzzzzz", "kaaaaaaa"})
	if err != nil || len(got) != 2 || got[0] != "kaaaaaaa" || got[1] != "kzzzzzzz" {
		t.Fatalf("normalizeTaskMCPConnectorIDs() = %#v, %v", got, err)
	}
	for name, values := range map[string][]string{
		"duplicate": {"kaaaaaaa", "kaaaaaaa"},
		"malformed": {"bad-id"},
		"too many":  makeConnectorIDs(maxTaskMCPConnectorGrants + 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeTaskMCPConnectorIDs(values); domain.CodeOf(err) != domain.CodeInvalidArgument {
				t.Fatalf("normalizeTaskMCPConnectorIDs(%#v) error = %v", values, err)
			}
		})
	}
}

func TestValidateTaskMCPConnectorToolSchema(t *testing.T) {
	schema := []byte(`[{"name":"read_file"}]`)
	digest := sha256.Sum256(schema)
	if err := validateTaskMCPConnectorToolSchema(schema, digest[:]); err != nil {
		t.Fatalf("valid schema rejected: %v", err)
	}
	if err := validateTaskMCPConnectorToolSchema(schema, make([]byte, sha256.Size)); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("schema with wrong digest error = %v", err)
	}
	if err := validateTaskMCPConnectorToolSchema([]byte(`{`), digest[:]); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("malformed schema error = %v", err)
	}
}

func makeConnectorIDs(count int) []string {
	ids := make([]string, count)
	for i := range ids {
		ids[i] = "k0000000"
		ids[i] = string([]byte{'k', 'a', 'a', 'a', 'a', 'a', byte('a' + i%26), 'a'})
	}
	return ids
}
