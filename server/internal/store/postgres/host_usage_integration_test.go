package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/project"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestPostgresHostUsageCalculatesBoundedIntervalsAndPreservesSamples(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 10_000, 20_000)
	value := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 10_000, MemMiB: 20_000}, nil)
	if err := store.MarkCreateAttempt(ctx, value.ID); err != nil {
		t.Fatal(err)
	}
	sandboxID := "sandbox-usage-a"
	if err := store.SetSandboxID(ctx, value.ID, host.StateCreating, &sandboxID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRuntimeInstanceID(ctx, value.ID, value.RuntimeGeneration, value.ControlVersion, "", "runtime-usage-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionHost(ctx, value.ID, host.StateCreating, host.StateRunning, ""); err != nil {
		t.Fatal(err)
	}

	start := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	first := usageObservation(value.ID, 1, sandboxID, start, start, usageContainerA, 10_000_000_000, 100<<20)
	if interval, err := store.RecordHostUsageObservation(ctx, first, 5*time.Second); err != nil || interval.State != host.UsageIntervalUnknown || interval.UnknownReason != host.UsageNoPreviousSample {
		t.Fatalf("first interval = %#v, %v", interval, err)
	}
	second := usageObservation(value.ID, 1, sandboxID, start.Add(time.Second), start.Add(time.Second), usageContainerA, 11_250_000_000, 300<<20)
	interval, err := store.RecordHostUsageObservation(ctx, second, 5*time.Second)
	if err != nil || interval.State != host.UsageIntervalMeasured || interval.UnknownReason != "" ||
		interval.CalculationMethod != host.UsageCalculationMethod || interval.CPUSeconds != 1.25 || interval.EstimatedMemoryMiBSeconds != 200 {
		t.Fatalf("measured interval = %#v, %v", interval, err)
	}
	replaced := usageObservation(value.ID, 1, sandboxID, start.Add(2*time.Second), start.Add(2*time.Second), usageContainerB, 12_000_000_000, 400<<20)
	interval, err = store.RecordHostUsageObservation(ctx, replaced, 5*time.Second)
	if err != nil || interval.State != host.UsageIntervalUnknown || interval.UnknownReason != host.UsageContainerReplaced {
		t.Fatalf("replacement interval = %#v, %v", interval, err)
	}
	reset := usageObservation(value.ID, 1, sandboxID, start.Add(3*time.Second), start.Add(3*time.Second), usageContainerB, 1_000_000_000, 250<<20)
	interval, err = store.RecordHostUsageObservation(ctx, reset, 5*time.Second)
	if err != nil || interval.State != host.UsageIntervalUnknown || interval.UnknownReason != host.UsageCounterReset {
		t.Fatalf("counter reset interval = %#v, %v", interval, err)
	}
	gap := usageObservation(value.ID, 1, sandboxID, start.Add(10*time.Second), start.Add(10*time.Second), usageContainerB, 8_000_000_000, 600<<20)
	interval, err = store.RecordHostUsageObservation(ctx, gap, 5*time.Second)
	if err != nil || interval.State != host.UsageIntervalUnknown || interval.UnknownReason != host.UsageSamplingGap {
		t.Fatalf("gap interval = %#v, %v", interval, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_generation=2,sandbox_id='sandbox-usage-b' WHERE id=$1`, value.ID); err != nil {
		t.Fatal(err)
	}
	changed := usageObservation(value.ID, 2, "sandbox-usage-b", start.Add(11*time.Second), start.Add(11*time.Second), usageContainerC, 15_000_000_000, 200<<20)
	interval, err = store.RecordHostUsageObservation(ctx, changed, 5*time.Second)
	if err != nil || interval.State != host.UsageIntervalUnknown || interval.UnknownReason != host.UsageGenerationChange {
		t.Fatalf("generation interval = %#v, %v", interval, err)
	}

	history, err := store.GetHostUsageHistory(ctx, value.ID, 1)
	if err != nil || history.Samples != 5 || len(history.Intervals) != 5 || history.Intervals[1].State != host.UsageIntervalMeasured ||
		history.Intervals[1].CPUSeconds != 1.25 || history.Intervals[1].EstimatedMemoryMiBSeconds != 200 ||
		history.Intervals[2].UnknownReason != host.UsageContainerReplaced || history.Intervals[3].UnknownReason != host.UsageCounterReset ||
		history.Intervals[4].UnknownReason != host.UsageSamplingGap || len(history.Observations) != 5 ||
		string(history.Observations[0].Metrics.RawPayload) != string(first.Metrics.RawPayload) ||
		history.Observations[0].Attribution.OwnerID != owner.ID ||
		history.Observations[0].Attribution.State != host.UsageAttributionUnknown ||
		history.Observations[0].Attribution.Reason != host.UsageAttributionNoActiveBinding {
		t.Fatalf("generation 1 usage history = %#v, %v", history, err)
	}
	var raw []byte
	if err := store.pool.QueryRow(ctx, `SELECT raw_payload FROM host_usage_samples WHERE host_id=$1 AND runtime_generation=1 ORDER BY id LIMIT 1`, value.ID).Scan(&raw); err != nil || string(raw) != string(first.Metrics.RawPayload) {
		t.Fatalf("raw sample was not persisted exactly: %q, %v", raw, err)
	}
	var actorID pgtype.Text
	if err := store.pool.QueryRow(ctx, `SELECT actor_id FROM host_usage_samples WHERE host_id=$1 ORDER BY id LIMIT 1`, value.ID).Scan(&actorID); err != nil || actorID.Valid {
		t.Fatalf("Host sample actor = %v, %v; want null", actorID, err)
	}
	encoded, err := json.Marshal(history.Observations[0])
	if err != nil || string(encoded) == "" || containsUsageContainerID(string(encoded), usageContainerA) {
		t.Fatalf("private container ID leaked in observation JSON %s, err=%v", encoded, err)
	}
	newGeneration, err := store.GetHostUsageHistory(ctx, value.ID, 2)
	if err != nil || newGeneration.Samples != 1 || len(newGeneration.Intervals) != 1 || newGeneration.Intervals[0].UnknownReason != host.UsageGenerationChange {
		t.Fatalf("generation 2 usage history = %#v, %v", newGeneration, err)
	}
	provider, closeProvider, err := newMigrationProvider(store.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 117); err == nil || !strings.Contains(err.Error(), "cannot roll back Host usage metering") {
		t.Fatalf("migration downgrade with usage history = %v, want data protection rejection", err)
	}
}

func TestPostgresHostUsageSnapshotsUniqueProjectWorkspaceAndKeepsActorUnknown(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 10_000, 20_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "metered workspace")
	value := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 10_000, MemMiB: 20_000}, []string{workspaceValue.ID})
	if err := store.MarkCreateAttempt(ctx, value.ID); err != nil {
		t.Fatal(err)
	}
	sandboxID := "sandbox-attribution"
	if err := store.SetSandboxID(ctx, value.ID, host.StateCreating, &sandboxID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRuntimeInstanceID(ctx, value.ID, value.RuntimeGeneration, value.ControlVersion, "", "runtime-usage-attribution"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionHost(ctx, value.ID, host.StateCreating, host.StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_bindings SET state='active' WHERE host_id=$1 AND workspace_id=$2`, value.ID, workspaceValue.ID); err != nil {
		t.Fatal(err)
	}
	projectA := createProjectStoreFixture(t, ctx, store, owner.ID, "Usage project A", "Usage project attribution", "usage-project-create-a")
	if _, _, err := store.AddProjectResource(ctx, owner.ID, project.Resource{
		ProjectID: projectA.ID, Kind: project.ResourceWorkspace, ResourceID: workspaceValue.ID, Version: 1,
	}, projectA.Version, "usage-project-resource-a"); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 3, 4, 0, 0, 0, time.UTC)
	first := usageObservation(value.ID, 1, sandboxID, start, start, usageContainerA, 1_000_000_000, 128<<20)
	if _, err := store.RecordHostUsageObservation(ctx, first, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	history, err := store.GetHostUsageHistory(ctx, value.ID, 1)
	if err != nil || len(history.Observations) != 1 {
		t.Fatalf("single-project usage history = %#v, %v", history, err)
	}
	attr := history.Observations[0].Attribution
	if attr.State != host.UsageAttributionProject || attr.OwnerID != owner.ID || attr.WorkspaceID != workspaceValue.ID ||
		attr.WorkspaceBindingID == "" || attr.WorkspaceBindingVersion < 1 || attr.ProjectID != projectA.ID ||
		attr.ProjectResourceVersion != 1 || attr.ActorID != nil {
		t.Fatalf("unique project attribution = %#v", attr)
	}
	stable := usageObservation(value.ID, 1, sandboxID, start.Add(time.Second), start.Add(time.Second), usageContainerA, 2_000_000_000, 128<<20)
	if _, err := store.RecordHostUsageObservation(ctx, stable, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	history, err = store.GetHostUsageHistory(ctx, value.ID, 1)
	if err != nil || len(history.Intervals) != 2 || history.Intervals[1].State != host.UsageIntervalMeasured ||
		history.Intervals[1].Attribution.State != host.UsageAttributionProject || history.Intervals[1].Attribution.ProjectID != projectA.ID {
		t.Fatalf("stable project interval attribution = %#v, %v", history.Intervals, err)
	}
	viewer := createTestUser(t, store, 4, 10_000, 20_000)
	currentProject, err := store.GetProject(ctx, owner.ID, projectA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, viewer.ID, currentProject, project.RoleViewer,
		currentProject.Version, "usage-viewer-invite-01", "usage-viewer-accept-01"); err != nil {
		t.Fatal(err)
	}
	projectHistory, err := store.GetProjectHostUsageHistory(ctx, viewer.ID, projectA.ID, value.ID)
	if err != nil || projectHistory.Samples != 2 || len(projectHistory.Intervals) != 2 ||
		projectHistory.Intervals[0].State != host.UsageIntervalUnknown ||
		projectHistory.Intervals[1].State != host.UsageIntervalMeasured ||
		projectHistory.Intervals[1].CPUSeconds != 1 || projectHistory.Intervals[1].EstimatedMemoryMiBSeconds != 128 ||
		len(projectHistory.Observations) != 0 || projectHistory.Intervals[1].HostID != "" {
		t.Fatalf("project-scoped usage history = %#v, %v", projectHistory, err)
	}

	projectB := createProjectStoreFixture(t, ctx, store, owner.ID, "Usage project B", "Ambiguous usage attribution", "usage-project-create-b")
	if _, _, err := store.AddProjectResource(ctx, owner.ID, project.Resource{
		ProjectID: projectB.ID, Kind: project.ResourceWorkspace, ResourceID: workspaceValue.ID, Version: 1,
	}, projectB.Version, "usage-project-resource-b"); err != nil {
		t.Fatal(err)
	}
	third := usageObservation(value.ID, 1, sandboxID, start.Add(2*time.Second), start.Add(2*time.Second), usageContainerA, 3_000_000_000, 128<<20)
	if _, err := store.RecordHostUsageObservation(ctx, third, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	history, err = store.GetHostUsageHistory(ctx, value.ID, 1)
	if err != nil || len(history.Observations) != 3 || history.Observations[2].Attribution.State != host.UsageAttributionUnknown ||
		history.Observations[2].Attribution.Reason != host.UsageAttributionMultipleProjects || history.Observations[2].Attribution.ProjectID != "" {
		t.Fatalf("ambiguous project attribution = %#v, %v", history, err)
	}
	if len(history.Intervals) != 3 || history.Intervals[2].State != host.UsageIntervalMeasured ||
		history.Intervals[2].Attribution.State != host.UsageAttributionUnknown ||
		history.Intervals[2].Attribution.Reason != host.UsageAttributionMultipleProjects {
		t.Fatalf("interval with ambiguous project attribution = %#v", history.Intervals)
	}
	projectHistory, err = store.GetProjectHostUsageHistory(ctx, viewer.ID, projectA.ID, value.ID)
	if err != nil || projectHistory.Samples != 2 || len(projectHistory.Intervals) != 2 {
		t.Fatalf("project A usage after ambiguous association = %#v, %v", projectHistory, err)
	}
	projectBHistory, err := store.GetProjectHostUsageHistory(ctx, owner.ID, projectB.ID, value.ID)
	if err != nil || projectBHistory.Samples != 0 || len(projectBHistory.Intervals) != 0 {
		t.Fatalf("project B usage = %#v, %v", projectBHistory, err)
	}
	currentProject, err = store.GetProject(ctx, owner.ID, projectA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveProjectMember(ctx, owner.ID, projectA.ID, viewer.ID, currentProject.Version, "usage-viewer-remove-01"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetProjectHostUsageHistory(ctx, viewer.ID, projectA.ID, value.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("revoked project viewer usage history = %v, want not found", err)
	}
}

const (
	usageContainerA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	usageContainerB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	usageContainerC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func usageObservation(hostID string, generation int64, sandboxID string, sampledAt, observedAt time.Time, containerID string, cpuTotalNS, memoryBytes int64) host.UsageObservation {
	return host.UsageObservation{
		HostID: hostID, RuntimeGeneration: generation, SandboxID: sandboxID, ObservedAt: observedAt,
		State: host.UsageObservationSampled,
		Metrics: &host.ResourceMetrics{SampledAt: sampledAt, ContainerID: containerID, CPUUsageTotalNS: cpuTotalNS,
			MemoryUsageBytes: memoryBytes,
			RawPayload:       []byte(fmt.Sprintf(`{"cpu_usage_total_ns":%d,"memory_usage_bytes":%d}`, cpuTotalNS, memoryBytes))},
	}
}

func containsUsageContainerID(value, containerID string) bool {
	return strings.Contains(value, containerID)
}
