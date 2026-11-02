package personal

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateMutation(t *testing.T) {
	valid := Mutation{
		OwnerID: "u1234567", MutationID: "mutation-123", ObjectType: ObjectTypeProfile,
		ObjectID: ProfileObjectID, BaseRevision: 0, Body: json.RawMessage(`{"name":"Muse","avatar":"dragon"}`),
	}
	if err := ValidateMutation(valid); err != nil {
		t.Fatalf("valid upsert: %v", err)
	}
	delete := valid
	delete.Delete, delete.Body = true, nil
	if err := ValidateMutation(delete); err != nil {
		t.Fatalf("valid delete: %v", err)
	}

	invalid := []struct {
		name   string
		change func(*Mutation)
	}{
		{"owner", func(value *Mutation) { value.OwnerID = "bad" }},
		{"mutation id", func(value *Mutation) { value.MutationID = "short" }},
		{"non-ascii mutation id", func(value *Mutation) { value.MutationID = "mutation-你好" }},
		{"object type", func(value *Mutation) { value.ObjectType = "Profile" }},
		{"object type separator", func(value *Mutation) { value.ObjectType = "profile--data" }},
		{"unsupported object type", func(value *Mutation) { value.ObjectType = "goal" }},
		{"object id", func(value *Mutation) { value.ObjectID = "bad/id" }},
		{"non-default profile id", func(value *Mutation) { value.ObjectID = "other" }},
		{"object id too long", func(value *Mutation) { value.ObjectID = strings.Repeat("a", 129) }},
		{"negative revision", func(value *Mutation) { value.BaseRevision = -1 }},
		{"empty body", func(value *Mutation) { value.Body = nil }},
		{"malformed body", func(value *Mutation) { value.Body = json.RawMessage(`{"key":`) }},
		{"non-object body", func(value *Mutation) { value.Body = json.RawMessage(`[]`) }},
		{"null body", func(value *Mutation) { value.Body = json.RawMessage(`null`) }},
		{"multiple values", func(value *Mutation) { value.Body = json.RawMessage(`{} {}`) }},
		{"unknown profile field", func(value *Mutation) {
			value.Body = json.RawMessage(`{"name":"Muse","avatar":"dragon","api_key":"secret"}`)
		}},
		{"connector configuration", func(value *Mutation) {
			value.Body = json.RawMessage(`{"name":"Muse","avatar":"dragon","connectors":[]}`)
		}},
		{"face requires attachment service", func(value *Mutation) { value.Body = json.RawMessage(`{"name":"Muse","avatar":"face"}`) }},
		{"missing avatar", func(value *Mutation) { value.Body = json.RawMessage(`{"name":"Muse"}`) }},
		{"empty profile name", func(value *Mutation) { value.Body = json.RawMessage(`{"name":"","avatar":"dragon"}`) }},
		{"name too long", func(value *Mutation) {
			value.Body = json.RawMessage(`{"name":"` + strings.Repeat("a", 61) + `","avatar":"dragon"}`)
		}},
		{"emoji too long", func(value *Mutation) {
			value.Body = json.RawMessage(`{"name":"Muse","avatar":"emoji","emoji":"` + strings.Repeat("x", 17) + `","color":"#0064d4"}`)
		}},
		{"description too long", func(value *Mutation) {
			value.Body = json.RawMessage(`{"name":"Muse","avatar":"dragon","description":"` + strings.Repeat("x", 201) + `"}`)
		}},
		{"invalid color", func(value *Mutation) {
			value.Body = json.RawMessage(`{"name":"Muse","avatar":"emoji","emoji":"✨","color":"red"}`)
		}},
		{"invalid style", func(value *Mutation) {
			value.Body = json.RawMessage(`{"name":"Muse","avatar":"dragon","style":"custom"}`)
		}},
		{"dragon with hidden emoji data", func(value *Mutation) {
			value.Body = json.RawMessage(`{"name":"Muse","avatar":"dragon","emoji":"✨","color":"#0064d4"}`)
		}},
		{"oversized body", func(value *Mutation) {
			value.Body = json.RawMessage(`{"name":"` + strings.Repeat("a", MaxMutationBodyBytes) + `","avatar":"dragon"}`)
		}},
		{"delete body", func(value *Mutation) { value.Delete = true }},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			value := valid
			test.change(&value)
			if err := ValidateMutation(value); err == nil {
				t.Fatal("invalid mutation was accepted")
			}
		})
	}
}

func TestValidateProfileMutationAcceptsFieldBoundaries(t *testing.T) {
	body, err := json.Marshal(profileBody{
		Name: strings.Repeat("你", 60), Avatar: "emoji", Emoji: strings.Repeat("🌟", 16),
		Color: "#aBc123", Description: strings.Repeat("描", 200), Style: "sticker",
	})
	if err != nil {
		t.Fatal(err)
	}
	value := Mutation{
		OwnerID: "u1234567", MutationID: "mutation-123", ObjectType: ObjectTypeProfile,
		ObjectID: ProfileObjectID, Body: body,
	}
	if err := ValidateMutation(value); err != nil {
		t.Fatalf("valid profile at character limits: %v", err)
	}
}

func TestValidateMutationAcceptsAllowlistedMemoryAndRejectsUnknownContent(t *testing.T) {
	mutation := Mutation{
		OwnerID: "u1234567", MutationID: "memory-write-1", ObjectType: ObjectTypeMemory,
		ObjectID: "global", Body: json.RawMessage(`{"content":"A user-selected memory.","category":"fact"}`),
	}
	if err := ValidateMutation(mutation); err != nil {
		t.Fatalf("allowlisted memory mutation rejected: %v", err)
	}
	for _, body := range []string{
		`{"content":"memory","api_key":"secret"}`,
		`{"content":"memory","path":"/private/file"}`,
	} {
		mutation.Body = json.RawMessage(body)
		if err := ValidateMutation(mutation); err == nil {
			t.Errorf("memory body accepted an unknown field: %s", body)
		}
	}
}

func TestHashMutationContentExcludesMutationID(t *testing.T) {
	value := Mutation{
		ObjectType: ObjectTypeProfile, ObjectID: ProfileObjectID, BaseRevision: 4,
		Body: json.RawMessage(`{"name":"Muse","avatar":"dragon"}`),
	}
	first, err := HashMutationContent(value)
	if err != nil {
		t.Fatal(err)
	}
	value.MutationID = "a-different-mutation-id"
	second, err := HashMutationContent(value)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("mutation ID changed content hash: %x, %x, %v", first, second, err)
	}
	value.BaseRevision++
	third, err := HashMutationContent(value)
	if err != nil || bytes.Equal(first, third) {
		t.Fatalf("content change did not change hash: %x, %x, %v", first, third, err)
	}
}
