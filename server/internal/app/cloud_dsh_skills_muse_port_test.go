package app

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func TestMusePortSkillsAreEmbeddedAndUseOnlyMordeWorkspaceTools(t *testing.T) {
	toolNames := make(map[string]struct{})
	for _, definition := range cloudDSHWorkspaceToolDefinitions(0) {
		toolNames[definition.Name] = struct{}{}
	}
	for _, definition := range cloudDSHBuiltinSearchDefinitions() {
		toolNames[definition.Name] = struct{}{}
	}
	for _, definition := range cloudDSHGoalToolDefinitions() {
		toolNames[definition.Name] = struct{}{}
	}
	for _, definition := range cloudDSHScheduleToolDefinitions() {
		toolNames[definition.Name] = struct{}{}
	}
	toolNames[cloudDSHTaskAttachmentReadToolName] = struct{}{}

	mordeToolReference := regexp.MustCompile(`\bmorde_[a-z_]+\b`)
	blockedReferences := []string{
		"todo.write", "tracking.create", "cron.add", "muse.connector_search", "muse.skill_search", "browser.browse",
		"/opt/muse/", "/opt/hatch/", "~/workspace/", "Todoist", "Asana", "Google Tasks", "Notion", "Evernote",
		"Google Drive", "Dropbox", "Readwise", "Studocu",
	}

	definition := cloudDSHSkillReadDefinition()
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
		t.Fatalf("decode skill reader schema: %v", err)
	}
	allowedSkillIDs := make(map[string]struct{}, len(schema.Properties["skill_id"].Enum))
	for _, id := range schema.Properties["skill_id"].Enum {
		allowedSkillIDs[id] = struct{}{}
	}

	for _, id := range []string{"task-management", "knowledge-organization", "wide-research", "translation", "meeting-follow-up", "app-creation", "email-management", "meeting-preparation", "scheduling", "weekly-review-planning"} {
		t.Run(id, func(t *testing.T) {
			skill, ok := cloudDSHSkillForID(id)
			if !ok {
				t.Fatalf("skill %q is absent from the fixed catalog", id)
			}
			metadata, ok := CloudDSHSkillMetadataForID(id)
			wantSourceRef := "server/internal/app/cloud_dsh_skills/" + id + ".md"
			if !ok || metadata.ID != id || metadata.Version != skill.version || metadata.SourceRef != wantSourceRef {
				t.Fatalf("skill metadata is not fixed for %q: %#v", id, metadata)
			}
			if _, ok := allowedSkillIDs[id]; !ok {
				t.Fatalf("skill %q is not in the read tool's fixed enum", id)
			}

			content, err := cloudDSHSkillBundle.ReadFile(skill.resource)
			if err != nil || len(content) == 0 || !strings.Contains(string(content), "author: Morde") {
				t.Fatalf("embedded skill %q is unreadable: %v", id, err)
			}
			text := string(content)
			for _, name := range []string{
				cloudDSHWorkspaceListToolName,
				cloudDSHWorkspaceReadToolName,
				cloudDSHWorkspaceSearchToolName,
				cloudDSHWorkspaceWriteToolName,
			} {
				if !strings.Contains(text, name) {
					t.Errorf("skill %q does not describe available tool %q", id, name)
				}
			}
			if id == "task-management" && !strings.Contains(text, cloudDSHTaskAttachmentReadToolName) {
				t.Errorf("task-management skill omits the authorized current-task attachment reader")
			}
			if id == "translation" && (!strings.Contains(text, cloudDSHTaskAttachmentReadToolName) ||
				!strings.Contains(text, "Do not send user material") || !strings.Contains(text, "Markdown only")) {
				t.Errorf("translation does not state its source and output limits")
			}
			if id == "meeting-follow-up" && (!strings.Contains(text, cloudDSHTaskAttachmentReadToolName) ||
				!strings.Contains(text, "does not read calendars") || !strings.Contains(text, "create external tasks")) {
				t.Errorf("meeting-follow-up does not state its source and action limits")
			}
			if id == "email-management" && (!strings.Contains(text, "does not grant mailbox access") ||
				!strings.Contains(text, cloudDSHTaskAttachmentReadToolName) || !strings.Contains(text, "Send only when the user explicitly requests")) {
				t.Errorf("email-management does not enforce source and send authorization limits")
			}
			if id == "scheduling" && (!strings.Contains(text, "does not grant calendar access") ||
				!strings.Contains(text, "label every option provisional") || !strings.Contains(text, "daylight-saving")) {
				t.Errorf("scheduling does not preserve availability and time-zone uncertainty")
			}
			if id == "meeting-preparation" && (!strings.Contains(text, cloudDSHTaskAttachmentReadToolName) ||
				!strings.Contains(text, "do not guess from Morde recurring schedules") ||
				!strings.Contains(text, "Do not send invitations")) {
				t.Errorf("meeting-preparation confuses schedules with calendar events or permits unrequested writes")
			}
			if id == "weekly-review-planning" && (!strings.Contains(text, cloudDSHTaskAttachmentReadToolName) ||
				!strings.Contains(text, "not calendar events") ||
				!strings.Contains(text, "Do not overwrite an existing review")) {
				t.Errorf("weekly-review-planning confuses Morde schedules or overwrites prior reviews")
			}
			if id == "app-creation" {
				for _, name := range []string{runtimeToolShellExec, runtimeToolFileRead, runtimeToolFileWrite,
					runtimeToolFileList, runtimeToolFileFind, runtimeToolBrowserOpenTab, runtimeToolBrowserSnapshot} {
					if !strings.Contains(text, name) {
						t.Errorf("app-creation does not condition execution on %q", name)
					}
					found := false
					for _, mapping := range runtimeToolMappings {
						if mapping.name == name {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("app-creation references runtime tool %q outside the registered map", name)
					}
				}
			}
			if id == "wide-research" && (!strings.Contains(text, "dsh_delegate") ||
				!strings.Contains(text, "dsh_job_list") || !strings.Contains(text, "dsh_job_get") ||
				!strings.Contains(text, "dsh_job_output") || !strings.Contains(text, "no more than 16 child jobs")) {
				t.Errorf("wide-research skill does not follow the bounded DSH delegation contract")
			}

			for _, match := range mordeToolReference.FindAllString(text, -1) {
				if _, ok := toolNames[match]; !ok {
					t.Errorf("skill %q refers to an unavailable Morde tool %q", id, match)
				}
			}
			for _, reference := range blockedReferences {
				if strings.Contains(strings.ToLower(text), strings.ToLower(reference)) {
					t.Errorf("skill %q contains Muse-private or unconnected reference %q", id, reference)
				}
			}
		})
	}
	for _, id := range []string{"inbox-triage", "meeting-prep", "weekly-review"} {
		if _, ok := allowedSkillIDs[id]; ok {
			t.Errorf("legacy Muse skill %q remains available to new Tasks", id)
		}
	}
}
