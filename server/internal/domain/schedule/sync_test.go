package schedule

import (
	"encoding/json"
	"strings"
	"testing"

	"morde/server/internal/domain/task"
)

func TestSyncDefinitionOmitsAuthorizationAndRuntimeState(t *testing.T) {
	value := Schedule{
		ID: "j1234567", OwnerID: "u1234567", Version: 1, Status: StatusDisabled,
		HostID: "h1234567", ModelID: "model-a", Title: "Report title", Objective: "check report",
		Timing:        Timing{Kind: TimingEveryNHours, IntervalHours: 6, AnchorAt: "2030-03-10T06:00:30Z", TimeZone: "Asia/Shanghai"},
		Budget:        task.Budget{ActiveSeconds: 60, ToolCalls: 2, ModelTokens: 100},
		Authorization: task.Authorization{Kind: "client", TokenID: "t1234567", Scopes: []string{"schedule:manage"}},
		MisfirePolicy: MisfireSkip, NotificationEvents: []NotificationEvent{NotifyCompleted},
		NextOccurrenceKey: "internal-key", ExecutionEpoch: 9,
	}
	encoded, err := json.Marshal(NewSyncDefinition(value))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"title":"Report title"`) || !strings.Contains(string(encoded), `"objective":"check report"`) ||
		!strings.Contains(string(encoded), `"anchor_at":"2030-03-10T06:00:30Z"`) {
		t.Fatalf("sync definition did not keep title separate from objective: %s", encoded)
	}
	for _, forbidden := range []string{"authorization", "token_id", "schedule:manage", "internal-key", "execution_epoch"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("sync definition exposed %q: %s", forbidden, encoded)
		}
	}
}

func TestSyncDefinitionAllowsOnlyHostlessModelSchedules(t *testing.T) {
	value := Schedule{
		ID: "j1234567", OwnerID: "u1234567", Version: 1, Status: StatusDisabled,
		HostID: "h1234567", ModelID: "model-a", Title: "Report title", Objective: "check report",
		Timing:        Timing{Kind: TimingDaily, StartsOn: "2026-11-02", LocalTime: "09:00", TimeZone: "Asia/Shanghai"},
		Budget:        task.Budget{ActiveSeconds: 60, ToolCalls: 2, ModelTokens: 100},
		MisfirePolicy: MisfireSkip, NotificationEvents: []NotificationEvent{NotifyCompleted},
	}
	definition := NewSyncDefinition(value)
	definition.HostID = nil
	if err := ValidateSyncDefinition(value.ID, definition); err != nil {
		t.Fatalf("valid hostless sync definition rejected: %v", err)
	}
	definition.Observation = &ObservationSpec{URL: "https://example.test/status", Selector: "#status", Field: ObservationText}
	if err := ValidateSyncDefinition(value.ID, definition); err == nil {
		t.Fatal("hostless sync definition accepted computer observation")
	}
	definition.Observation = nil
	definition.Title = "  "
	if err := ValidateSyncDefinition(value.ID, definition); err == nil {
		t.Fatal("sync definition accepted an empty title")
	}
	definition.Title = "Report title"
	badHostID := "bad-host"
	definition.HostID = &badHostID
	if err := ValidateSyncDefinition(value.ID, definition); err == nil {
		t.Fatal("sync definition accepted malformed Host ID")
	}
}
