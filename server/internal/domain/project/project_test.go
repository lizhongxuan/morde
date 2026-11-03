package project

import (
	"strings"
	"testing"

	"morde/server/internal/domain"
)

func TestProjectIDAndRoleContracts(t *testing.T) {
	id, err := NewID()
	if err != nil || !ValidID(id) {
		t.Fatalf("NewID() = %q, %v", id, err)
	}
	for _, invalid := range []string{"", "u1234567", "j123456", "j123456_", "J1234567"} {
		if ValidID(invalid) {
			t.Errorf("ValidID(%q) = true", invalid)
		}
	}
	if !RoleAtLeast(RoleOwner, RoleViewer) || !RoleAtLeast(RoleMember, RoleViewer) || RoleAtLeast(RoleViewer, RoleMember) || RoleAtLeast(RoleOwner, Role("invalid")) {
		t.Fatal("project role ordering is incorrect")
	}
}

func TestNewProjectValidatesAndTrimsUserFields(t *testing.T) {
	value, err := New("j1234567", "u1234567", "  Product  ", "  Shared context  ")
	if err != nil || value.Name != "Product" || value.Description != "Shared context" || value.Version != 1 {
		t.Fatalf("New() = %+v, %v", value, err)
	}
	for _, test := range []Project{
		{ID: "bad", OwnerID: "u1234567", Name: "Project", Version: 1},
		{ID: "j1234567", OwnerID: "bad", Name: "Project", Version: 1},
		{ID: "j1234567", OwnerID: "u1234567", Name: " ", Version: 1},
		{ID: "j1234567", OwnerID: "u1234567", Name: strings.Repeat("x", 121), Version: 1},
		{ID: "j1234567", OwnerID: "u1234567", Name: "Project", Version: 0},
	} {
		if err := test.Validate(); err == nil {
			t.Errorf("Validate(%+v) accepted", test)
		}
	}
}

func TestValidateContentBodyRejectsSecretFieldsAndPersonalReferences(t *testing.T) {
	valid := []struct {
		kind ContentKind
		id   string
		body string
	}{
		{ContentMemory, "m_0123abcd", `{"content":"Shared project memory","category":"decision"}`},
		{ContentGoal, "g_1234", `{"title":"Ship release","description":"","status":"active","steps":[]}`},
		{ContentDocument, "brief-1", `{"title":"Brief","content":"Project context","tags":["product"]}`},
	}
	for _, test := range valid {
		if err := ValidateContentBody(test.kind, test.id, []byte(test.body)); err != nil {
			t.Errorf("ValidateContentBody(%q) rejected valid content: %v", test.kind, err)
		}
	}

	invalid := []struct {
		name string
		kind ContentKind
		id   string
		body string
	}{
		{"memory connector secret field", ContentMemory, "m_0123abcd", `{"content":"Shared","connector_secret":"secret"}`},
		{"memory too long", ContentMemory, "m_0123abcd", `{"content":"` + strings.Repeat("m", 65537) + `"}`},
		{"goal personal material reference", ContentGoal, "g_1234", `{"title":"Ship release","status":"active","steps":[],"related_material_ids":["library_1"]}`},
		{"goal invalid status", ContentGoal, "g_1234", `{"title":"Ship release","status":"running","steps":[]}`},
		{"goal too long", ContentGoal, "g_1234", `{"title":"` + strings.Repeat("g", 201) + `","status":"active","steps":[]}`},
		{"document credential field", ContentDocument, "brief-1", `{"title":"Brief","content":"Project context","api_key":"secret"}`},
		{"document nested connector object", ContentDocument, "brief-1", `{"title":"Brief","content":"Project context","connector":{"secret":"secret"}}`},
		{"document too long", ContentDocument, "brief-1", `{"title":"Brief","content":"` + strings.Repeat("d", 65537) + `"}`},
		{"document duplicate tag", ContentDocument, "brief-1", `{"title":"Brief","content":"Project context","tags":["product","product"]}`},
		{"content id invalid for memory", ContentMemory, "not-global", `{"content":"Shared"}`},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateContentBody(test.kind, test.id, []byte(test.body)); err == nil || domain.CodeOf(err) != domain.CodeInvalidArgument {
				t.Fatalf("ValidateContentBody() error = %v", err)
			}
		})
	}
}

func TestMuseContentKindsValidateProjectScopedContracts(t *testing.T) {
	valid := []struct {
		kind ContentKind
		id   string
		body string
	}{
		{ContentFeedPost, "post-1", `{"title":"Release","type":"note","created_at":"2026-01-02T03:04:05Z","liked":false,"body":"Project update"}`},
		{ContentIdea, "idea-1", `{"title":"Reduce onboarding steps","content":"Test a shorter flow","tags":["product"]}`},
		{ContentLibrary, "library-1", `{"name":"Review checklist.pdf","kind":"artifact","mime":"application/pdf","size_bytes":120}`},
		{ContentBrief, "brief-1", `{"title":"Weekly brief","summary":"Progress","content":"Project-only summary","tags":["weekly"]}`},
	}
	for _, test := range valid {
		if !ValidMuseContentKind(test.kind) {
			t.Errorf("ValidMuseContentKind(%q) = false", test.kind)
		}
		if err := ValidateContentBody(test.kind, test.id, []byte(test.body)); err != nil {
			t.Errorf("ValidateContentBody(%q) rejected valid body: %v", test.kind, err)
		}
	}
	for _, test := range []struct {
		kind ContentKind
		id   string
		body string
	}{
		{ContentIdea, "idea-1", `{"title":"Idea","content":"Body","tags":["same","same"]}`},
		{ContentLibrary, "library-1", `{"name":"Personal file","kind":"artifact","attachment_id":"a1234567"}`},
		{ContentLibrary, "library-1", `{"name":"Personal file","kind":"artifact","personal_object_id":"library_item-1"}`},
		{ContentBrief, "brief-1", `{"title":"Brief","content":"Body","connector":{"token":"secret"}}`},
		{ContentFeedPost, "post-1", `{"title":"Update","type":"note","body":"Body","attachment_id":"a1234567"}`},
	} {
		if err := ValidateContentBody(test.kind, test.id, []byte(test.body)); err == nil {
			t.Errorf("ValidateContentBody(%q, %q, %s) accepted invalid project content", test.kind, test.id, test.body)
		}
	}
	if ValidContentKind(ContentKind("profile")) || ValidMuseContentKind(ContentMemory) {
		t.Fatal("personal content scope leaked into project Muse content kinds")
	}
}

func TestProjectContentCommentAndSearchValidation(t *testing.T) {
	comment := ContentComment{
		ProjectID: "j1234567", Kind: ContentBrief, ContentID: "brief-1", ContentRevision: 3,
		RowID: "section-2", ID: "c1234567", Revision: 1, Body: "Please verify this row.", UpdatedBy: "u1234567",
	}
	if err := ValidateContentComment(comment); err != nil {
		t.Fatalf("valid project content comment rejected: %v", err)
	}
	if id, err := NewCommentID(); err != nil || !ValidCommentID(id) {
		t.Fatalf("NewCommentID() = %q, %v", id, err)
	}
	for _, invalid := range []ContentComment{
		{ProjectID: "j1234567", Kind: ContentBrief, ContentID: "brief-1", ContentRevision: 0, RowID: "row-1", ID: "c1234567", Revision: 1, Body: "comment", UpdatedBy: "u1234567"},
		{ProjectID: "j1234567", Kind: ContentMemory, ContentID: "global", ContentRevision: 1, RowID: "row-1", ID: "c1234567", Revision: 1, Body: "comment", UpdatedBy: "u1234567"},
		{ProjectID: "j1234567", Kind: ContentBrief, ContentID: "brief-1", ContentRevision: 1, RowID: "row-1", ID: "c1234567", Revision: 1, Body: strings.Repeat("x", MaxContentCommentBodyRunes+1), UpdatedBy: "u1234567"},
	} {
		if err := ValidateContentComment(invalid); err == nil {
			t.Errorf("ValidateContentComment(%+v) accepted invalid comment", invalid)
		}
	}
	for _, query := range []string{"project", "équipe"} {
		if err := ValidateContentSearchQuery(query); err != nil {
			t.Errorf("ValidateContentSearchQuery(%q) = %v", query, err)
		}
	}
	for _, query := range []string{"", " padded ", strings.Repeat("x", MaxContentSearchQueryRunes+1), "bad\nquery"} {
		if err := ValidateContentSearchQuery(query); err == nil {
			t.Errorf("ValidateContentSearchQuery(%q) accepted invalid input", query)
		}
	}
}

func TestValidateContentBodySizeAndIdentifierLimits(t *testing.T) {
	tooLarge := []byte(`{"title":"Brief","content":"` + strings.Repeat("x", 1<<20) + `"}`)
	if err := ValidateContentBody(ContentDocument, "brief-1", tooLarge); domain.CodeOf(err) != domain.CodeRequestTooLarge {
		t.Fatalf("oversize content error = %v", err)
	}
	if ValidContentID(strings.Repeat("x", 129)) || ValidContentID(" bad ") || ValidContentID("x\n") {
		t.Fatal("invalid project content ID accepted")
	}
}
