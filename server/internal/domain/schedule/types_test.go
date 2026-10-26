package schedule

import (
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain/task"
)

func TestTimingValidationRequiresIANAZoneAndExplicitOnceOffset(t *testing.T) {
	base := Timing{Kind: TimingOnce, RunAt: "2026-11-01T01:30:00-04:00", TimeZone: "America/New_York"}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []Timing{
		{Kind: TimingOnce, RunAt: "2026-11-01T01:30:00", TimeZone: "America/New_York"},
		{Kind: TimingOnce, RunAt: "2026-11-01T01:30:00Z", TimeZone: "Mars/Olympus"},
		{Kind: TimingDaily, StartsOn: "2026-02-30", LocalTime: "02:30", TimeZone: "UTC"},
		{Kind: TimingWeekly, StartsOn: "2026-11-01", LocalTime: "01:30", TimeZone: "UTC", Weekdays: []int{1, 1}},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("Validate(%#v) unexpectedly succeeded", invalid)
		}
	}
}

func TestFirstOccurrenceUsesEarlierInstantForDSTFold(t *testing.T) {
	value, err := FirstOccurrence(Timing{
		Kind: TimingDaily, StartsOn: "2026-11-01", LocalTime: "01:30", TimeZone: "America/New_York",
	}, mustTime(t, "2026-11-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if value.ScheduledAt == nil || !value.ScheduledAt.Equal(mustTime(t, "2026-11-01T05:30:00Z")) {
		t.Fatalf("fold occurrence = %#v, want earlier 05:30Z instant", value)
	}
}

func TestFirstOccurrenceRecordsDSTGapAtTransition(t *testing.T) {
	value, err := FirstOccurrence(Timing{
		Kind: TimingDaily, StartsOn: "2026-03-08", LocalTime: "02:30", TimeZone: "America/New_York",
	}, mustTime(t, "2026-03-08T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if value.ScheduledAt != nil || value.SkipReason == nil || *value.SkipReason != SkipDSTGap ||
		!value.CheckAt.Equal(mustTime(t, "2026-03-08T07:00:00Z")) {
		t.Fatalf("gap occurrence = %#v, want dst_gap at 07:00Z", value)
	}
}

func TestWeeklyFirstOccurrenceUsesISOWeekdaysAndLatestDueCoalesces(t *testing.T) {
	timing := Timing{Kind: TimingWeekly, StartsOn: "2026-10-19", LocalTime: "09:00", TimeZone: "UTC", Weekdays: []int{1, 3}}
	first, err := FirstOccurrence(timing, mustTime(t, "2026-10-19T08:00:00Z"))
	if err != nil || first.Key != "2026-10-19T09:00" {
		t.Fatalf("first occurrence = %#v, %v", first, err)
	}
	latest, coalesced, err := LatestDue(timing, first, mustTime(t, "2026-10-28T10:00:00Z"))
	if err != nil || latest.Key != "2026-10-28T09:00" || coalesced != 3 {
		t.Fatalf("latest/coalesced = %#v/%d, %v", latest, coalesced, err)
	}
}

func TestOnceOccurrencePreservesOffsetAsInstant(t *testing.T) {
	value, err := FirstOccurrence(Timing{Kind: TimingOnce, RunAt: "2026-11-01T01:30:00-04:00", TimeZone: "America/New_York"}, time.Now())
	if err != nil || value.Key != "once:2026-11-01T05:30:00Z" || value.ScheduledAt == nil || value.ScheduledAt.Location() != time.UTC {
		t.Fatalf("once occurrence = %#v, %v", value, err)
	}
}

func TestMisfireGraceTreatsOneMinuteAsOnTimeAndOlderAsMissed(t *testing.T) {
	scheduled := mustTime(t, "2026-10-22T03:00:00Z")
	if IsMisfire(scheduled, scheduled.Add(MisfireGrace)) {
		t.Fatal("an occurrence observed exactly at the one-minute grace boundary was treated as missed")
	}
	if !IsMisfire(scheduled, scheduled.Add(MisfireGrace+time.Nanosecond)) {
		t.Fatal("an occurrence observed beyond the one-minute grace boundary was not treated as missed")
	}
}

func TestScheduleIDsUseOpenAPIPrefixes(t *testing.T) {
	id, err := NewID()
	if err != nil || !ValidID(id) || len(id) != 8 {
		t.Fatalf("NewID() = %q, %v", id, err)
	}
	runID, err := NewRunID()
	if err != nil || !ValidRunID(runID) || len(runID) != 8 {
		t.Fatalf("NewRunID() = %q, %v", runID, err)
	}
	for _, invalid := range []string{"", "y1234567", "j123456", "j123456!"} {
		if ValidID(invalid) {
			t.Errorf("invalid schedule ID %q was accepted", invalid)
		}
	}
	for _, invalid := range []string{"", "j1234567", "y123456", "y123456!"} {
		if ValidRunID(invalid) {
			t.Errorf("invalid run ID %q was accepted", invalid)
		}
	}
}

func TestScheduleEnumsAndValidation(t *testing.T) {
	for _, value := range []Status{StatusEnabled, StatusDisabled, StatusCompleted} {
		if !value.Valid() {
			t.Errorf("Status(%q) is invalid", value)
		}
	}
	for _, value := range []MisfirePolicy{MisfireSkip, MisfireRunOnce} {
		if !value.Valid() {
			t.Errorf("MisfirePolicy(%q) is invalid", value)
		}
	}
	for _, value := range []NotificationEvent{NotifyCompleted, NotifyFailed, NotifyWaitingUser} {
		if !value.Valid() {
			t.Errorf("NotificationEvent(%q) is invalid", value)
		}
	}
	active := []RunStatus{RunQueued, RunRunning, RunWaitingUser, RunPausing, RunPaused, RunStopping}
	for _, value := range active {
		if !value.Valid() || !value.Active() {
			t.Errorf("active RunStatus(%q) is invalid or inactive", value)
		}
	}
	for _, value := range []RunStatus{RunCompleted, RunFailed, RunCancelled, RunSkipped, RunMissed} {
		if !value.Valid() || value.Active() {
			t.Errorf("terminal RunStatus(%q) is invalid or active", value)
		}
	}
	if RunStatus("unknown").Valid() || Status("unknown").Valid() || MisfirePolicy("unknown").Valid() || NotificationEvent("unknown").Valid() {
		t.Fatal("unknown schedule enum value was accepted")
	}

	next := mustTime(t, "2026-11-01T05:30:00Z")
	base := Schedule{
		ID: "j1234567", OwnerID: "u1234567", Version: 1, Status: StatusEnabled,
		HostID: "h1234567", ModelID: "model", Objective: "check for updates",
		Timing:        Timing{Kind: TimingOnce, RunAt: "2026-11-01T01:30:00-04:00", TimeZone: "America/New_York"},
		Budget:        task.Budget{ActiveSeconds: 60, ToolCalls: 1, ModelTokens: 100},
		Authorization: task.Authorization{Kind: "session"}, MisfirePolicy: MisfireSkip,
		NotificationEvents: []NotificationEvent{NotifyCompleted}, NextOccurrenceKey: "once:2026-11-01T05:30:00Z",
		NextCheckAt: &next, ExecutionEpoch: 1,
	}
	if err := Validate(base); err != nil {
		t.Fatalf("valid enabled schedule rejected: %v", err)
	}
	for _, mutate := range []func(*Schedule){
		func(value *Schedule) { value.ID = "invalid" },
		func(value *Schedule) { value.OwnerID = "" },
		func(value *Schedule) { value.Version = 0 },
		func(value *Schedule) { value.Status = "unknown" },
		func(value *Schedule) { value.HostID = "" },
		func(value *Schedule) { value.ModelID = "" },
		func(value *Schedule) { value.Objective = "  " },
		func(value *Schedule) { value.Authorization.Kind = "" },
		func(value *Schedule) { value.MisfirePolicy = "unknown" },
		func(value *Schedule) { value.ExecutionEpoch = 0 },
		func(value *Schedule) { value.NextOccurrenceKey = "" },
		func(value *Schedule) { value.NextCheckAt = nil },
		func(value *Schedule) { value.NotificationEvents = []NotificationEvent{"unknown"} },
		func(value *Schedule) { value.NotificationEvents = []NotificationEvent{NotifyFailed, NotifyFailed} },
	} {
		invalid := base
		invalid.NotificationEvents = append([]NotificationEvent(nil), base.NotificationEvents...)
		mutate(&invalid)
		if err := Validate(invalid); err == nil {
			t.Errorf("Validate(%#v) accepted invalid schedule", invalid)
		}
	}
}

func TestNextOccurrenceValidatesKeyAndAdvancesRecurringSchedules(t *testing.T) {
	timing := Timing{Kind: TimingDaily, StartsOn: "2026-10-22", LocalTime: "09:00", TimeZone: "UTC"}
	next, err := NextOccurrence(timing, "2026-10-22T09:00")
	if err != nil || next.Key != "2026-10-23T09:00" || next.ScheduledAt == nil {
		t.Fatalf("NextOccurrence() = %#v, %v", next, err)
	}
	if _, err := NextOccurrence(timing, "not-a-date"); err == nil {
		t.Fatal("invalid occurrence key was accepted")
	}
	once, err := NextOccurrence(Timing{Kind: TimingOnce, RunAt: "2026-11-01T01:30:00-04:00", TimeZone: "America/New_York"}, "ignored")
	if err != nil || once != (Occurrence{}) {
		t.Fatalf("one-time NextOccurrence() = %#v, %v", once, err)
	}
}

func TestObservationSpecValidationAndConfigFingerprint(t *testing.T) {
	value := ObservationSpec{URL: "https://example.test/status", Selector: "#status", Field: ObservationText}
	if err := value.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (ObservationSpec{URL: value.URL, Selector: strings.Repeat("#", 1024), Field: value.Field}).Validate(); err != nil {
		t.Fatalf("selector at runtime limit rejected: %v", err)
	}
	hash := value.ConfigHash()
	if len(hash) != 64 || hash != (ObservationSpec{URL: "https://example.test/status", Selector: "#status", Field: ObservationText}).ConfigHash() {
		t.Fatalf("unstable observation config hash: %q", hash)
	}
	for _, invalid := range []ObservationSpec{
		{URL: "javascript:alert(1)", Selector: "#status", Field: ObservationText},
		{URL: "https://user:pass@example.test/status", Selector: "#status", Field: ObservationText},
		{URL: "https://example.test/status#fragment", Selector: "#status", Field: ObservationText},
		{URL: "https://example.test/status", Selector: " ", Field: ObservationText},
		{URL: "https://example.test/status", Selector: "#status", Field: "attribute"},
		{URL: "https://example.test/status", Selector: strings.Repeat("#", 1025), Field: ObservationText},
	} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("invalid observation accepted: %#v", invalid)
		}
	}
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
