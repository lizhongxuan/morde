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
		HostID: "h1234567", ModelID: "model-a", Objective: "check report",
		Timing:        Timing{Kind: TimingDaily, StartsOn: "2026-11-02", LocalTime: "09:00", TimeZone: "Asia/Shanghai"},
		Budget:        task.Budget{ActiveSeconds: 60, ToolCalls: 2, ModelTokens: 100},
		Authorization: task.Authorization{Kind: "client", TokenID: "t1234567", Scopes: []string{"schedule:manage"}},
		MisfirePolicy: MisfireSkip, NotificationEvents: []NotificationEvent{NotifyCompleted},
		NextOccurrenceKey: "internal-key", ExecutionEpoch: 9,
	}
	encoded, err := json.Marshal(NewSyncDefinition(value))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"authorization", "token_id", "schedule:manage", "internal-key", "execution_epoch"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("sync definition exposed %q: %s", forbidden, encoded)
		}
	}
}
