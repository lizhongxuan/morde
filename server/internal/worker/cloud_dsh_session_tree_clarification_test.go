package worker

import (
	"encoding/json"
	"testing"

	"morde/server/internal/app"
)

func TestCloudDSHCheckpointAcceptsOnlyLockedCurrentOrLegacyAskUserDefinition(t *testing.T) {
	current := app.CloudDSHAskUserToolDefinition()
	legacy := app.CloudDSHLegacyAskUserToolDefinition()
	for _, scenario := range []struct {
		name string
		tool app.AgentToolDefinition
	}{
		{name: "current structured definition", tool: current},
		{name: "pinned legacy definition", tool: legacy},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			encoded, err := json.Marshal(cloudDSHToolPayload([]app.AgentToolDefinition{scenario.tool}))
			if err != nil {
				t.Fatal(err)
			}
			var payloads []json.RawMessage
			if err := json.Unmarshal(encoded, &payloads); err != nil {
				t.Fatal(err)
			}
			selected, err := cloudDSHDefinitionsFromCheckpointToolPayload([]app.AgentToolDefinition{current}, payloads, false, false)
			if err != nil || len(selected) != 1 || selected[0].Name != "ask_user" {
				t.Fatalf("pinned ask_user definition = %#v, err=%v", selected, err)
			}
			want, err := json.Marshal(cloudDSHToolPayload([]app.AgentToolDefinition{scenario.tool}))
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.Marshal(cloudDSHToolPayload(selected))
			if err != nil || string(got) != string(want) {
				t.Fatalf("selected definition differs from pinned schema: got=%s want=%s err=%v", got, want, err)
			}
		})
	}

	spoofed := legacy
	spoofed.Description = "Extension replacement that bypasses Morde clarification persistence."
	encoded, err := json.Marshal(cloudDSHToolPayload([]app.AgentToolDefinition{spoofed}))
	if err != nil {
		t.Fatal(err)
	}
	var payloads []json.RawMessage
	if err := json.Unmarshal(encoded, &payloads); err != nil {
		t.Fatal(err)
	}
	if _, err := cloudDSHDefinitionsFromCheckpointToolPayload([]app.AgentToolDefinition{current}, payloads, false, false); err == nil {
		t.Fatal("checkpoint accepted an extension-provided ask_user definition")
	}
}
