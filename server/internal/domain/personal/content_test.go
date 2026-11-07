package personal

import (
	"strings"
	"testing"
)

func TestPersonalSyncObjectTypeAllowlist(t *testing.T) {
	want := []string{
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
	got := PersonalSyncObjectTypes()
	if len(got) != len(want) {
		t.Fatalf("PersonalSyncObjectTypes() has %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("PersonalSyncObjectTypes()[%d] = %q, want %q", i, got[i], want[i])
		}
		if !IsPersonalSyncObjectType(got[i]) {
			t.Errorf("allowlisted type %q was not recognized", got[i])
		}
	}
	got[0] = "mutated"
	if PersonalSyncObjectTypes()[0] != ObjectTypeProfile {
		t.Fatal("PersonalSyncObjectTypes returned shared mutable storage")
	}
	if IsPersonalSyncObjectType("profile_extra") || IsPersonalSyncObjectType("Profile") {
		t.Fatal("unallowlisted type was recognized")
	}
}

func TestValidateObjectIdentifier(t *testing.T) {
	valid := []struct{ objectType, objectID string }{
		{ObjectTypeProfile, ProfileObjectID},
		{ObjectTypeMemory, "global"},
		{ObjectTypeMemory, "user"},
		{ObjectTypeMemory, "m_0123abcd"},
		{ObjectTypeSoul, "soul_v1"},
		{ObjectTypeDiary, "2026-11-02"},
		{ObjectTypeGoal, "g_1234"},
		{ObjectTypeSchedule, "j1234567"},
		{ObjectTypeFeedPost, "feedpost_2026-11-02-01"},
		{ObjectTypeFeedPreferences, "default"},
		{ObjectTypeIdeaMark, "train-watch"},
		{ObjectTypeLibraryItem, "library-item_1"},
		{ObjectTypeSkillSetting, "builtin_weather"},
		{ObjectTypeConnectorSetting, "catalogue_calendar"},
	}
	for _, test := range valid {
		t.Run(test.objectType+"/"+test.objectID, func(t *testing.T) {
			if err := ValidateObjectIdentifier(test.objectType, test.objectID); err != nil {
				t.Fatalf("valid identifier rejected: %v", err)
			}
		})
	}

	invalid := []struct{ objectType, objectID string }{
		{"unknown", "ok"},
		{"memory", "other"},
		{"memory", "m_0123ABCd"},
		{"memory", "m_0123abc"},
		{"profile", "other"},
		{"feed_preferences", "default2"},
		{"skill_setting", "weather"},
		{"skill_setting", "builtin_"},
		{"connector_setting", "calendar"},
		{"connector_setting", "catalogue_"},
		{"schedule", "s1234567"},
		{"diary", ".."},
		{"diary", "bad/id"},
		{"diary", "bad\\id"},
		{"diary", strings.Repeat("a", 129)},
	}
	for _, test := range invalid {
		t.Run(test.objectType+"/"+test.objectID, func(t *testing.T) {
			if err := ValidateObjectIdentifier(test.objectType, test.objectID); err == nil {
				t.Fatal("invalid identifier accepted")
			}
		})
	}
}

func TestValidateObjectBodyAcceptsAllowlistedShapesAndBoundaries(t *testing.T) {
	valid := []struct {
		objectType string
		objectID   string
		body       string
	}{
		{ObjectTypeMemory, "m_0123abcd", `{"content":"x","category":"` + strings.Repeat("c", 64) + `"}`},
		{ObjectTypeSoul, "agent_soul", `{"name":"` + strings.Repeat("n", 80) + `","emoji":"` + strings.Repeat("e", 16) + `","style":"` + strings.Repeat("s", 64) + `","lang":"en_US-123456789","content":"x"}`},
		{ObjectTypeDiary, "2026-11-02", `{"date":"2024-02-29","content":"x"}`},
		{ObjectTypeGoal, "g_1234", `{"title":"Learn Go","description":"Practice every week.","status":"active","steps":[{"id":"s_1","title":"Read the tour","status":"in_progress"}],"related_material_ids":["library_1"]}`},
		{ObjectTypeSchedule, "j1234567", `{"status":"enabled","host_id":"h1234567","model_id":"morde-default","title":"Morning report","objective":"Check a local report","timing":{"kind":"daily","starts_on":"2026-11-02","local_time":"09:00","time_zone":"Asia/Shanghai"},"budget":{"active_seconds":60,"tool_calls":2,"model_tokens":100},"misfire_policy":"skip","notification_events":["completed"]}`},
		{ObjectTypeFeedPost, "2026-11-02-01", `{"title":"` + strings.Repeat("t", 200) + `","type":"` + strings.Repeat("y", 32) + `","emoji":"` + strings.Repeat("e", 16) + `","created_at":"2026-11-02T12:30:45.123Z","liked":false,"body":"x"}`},
		{ObjectTypeFeedPreferences, "default", `{"content":"` + strings.Repeat("p", 8192) + `"}`},
		{ObjectTypeIdeaMark, "train-watch", `{"state":"saved"}`},
		{ObjectTypeIdeaMark, "train-watch", `{"state":"dismissed"}`},
		{ObjectTypeLibraryItem, "item_1", `{"name":"` + strings.Repeat("n", 255) + `","kind":"media","mime":"` + strings.Repeat("m", 128) + `","size_bytes":0}`},
		{ObjectTypeLibraryItem, "item_2", `{"name":"x","kind":"artifact","attachment_id":"pa_0123456789abcdef0123456789abcdef","mime":"application/pdf","size_bytes":20}`},
		{ObjectTypeSkillSetting, "builtin_weather", `{"enabled":false}`},
		{ObjectTypeConnectorSetting, "catalogue_calendar", `{"auth":"open","enabled":true}`},
	}
	for _, test := range valid {
		t.Run(test.objectType, func(t *testing.T) {
			if err := ValidateObjectBody(test.objectType, test.objectID, []byte(test.body)); err != nil {
				t.Fatalf("valid body rejected: %v", err)
			}
		})
	}

	if err := ValidateObjectBody(ObjectTypeMemory, "global", []byte(`{"content":"`+strings.Repeat("你", 65536)+`"}`)); err != nil {
		t.Fatalf("maximum rune length rejected: %v", err)
	}
	if err := ValidateObjectBody(ObjectTypeProfile, ProfileObjectID, []byte(`{"name":"Muse","avatar":"dragon"}`)); err != nil {
		t.Fatalf("valid profile body rejected: %v", err)
	}
}

func TestValidateObjectBodyRejectsUnknownFieldsAndMalformedJSON(t *testing.T) {
	invalid := []struct {
		name       string
		objectType string
		objectID   string
		body       string
	}{
		{"not object", ObjectTypeMemory, "global", `[]`},
		{"null", ObjectTypeMemory, "global", `null`},
		{"trailing JSON", ObjectTypeMemory, "global", `{"content":"x"}{}`},
		{"trailing garbage", ObjectTypeMemory, "global", `{"content":"x"} garbage`},
		{"missing required", ObjectTypeMemory, "global", `{}`},
		{"empty required", ObjectTypeMemory, "global", `{"content":""}`},
		{"null optional", ObjectTypeMemory, "global", `{"content":"x","category":null}`},
		{"case changed field", ObjectTypeMemory, "global", `{"Content":"x"}`},
		{"unknown field", ObjectTypeMemory, "global", `{"content":"x","owner_id":"u123"}`},
		{"SOUL icon data URI denied", ObjectTypeSoul, "soul", `{"name":"Muse","content":"x","icon":"data:image/png;base64,secret"}`},
		{"invalid lang", ObjectTypeSoul, "soul", `{"name":"Muse","lang":"en/US","content":"x"}`},
		{"invalid diary date", ObjectTypeDiary, "2026-11-02", `{"date":"2025-02-29","content":"x"}`},
		{"goal unknown field denied", ObjectTypeGoal, "g_1", `{"title":"Learn Go","status":"active","steps":[],"owner_id":"u1234567"}`},
		{"goal status invalid", ObjectTypeGoal, "g_1", `{"title":"Learn Go","status":"running","steps":[]}`},
		{"goal step status invalid", ObjectTypeGoal, "g_1", `{"title":"Learn Go","status":"active","steps":[{"id":"s_1","title":"Read","status":"running"}]}`},
		{"goal duplicate step IDs denied", ObjectTypeGoal, "g_1", `{"title":"Learn Go","status":"active","steps":[{"id":"s_1","title":"Read","status":"pending"},{"id":"s_1","title":"Practice","status":"pending"}]}`},
		{"goal duplicate material IDs denied", ObjectTypeGoal, "g_1", `{"title":"Learn Go","status":"active","steps":[],"related_material_ids":["library_1","library_1"]}`},
		{"goal path field denied", ObjectTypeGoal, "g_1", `{"title":"Learn Go","status":"active","steps":[],"path":"/private/file"}`},
		{"schedule credentials denied", ObjectTypeSchedule, "j1234567", `{"status":"disabled","host_id":"h1234567","model_id":"morde-default","title":"Morning report","objective":"Check a local report","timing":{"kind":"daily","starts_on":"2026-11-02","local_time":"09:00","time_zone":"Asia/Shanghai"},"budget":{"active_seconds":60,"tool_calls":2,"model_tokens":100},"misfire_policy":"skip","notification_events":[],"auth_token_id":"t1234567"}`},
		{"feed source denied", ObjectTypeFeedPost, "post_1", `{"title":"t","type":"note","body":"b","source":["GLOBAL.md"]}`},
		{"invalid RFC3339", ObjectTypeFeedPost, "post_1", `{"title":"t","type":"note","body":"b","created_at":"2026-11-02"}`},
		{"idea session denied", ObjectTypeIdeaMark, "idea_1", `{"state":"saved","session_id":"chat"}`},
		{"idea state invalid", ObjectTypeIdeaMark, "idea_1", `{"state":"opened"}`},
		{"library path denied", ObjectTypeLibraryItem, "file_1", `{"name":"x","kind":"artifact","path":"attachments/a"}`},
		{"library slash denied", ObjectTypeLibraryItem, "file_1", `{"name":"a/b","kind":"artifact"}`},
		{"library backslash denied", ObjectTypeLibraryItem, "file_1", `{"name":"a\\b","kind":"artifact"}`},
		{"library dot name denied", ObjectTypeLibraryItem, "file_1", `{"name":"..","kind":"artifact"}`},
		{"invalid library attachment id denied", ObjectTypeLibraryItem, "file_1", `{"name":"x","kind":"artifact","attachment_id":"a"}`},
		{"negative library size denied", ObjectTypeLibraryItem, "file_1", `{"name":"x","kind":"artifact","size_bytes":-1}`},
		{"skill field denied", ObjectTypeSkillSetting, "builtin_weather", `{"enabled":true,"config":{"url":"https://example.invalid"}}`},
		{"connector URL denied", ObjectTypeConnectorSetting, "catalogue_calendar", `{"auth":"oauth","enabled":true,"url":"https://example.invalid"}`},
		{"connector grant denied", ObjectTypeConnectorSetting, "catalogue_calendar", `{"auth":"key","enabled":true,"grant":"secret"}`},
		{"SOUL absent in profile", ObjectTypeProfile, ProfileObjectID, `{"name":"Muse","avatar":"face"}`},
		{"profile trailing JSON", ObjectTypeProfile, ProfileObjectID, `{"name":"Muse","avatar":"dragon"}{}`},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateObjectBody(test.objectType, test.objectID, []byte(test.body)); err == nil {
				t.Fatal("invalid body accepted")
			}
		})
	}

	if err := ValidateObjectBody(ObjectTypeMemory, "global", []byte(`{"content":"x"}`+strings.Repeat(" ", MaxMutationBodyBytes))); err == nil {
		t.Fatal("body larger than MaxMutationBodyBytes accepted")
	}
	if err := ValidateObjectBody(ObjectTypeMemory, "not-global", []byte(`{"content":"x"}`)); err == nil {
		t.Fatal("body with an invalid object ID accepted")
	}
}

func TestValidateObjectBodyRejectsFieldLimitAndEnumViolations(t *testing.T) {
	invalid := []struct {
		name       string
		objectType string
		objectID   string
		body       string
	}{
		{"memory content too long", ObjectTypeMemory, "global", `{"content":"` + strings.Repeat("x", 65537) + `"}`},
		{"memory category too long", ObjectTypeMemory, "global", `{"content":"x","category":"` + strings.Repeat("c", 65) + `"}`},
		{"soul name too long", ObjectTypeSoul, "soul", `{"name":"` + strings.Repeat("n", 81) + `","content":"x"}`},
		{"soul emoji too long", ObjectTypeSoul, "soul", `{"name":"Muse","emoji":"` + strings.Repeat("e", 17) + `","content":"x"}`},
		{"soul style too long", ObjectTypeSoul, "soul", `{"name":"Muse","style":"` + strings.Repeat("s", 65) + `","content":"x"}`},
		{"soul lang too long", ObjectTypeSoul, "soul", `{"name":"Muse","lang":"` + strings.Repeat("e", 17) + `","content":"x"}`},
		{"diary content too long", ObjectTypeDiary, "diary_1", `{"date":"2026-11-02","content":"` + strings.Repeat("x", 65537) + `"}`},
		{"feed title too long", ObjectTypeFeedPost, "post_1", `{"title":"` + strings.Repeat("t", 201) + `","type":"note","body":"b"}`},
		{"feed type too long", ObjectTypeFeedPost, "post_1", `{"title":"t","type":"` + strings.Repeat("y", 33) + `","body":"b"}`},
		{"feed emoji too long", ObjectTypeFeedPost, "post_1", `{"title":"t","type":"note","emoji":"` + strings.Repeat("e", 17) + `","body":"b"}`},
		{"feed body too long", ObjectTypeFeedPost, "post_1", `{"title":"t","type":"note","body":"` + strings.Repeat("b", 65537) + `"}`},
		{"feed preferences too long", ObjectTypeFeedPreferences, "default", `{"content":"` + strings.Repeat("p", 8193) + `"}`},
		{"library name too long", ObjectTypeLibraryItem, "file_1", `{"name":"` + strings.Repeat("n", 256) + `","kind":"artifact"}`},
		{"library mime too long", ObjectTypeLibraryItem, "file_1", `{"name":"x","kind":"artifact","mime":"` + strings.Repeat("m", 129) + `"}`},
		{"library kind invalid", ObjectTypeLibraryItem, "file_1", `{"name":"x","kind":"attachment"}`},
		{"library dot name denied", ObjectTypeLibraryItem, "file_1", `{"name":".","kind":"artifact"}`},
		{"library control denied", ObjectTypeLibraryItem, "file_1", "{\"name\":\"a\\u0001b\",\"kind\":\"artifact\"}"},
		{"skill enabled type invalid", ObjectTypeSkillSetting, "builtin_weather", `{"enabled":"true"}`},
		{"connector auth invalid", ObjectTypeConnectorSetting, "catalogue_calendar", `{"auth":"bearer","enabled":true}`},
		{"connector enabled type invalid", ObjectTypeConnectorSetting, "catalogue_calendar", `{"auth":"key","enabled":1}`},
		{"goal title too long", ObjectTypeGoal, "g_1", `{"title":"` + strings.Repeat("g", 201) + `","status":"active","steps":[]}`},
		{"goal step note too long", ObjectTypeGoal, "g_1", `{"title":"Learn Go","status":"active","steps":[{"id":"s_1","title":"Read","status":"done","note":"` + strings.Repeat("n", 2001) + `"}]}`},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateObjectBody(test.objectType, test.objectID, []byte(test.body)); err == nil {
				t.Fatal("invalid body accepted")
			}
		})
	}
}
