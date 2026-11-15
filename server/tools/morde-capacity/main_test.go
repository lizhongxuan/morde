package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidateTargetRequiresExactHTTPSHost(t *testing.T) {
	remote, err := url.Parse("https://load.example.test")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		allow   string
		wantErr bool
	}{
		{name: "missing allow host", wantErr: true},
		{name: "wrong allow host", allow: "other.example.test", wantErr: true},
		{name: "matching allow host", allow: "load.example.test"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateTarget(remote, test.allow); (err != nil) != test.wantErr {
				t.Fatalf("validateTarget() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func TestCheckTargetCapabilitiesRequiresCloudTasks(t *testing.T) {
	for _, test := range []struct {
		name    string
		feature string
		wantErr bool
	}{
		{name: "cloud tasks available", feature: "cloud_tasks"},
		{name: "cloud tasks unavailable", feature: "hub", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/capabilities" {
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"supported_features": []string{test.feature}})
			}))
			defer server.Close()
			if err := checkTargetCapabilities(server.URL); (err != nil) != test.wantErr {
				t.Fatalf("checkTargetCapabilities() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func TestCheckScenarioEndpointsSelectsAvailableTaskScope(t *testing.T) {
	var conversationTasksStatus, accountTasksStatus atomic.Int32
	conversationTasksStatus.Store(http.StatusOK)
	accountTasksStatus.Store(http.StatusOK)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer chc_fixture" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/conversations/c0000001/messages" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/api/conversations/c0000001/tasks" {
			w.WriteHeader(int(conversationTasksStatus.Load()))
			return
		}
		if r.URL.Path == "/api/tasks" {
			w.WriteHeader(int(accountTasksStatus.Load()))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	user := virtualUser{AccessToken: "chc_fixture", ConversationID: "c0000001"}
	scope, err := checkScenarioEndpoints(server.URL, user)
	if err != nil || scope != "conversation" {
		t.Fatalf("valid conversation endpoints rejected: %v", err)
	}
	conversationTasksStatus.Store(http.StatusNotFound)
	scope, err = checkScenarioEndpoints(server.URL, user)
	if err != nil || scope != "account" {
		t.Fatalf("account task-list fallback rejected: scope=%q error=%v", scope, err)
	}
	accountTasksStatus.Store(http.StatusNotFound)
	if _, err := checkScenarioEndpoints(server.URL, user); err == nil || !strings.Contains(err.Error(), "account-tasks endpoint returned HTTP 404") {
		t.Fatalf("unavailable task endpoints error = %v", err)
	}
}

func TestReadScenarioRequiresPrivateManifest(t *testing.T) {
	path := t.TempDir() + "/scenario.json"
	if err := os.WriteFile(path, []byte(`{"users":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readScenario(path); err != nil {
		t.Fatalf("private manifest rejected: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readScenario(path); err == nil {
		t.Fatal("manifest readable by group/other users was accepted")
	}
}

func TestExpandScenarioReusesOneProfileForReadClients(t *testing.T) {
	cfg := options{Connections: 10}
	profiles := []virtualUser{{AccessToken: "chc_test", ActorID: "u0000001", ConversationID: "c0000001"}}
	users, err := expandScenario(cfg, profiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != cfg.Connections || users[0] != users[len(users)-1] {
		t.Fatalf("one read-only profile did not expand to virtual clients: len=%d", len(users))
	}
}

func TestValidateScenarioRequiresDistinctTaskFixturesForParallelStreams(t *testing.T) {
	cfg := options{Connections: 2, TaskStreams: 2}
	users := []virtualUser{
		{AccessToken: "chc_test_one", ActorID: "u0000001", ConversationID: "c0000001", TaskID: "q0000001"},
		{AccessToken: "chc_test_two", ActorID: "u0000002", ConversationID: "c0000002", TaskID: "q0000001"},
	}
	if err := validateScenario(cfg, users); err == nil {
		t.Fatal("parallel streams sharing a Task ID were accepted")
	}
	users[1].TaskID = "q0000002"
	if err := validateScenario(cfg, users); err != nil {
		t.Fatalf("distinct Task fixtures rejected: %v", err)
	}
}

func TestRecordRuntimeSampleTracksPeaks(t *testing.T) {
	stats := newCounters()
	stats.recordRuntimeSample(12, 1024, 2048)
	stats.recordRuntimeSample(8, 512, 4096)
	if stats.peakRuntime != (runtimeReport{Goroutines: 12, HeapBytes: 1024, SysBytes: 4096}) {
		t.Fatalf("runtime peaks = %#v", stats.peakRuntime)
	}
}

func TestCanceledConversationPollsAreNotReportedAsTransportErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	base, err := url.Parse("http://127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	stats := newCounters()
	user := virtualUser{AccessToken: "chc_fixture", ConversationID: "c0000001"}
	pollConversation(ctx, base, &http.Client{}, user, stats)
	pollTasks(ctx, base, &http.Client{}, options{TaskPollScope: "account"}, user, stats)
	if len(stats.conversationStatuses) != 0 || len(stats.taskPollStatuses) != 0 || len(stats.errors) != 0 {
		t.Fatalf("intentional shutdown cancellation counted as failure: conversation=%#v tasks=%#v errors=%#v", stats.conversationStatuses, stats.taskPollStatuses, stats.errors)
	}
}

func TestRunLoadConversationPollsTaskStreamsAndOptInWrites(t *testing.T) {
	var messages atomic.Int64
	var approvals atomic.Int64
	var eventStreams atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/api/capabilities", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"supported_features": []string{"cloud_tasks"}})
	})
	mux.HandleFunc("/api/conversations/c0000001/messages", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer chc_") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			if r.Header.Get("X-Morde-Actor-ID") == "" || r.Header.Get("X-Morde-Service-Instance-ID") == "" || r.Header.Get("Idempotency-Key") == "" {
				http.Error(w, "invalid message request", http.StatusBadRequest)
				return
			}
			messages.Add(1)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}, "next_cursor": nil})
	})
	mux.HandleFunc("/api/conversations/c0000001/tasks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}, "next_cursor": nil, "pending_count": 0})
	})
	mux.HandleFunc("/api/tasks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}, "next_cursor": nil})
	})
	mux.HandleFunc("/api/tasks/q0000001/events", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer chc_") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if lastID := r.Header.Get("Last-Event-ID"); lastID != "" && lastID != "1" {
			http.Error(w, "invalid event cursor", http.StatusBadRequest)
			return
		}
		eventStreams.Add(1)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "id: 1\nevent: task.status\ndata: {}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("/api/tasks/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Idempotency-Key") == "" {
			http.Error(w, "invalid approval request", http.StatusBadRequest)
			return
		}
		approvals.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	users := []virtualUser{
		{AccessToken: "chc_test_secret_one", ActorID: "u0000001", ServiceInstanceID: "instance-001", ConversationID: "c0000001", TaskID: "q0000001", LongTask: true},
		{AccessToken: "chc_test_secret_two", ActorID: "u0000002", ConversationID: "c0000001", Approval: &approvalFixture{TaskID: "q0000002", ApprovalID: "approval-02", Version: 1}},
	}
	cfg := options{
		BaseURL: server.URL, Environment: "local-mock", SuccessCriteria: "all protocol assertions pass",
		TaskPollScope: "account", ManifestPath: "in-memory", Connections: 2, TaskStreams: 1, LongTaskStreams: 1,
		Duration: 240 * time.Millisecond, PollInterval: 30 * time.Millisecond, Reconnect: 50 * time.Millisecond,
		MessageCount: 1, BurstSize: 2, ApprovalCount: 1, ApprovalDecision: "reject", MessageText: "short capacity probe",
		WriteRate: 100, EnableWrites: true,
	}
	if err := validateOptions(cfg); err != nil {
		t.Fatal(err)
	}
	if err := validateScenario(cfg, users); err != nil {
		t.Fatal(err)
	}
	result, err := runLoad(context.Background(), cfg, users)
	if err != nil {
		t.Fatal(err)
	}
	if result.Started != 2 || result.Conversation.Count == 0 || result.Tasks.Count == 0 {
		t.Fatalf("conversation polling not exercised: %#v", result)
	}
	if result.TaskPollScope != "account" || result.Tasks.Statuses["200"] == 0 {
		t.Fatalf("account task poll scope not reported: %#v", result)
	}
	if result.TaskStreams.Established < 1 || result.TaskStreams.Reconnects == 0 || result.TaskStreams.Events == 0 || eventStreams.Load() < 2 {
		t.Fatalf("Task event lifecycle not exercised: %#v; server streams=%d", result.TaskStreams, eventStreams.Load())
	}
	if result.TaskStreams.LongTaskRatio != 1 || result.TaskStreams.LongRunning != 1 {
		t.Fatalf("long-task ratio not reported: %#v", result.TaskStreams)
	}
	if result.ClientRuntime.Goroutines < 1 || result.ClientRuntime.HeapBytes == 0 || result.ClientRuntime.SysBytes == 0 {
		t.Fatalf("client runtime peaks not reported: %#v", result.ClientRuntime)
	}
	if messages.Load() != 2 || approvals.Load() != 1 || result.Messages.Statuses["202"] != 2 || result.Approvals.Statuses["202"] != 1 {
		t.Fatalf("write scenarios were not accepted: messages=%d approvals=%d report=%#v", messages.Load(), approvals.Load(), result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "test_secret") {
		t.Fatalf("report leaked a credential: %s", encoded)
	}
}
