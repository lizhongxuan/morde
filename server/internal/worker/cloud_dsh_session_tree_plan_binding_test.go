package worker

import (
	"encoding/json"
	"strings"
	"testing"

	"morde/server/internal/app"
)

func TestCloudDSHDelegatePlanSelectorsAreExposedAndStrippedFromBusinessArguments(t *testing.T) {
	base := []app.AgentToolDefinition{cloudDSHReadTool()}
	delegate := cloudDSHDelegateToolWithPlanSelectors(base)
	if !validCloudDSHToolDefinitions([]app.AgentToolDefinition{delegate}) {
		t.Fatalf("plan-aware delegate definition is invalid: %s", delegate.Parameters)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(delegate.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{"morde_plan_step_id", "morde_plan_revision"} {
		if len(schema.Properties[selector]) == 0 {
			t.Fatalf("delegate schema omitted %s: %s", selector, delegate.Parameters)
		}
		for _, required := range schema.Required {
			if required == selector {
				t.Fatalf("plan selector %s must remain optional for no-plan tasks", selector)
			}
		}
	}
	if !strings.Contains(delegate.Description, "exact morde_plan_step_id and morde_plan_revision") {
		t.Fatalf("delegate description does not explain plan selector requirement: %q", delegate.Description)
	}

	const raw = `{"title":"Collect","objective":"Record source notes","tool_names":["file_read"],"morde_plan_step_id":"collect_sources","morde_plan_revision":7}`
	arguments, definitions, stepID, planRevision, business, err := validateCloudDSHDelegateArgumentsWithPlanSelection([]byte(raw), base)
	if err != nil || arguments.Title != "Collect" || arguments.Objective != "Record source notes" ||
		stepID != "collect_sources" || planRevision != 7 || len(definitions) != 1 || definitions[0].Name != "file_read" {
		t.Fatalf("validated plan-aware delegation = args=%+v definitions=%+v step=%q revision=%d err=%v",
			arguments, definitions, stepID, planRevision, err)
	}
	var businessFields map[string]json.RawMessage
	if err := json.Unmarshal(business, &businessFields); err != nil {
		t.Fatal(err)
	}
	if _, exists := businessFields["morde_plan_step_id"]; exists {
		t.Fatalf("plan selector leaked into child business arguments: %s", business)
	}
	if _, exists := businessFields["morde_plan_revision"]; exists {
		t.Fatalf("plan revision leaked into child business arguments: %s", business)
	}
}

func TestCloudDSHDelegatePlanSelectorsRejectPartialOrInvalidSelection(t *testing.T) {
	base := []app.AgentToolDefinition{cloudDSHReadTool()}
	for _, raw := range []string{
		`{"title":"Inspect","objective":"Read","tool_names":["file_read"],"morde_plan_step_id":"inspect"}`,
		`{"title":"Inspect","objective":"Read","tool_names":["file_read"],"morde_plan_revision":2}`,
		`{"title":"Inspect","objective":"Read","tool_names":["file_read"],"morde_plan_step_id":"bad.step","morde_plan_revision":2}`,
		`{"title":"Inspect","objective":"Read","tool_names":["file_read"],"morde_plan_step_id":"inspect","morde_plan_revision":0}`,
		`{"title":"Inspect","objective":"Read","tool_names":["file_read"],"morde_plan_step_id":"inspect","morde_plan_step_id":"other","morde_plan_revision":2}`,
	} {
		if _, _, _, _, _, err := validateCloudDSHDelegateArgumentsWithPlanSelection([]byte(raw), base); err == nil {
			t.Fatalf("invalid plan selector was accepted: %s", raw)
		}
	}

	arguments, _, stepID, planRevision, business, err := validateCloudDSHDelegateArgumentsWithPlanSelection(
		[]byte(`{"title":"Inspect","objective":"Read","tool_names":["file_read"]}`), base)
	if err != nil || arguments.Title != "Inspect" || stepID != "" || planRevision != 0 || len(business) == 0 {
		t.Fatalf("legacy no-plan delegation changed: step=%q revision=%d business=%s err=%v", stepID, planRevision, business, err)
	}
}
