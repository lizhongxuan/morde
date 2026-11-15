package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const maxConnections = 10_000

type approvalFixture struct {
	TaskID     string `json:"task_id"`
	ApprovalID string `json:"approval_id"`
	Version    int64  `json:"version"`
}

type virtualUser struct {
	AccessToken       string           `json:"access_token"`
	ActorID           string           `json:"actor_id"`
	ServiceInstanceID string           `json:"service_instance_id"`
	ConversationID    string           `json:"conversation_id"`
	TaskID            string           `json:"task_id,omitempty"`
	LongTask          bool             `json:"long_task,omitempty"`
	Approval          *approvalFixture `json:"approval,omitempty"`
}

type scenario struct {
	Users []virtualUser `json:"users"`
}

type options struct {
	BaseURL          string
	Environment      string
	SuccessCriteria  string
	TaskPollScope    string
	ManifestPath     string
	Connections      int
	TaskStreams      int
	LongTaskStreams  int
	Duration         time.Duration
	Ramp             time.Duration
	PollInterval     time.Duration
	Reconnect        time.Duration
	MessageCount     int
	BurstSize        int
	ApprovalCount    int
	ApprovalDecision string
	MessageText      string
	WriteRate        float64
	EnableWrites     bool
	AllowHost        string
}

func main() {
	var cfg options
	var duration, ramp, pollInterval, reconnect time.Duration
	flag.StringVar(&cfg.BaseURL, "base-url", "", "Portal URL (for example http://127.0.0.1:8080)")
	flag.StringVar(&cfg.Environment, "environment", "", "test environment label to include in the report")
	flag.StringVar(&cfg.SuccessCriteria, "success-criteria", "", "pre-agreed success criteria to include in the report")
	flag.StringVar(&cfg.ManifestPath, "manifest", "", "path to a private JSON file containing virtual-user credentials and IDs")
	flag.IntVar(&cfg.Connections, "connections", 100, "number of concurrent virtual conversation clients (maximum 10000)")
	flag.IntVar(&cfg.TaskStreams, "task-streams", 0, "number of concurrent active Task event streams")
	flag.IntVar(&cfg.LongTaskStreams, "long-task-streams", 0, "number of task streams marked as long-running in the manifest")
	flag.DurationVar(&duration, "duration", 5*time.Minute, "load duration")
	flag.DurationVar(&ramp, "ramp", time.Minute, "time over which to start virtual clients")
	flag.DurationVar(&pollInterval, "poll-interval", 30*time.Second, "interval between conversation history and active Task polls")
	flag.DurationVar(&reconnect, "reconnect", time.Minute, "Task SSE reconnect interval; use 0 to disable intentional reconnects")
	flag.IntVar(&cfg.MessageCount, "messages", 0, "number of virtual users to submit short chat messages")
	flag.IntVar(&cfg.BurstSize, "burst-size", 1, "consecutive messages sent per selected message user")
	flag.IntVar(&cfg.ApprovalCount, "approvals", 0, "number of one-shot seeded approval decisions to submit")
	flag.StringVar(&cfg.ApprovalDecision, "approval-decision", "", "approve or reject; required when -approvals is nonzero")
	flag.StringVar(&cfg.MessageText, "message-text", "capacity test short chat message", "text sent by each selected message scenario")
	flag.Float64Var(&cfg.WriteRate, "write-rate", 5, "maximum write requests per second across messages and approvals")
	flag.BoolVar(&cfg.EnableWrites, "enable-writes", false, "allow message and approval writes; these can create Tasks or consume pending approvals")
	flag.StringVar(&cfg.AllowHost, "allow-host", "", "exact non-loopback hostname to allow; remote targets must use HTTPS")
	flag.Parse()
	cfg.Duration, cfg.Ramp, cfg.PollInterval, cfg.Reconnect = duration, ramp, pollInterval, reconnect

	if err := validateOptions(cfg); err != nil {
		fail(err)
	}
	if err := checkTargetCapabilities(cfg.BaseURL); err != nil {
		fail(err)
	}
	users, err := readScenario(cfg.ManifestPath)
	if err != nil {
		fail(err)
	}
	users, err = expandScenario(cfg, users)
	if err != nil {
		fail(err)
	}
	if err := validateScenario(cfg, users); err != nil {
		fail(err)
	}
	cfg.TaskPollScope, err = checkScenarioEndpoints(cfg.BaseURL, users[0])
	if err != nil {
		fail(err)
	}
	result, err := runLoad(context.Background(), cfg, users)
	if err != nil {
		fail(err)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		fail(errors.New("could not write load report"))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "morde-capacity:", err)
	os.Exit(2)
}

func validateOptions(cfg options) error {
	if cfg.ManifestPath == "" {
		return errors.New("-manifest is required")
	}
	if strings.TrimSpace(cfg.Environment) == "" || len(cfg.Environment) > 128 || strings.TrimSpace(cfg.SuccessCriteria) == "" || len(cfg.SuccessCriteria) > 1024 {
		return errors.New("-environment and -success-criteria are required and must be within their length limits")
	}
	if cfg.Connections < 1 || cfg.Connections > maxConnections {
		return fmt.Errorf("-connections must be between 1 and %d", maxConnections)
	}
	if cfg.TaskStreams < 0 || cfg.TaskStreams > cfg.Connections || cfg.LongTaskStreams < 0 || cfg.LongTaskStreams > cfg.TaskStreams {
		return errors.New("Task stream counts must be non-negative, and long Task streams cannot exceed total Task streams or virtual clients")
	}
	if cfg.Duration <= 0 || cfg.Ramp < 0 || cfg.Reconnect < 0 || cfg.PollInterval <= 0 {
		return errors.New("duration and poll interval must be positive; ramp and reconnect cannot be negative")
	}
	if cfg.MessageCount < 0 || cfg.ApprovalCount < 0 || cfg.MessageCount+cfg.ApprovalCount > cfg.Connections || cfg.BurstSize < 1 || cfg.BurstSize > 100 {
		return errors.New("message and approval users must fit within connections; burst size must be between 1 and 100")
	}
	if cfg.MessageCount+cfg.ApprovalCount > 0 && !cfg.EnableWrites {
		return errors.New("write scenarios require explicit -enable-writes")
	}
	if cfg.MessageCount > 0 && strings.TrimSpace(cfg.MessageText) == "" {
		return errors.New("-message-text cannot be empty when -messages is nonzero")
	}
	if cfg.ApprovalCount > 0 && cfg.ApprovalDecision != "approve" && cfg.ApprovalDecision != "reject" {
		return errors.New("-approval-decision must be approve or reject when -approvals is nonzero")
	}
	if cfg.WriteRate < 0.01 || cfg.WriteRate > 1000 || math.IsNaN(cfg.WriteRate) || math.IsInf(cfg.WriteRate, 0) {
		return errors.New("-write-rate must be between 0.01 and 1000")
	}
	if cfg.MessageCount > 0 && (len(cfg.MessageText) > 512 || strings.ContainsRune(cfg.MessageText, '\x00') || !utf8.ValidString(cfg.MessageText)) {
		return errors.New("-message-text must be at most 512 bytes, valid UTF-8, and cannot contain NUL")
	}
	base, err := url.Parse(cfg.BaseURL)
	if err != nil || base.Hostname() == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") {
		return errors.New("-base-url must be an http(s) origin without credentials, path, query, or fragment")
	}
	if err := validateTarget(base, cfg.AllowHost); err != nil {
		return err
	}
	if cfg.Ramp >= cfg.Duration && cfg.Ramp > 0 {
		return errors.New("-ramp must be shorter than -duration")
	}
	writeCount := cfg.MessageCount*cfg.BurstSize + cfg.ApprovalCount
	writeDuration := time.Duration(float64(writeCount) / cfg.WriteRate * float64(time.Second))
	if writeDuration > cfg.Duration-cfg.Ramp {
		return errors.New("configured write workload cannot finish within duration after the client ramp")
	}
	return nil
}

func validateTarget(base *url.URL, allowHost string) error {
	host := strings.TrimSuffix(strings.ToLower(base.Hostname()), ".")
	if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
		return nil
	}
	if base.Scheme != "https" {
		return errors.New("non-loopback targets require HTTPS")
	}
	if allowHost == "" || !strings.EqualFold(strings.TrimSuffix(allowHost, "."), host) {
		return errors.New("non-loopback target requires -allow-host matching the exact hostname")
	}
	return nil
}

func checkTargetCapabilities(baseURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(baseURL, "/")+"/api/capabilities", nil)
	if err != nil {
		return errors.New("could not create target capability request")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return errors.New("target capability check failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("target capability check returned an unexpected status")
	}
	var capabilities struct {
		SupportedFeatures []string `json:"supported_features"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&capabilities); err != nil {
		return errors.New("target capability response is invalid")
	}
	for _, feature := range capabilities.SupportedFeatures {
		if feature == "cloud_tasks" {
			return nil
		}
	}
	return errors.New("target does not advertise cloud_tasks; no load was started")
}

func checkScenarioEndpoints(baseURL string, user virtualUser) (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	base := strings.TrimSuffix(baseURL, "/")
	conversationPath := "/api/conversations/" + url.PathEscape(user.ConversationID)
	status, _, err := sendRead(context.Background(), client, base+conversationPath+"/messages?limit=1", user.AccessToken)
	if err != nil {
		return "", errors.New("scenario preflight failed for conversation-history endpoint")
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("scenario preflight conversation-history endpoint returned HTTP %d", status)
	}
	status, _, err = sendRead(context.Background(), client, base+conversationPath+"/tasks?view=active&limit=1", user.AccessToken)
	if err != nil {
		return "", errors.New("scenario preflight failed for conversation-active-tasks endpoint")
	}
	if status == http.StatusOK {
		return "conversation", nil
	}
	if status != http.StatusNotFound {
		return "", fmt.Errorf("scenario preflight conversation-active-tasks endpoint returned HTTP %d", status)
	}
	status, _, err = sendRead(context.Background(), client, base+"/api/tasks?limit=1", user.AccessToken)
	if err != nil {
		return "", errors.New("scenario preflight failed for account-tasks endpoint")
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("scenario preflight account-tasks endpoint returned HTTP %d", status)
	}
	return "account", nil
}

func readScenario(path string) ([]virtualUser, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, errors.New("could not read scenario manifest")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("scenario manifest must be a regular file with owner-only permissions (0600)")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("could not read scenario manifest")
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 16<<20))
	decoder.DisallowUnknownFields()
	var value scenario
	if err := decoder.Decode(&value); err != nil {
		return nil, errors.New("scenario manifest must be valid JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("scenario manifest must contain exactly one JSON value")
	}
	return value.Users, nil
}

func expandScenario(cfg options, profiles []virtualUser) ([]virtualUser, error) {
	if len(profiles) == 0 {
		return nil, errors.New("scenario manifest must contain at least one virtual-user profile")
	}
	users := make([]virtualUser, cfg.Connections)
	for i := range users {
		users[i] = profiles[i%len(profiles)]
	}
	return users, nil
}

func validateScenario(cfg options, users []virtualUser) error {
	if len(users) < cfg.Connections {
		return errors.New("expanded scenario has fewer users than requested connections")
	}
	approvalIDs := make(map[string]struct{}, cfg.ApprovalCount)
	taskIDs := make(map[string]struct{}, cfg.TaskStreams)
	longTasks := 0
	for i, user := range users {
		if !strings.HasPrefix(user.AccessToken, "chc_") {
			return fmt.Errorf("scenario user %d must contain an active restricted client credential", i+1)
		}
		if user.ActorID == "" {
			return fmt.Errorf("scenario user %d is missing actor_id", i+1)
		}
		if user.ConversationID == "" {
			return fmt.Errorf("scenario user %d is missing conversation_id", i+1)
		}
		if i < cfg.MessageCount && user.ServiceInstanceID == "" {
			return fmt.Errorf("scenario user %d is missing message-write fields", i+1)
		}
		if i < cfg.TaskStreams {
			if user.TaskID == "" {
				return fmt.Errorf("scenario user %d is missing task_id for its event stream", i+1)
			}
			if _, exists := taskIDs[user.TaskID]; exists {
				return errors.New("parallel Task event streams must use distinct Task fixtures")
			}
			taskIDs[user.TaskID] = struct{}{}
			if user.LongTask {
				longTasks++
			}
		}
		approvalIndex := i - cfg.MessageCount
		if approvalIndex >= 0 && approvalIndex < cfg.ApprovalCount {
			if user.Approval == nil || user.Approval.TaskID == "" || user.Approval.ApprovalID == "" || user.Approval.Version < 1 {
				return fmt.Errorf("scenario user %d is missing a pending approval fixture", i+1)
			}
			key := user.Approval.TaskID + "/" + user.Approval.ApprovalID
			if _, exists := approvalIDs[key]; exists {
				return errors.New("approval write scenarios must use distinct approval fixtures")
			}
			approvalIDs[key] = struct{}{}
		}
	}
	if longTasks != cfg.LongTaskStreams {
		return errors.New("-long-task-streams must equal the number of task fixtures marked long_task")
	}
	return nil
}

type report struct {
	Target          string           `json:"target_host"`
	Environment     string           `json:"environment"`
	SuccessCriteria string           `json:"success_criteria"`
	StartedAt       time.Time        `json:"started_at"`
	DurationSeconds float64          `json:"duration_seconds"`
	Configured      int              `json:"configured_virtual_clients"`
	Started         int              `json:"virtual_clients_started"`
	TaskPollScope   string           `json:"task_poll_scope"`
	Conversation    requestReport    `json:"conversation_history_polls"`
	Tasks           requestReport    `json:"active_task_polls"`
	TaskStreams     taskStreamReport `json:"task_event_streams"`
	Messages        requestReport    `json:"messages"`
	Approvals       requestReport    `json:"approval_decisions"`
	WriteRateLimit  float64          `json:"write_rate_limit_per_second"`
	Errors          map[string]int   `json:"errors"`
	ClientRuntime   runtimeReport    `json:"client_runtime"`
}

type requestReport struct {
	Count         int            `json:"count"`
	RatePerSecond float64        `json:"rate_per_second"`
	Statuses      map[string]int `json:"statuses"`
	LatencyMS     percentiles    `json:"latency_ms"`
}

type taskStreamReport struct {
	Configured        int            `json:"configured_concurrent_tasks"`
	LongRunning       int            `json:"long_running_tasks"`
	LongTaskRatio     float64        `json:"long_task_ratio"`
	Established       int            `json:"streams_established"`
	Reconnects        int            `json:"reconnects"`
	PeakActive        int            `json:"peak_active_streams"`
	Events            int            `json:"events_received"`
	Terminal          int            `json:"terminal_streams"`
	Statuses          map[string]int `json:"statuses"`
	ConnectionLatency percentiles    `json:"connection_latency_ms"`
}

type percentiles struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}

type runtimeReport struct {
	Goroutines int    `json:"peak_goroutines"`
	HeapBytes  uint64 `json:"peak_heap_bytes"`
	SysBytes   uint64 `json:"peak_sys_bytes"`
}

type counters struct {
	mu                    sync.Mutex
	started               int
	conversationLatencies []float64
	conversationStatuses  map[string]int
	taskPollLatencies     []float64
	taskPollStatuses      map[string]int
	messageLatencies      []float64
	messageStatuses       map[string]int
	approvalLatencies     []float64
	approvalStatuses      map[string]int
	taskStreamStatuses    map[string]int
	taskStreamLatencies   []float64
	taskStreams           int
	taskReconnects        int
	activeTaskStreams     int
	peakTaskStreams       int
	taskEvents            int
	terminalTaskStreams   int
	peakRuntime           runtimeReport
	errors                map[string]int
}

func newCounters() *counters {
	return &counters{
		conversationStatuses: map[string]int{}, taskPollStatuses: map[string]int{},
		messageStatuses: map[string]int{}, approvalStatuses: map[string]int{},
		taskStreamStatuses: map[string]int{}, errors: map[string]int{},
	}
}

func (c *counters) addError(kind string) {
	c.mu.Lock()
	c.errors[kind]++
	c.mu.Unlock()
}

func (c *counters) recordRuntimeSample(goroutines int, heapBytes, sysBytes uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if goroutines > c.peakRuntime.Goroutines {
		c.peakRuntime.Goroutines = goroutines
	}
	if heapBytes > c.peakRuntime.HeapBytes {
		c.peakRuntime.HeapBytes = heapBytes
	}
	if sysBytes > c.peakRuntime.SysBytes {
		c.peakRuntime.SysBytes = sysBytes
	}
}

func runLoad(parent context.Context, cfg options, users []virtualUser) (report, error) {
	base, _ := url.Parse(cfg.BaseURL)
	base.Path = ""
	ctx, cancel := context.WithTimeout(parent, cfg.Duration)
	defer cancel()
	startedAt := time.Now().UTC()
	stats := newCounters()
	maxConnections := cfg.Connections + cfg.TaskStreams + 16
	transport := &http.Transport{
		MaxIdleConns:          maxConnections,
		MaxIdleConnsPerHost:   maxConnections,
		MaxConnsPerHost:       maxConnections,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
	}
	defer transport.CloseIdleConnections()
	requestClient := &http.Client{Transport: transport, Timeout: 20 * time.Second}
	streamClient := &http.Client{Transport: transport}
	var clients sync.WaitGroup
	for i := 0; i < cfg.Connections; i++ {
		clients.Add(1)
		go func(index int, user virtualUser) {
			defer clients.Done()
			runConversationUser(ctx, base, requestClient, cfg, user, index, stats)
		}(i, users[i])
	}
	for i := 0; i < cfg.TaskStreams; i++ {
		clients.Add(1)
		go func(index int, user virtualUser) {
			defer clients.Done()
			runTaskStreamUser(ctx, base, streamClient, cfg, user, index, stats)
		}(i, users[i])
	}
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		sample := func() {
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			stats.recordRuntimeSample(runtime.NumGoroutine(), mem.HeapAlloc, mem.Sys)
		}
		sample()
		for {
			select {
			case <-ctx.Done():
				sample()
				return
			case <-ticker.C:
				sample()
			}
		}
	}()
	writesDone := make(chan struct{})
	go func() {
		defer close(writesDone)
		runWrites(ctx, base, requestClient, cfg, users, stats)
	}()
	<-ctx.Done()
	<-monitorDone
	clients.Wait()
	<-writesDone
	transport.CloseIdleConnections()

	stats.mu.Lock()
	defer stats.mu.Unlock()
	elapsed := time.Since(startedAt).Seconds()
	longRatio := 0.0
	if cfg.TaskStreams > 0 {
		longRatio = float64(cfg.LongTaskStreams) / float64(cfg.TaskStreams)
	}
	return report{
		Target: base.Hostname(), Environment: cfg.Environment, SuccessCriteria: cfg.SuccessCriteria,
		StartedAt: startedAt, DurationSeconds: elapsed, Configured: cfg.Connections, Started: stats.started, TaskPollScope: cfg.TaskPollScope,
		Conversation:   requestReport{Count: countValues(stats.conversationStatuses), RatePerSecond: float64(countValues(stats.conversationStatuses)) / elapsed, Statuses: cloneCounts(stats.conversationStatuses), LatencyMS: summarize(stats.conversationLatencies)},
		Tasks:          requestReport{Count: countValues(stats.taskPollStatuses), RatePerSecond: float64(countValues(stats.taskPollStatuses)) / elapsed, Statuses: cloneCounts(stats.taskPollStatuses), LatencyMS: summarize(stats.taskPollLatencies)},
		TaskStreams:    taskStreamReport{Configured: cfg.TaskStreams, LongRunning: cfg.LongTaskStreams, LongTaskRatio: longRatio, Established: stats.taskStreams, Reconnects: stats.taskReconnects, PeakActive: stats.peakTaskStreams, Events: stats.taskEvents, Terminal: stats.terminalTaskStreams, Statuses: cloneCounts(stats.taskStreamStatuses), ConnectionLatency: summarize(stats.taskStreamLatencies)},
		Messages:       requestReport{Count: countValues(stats.messageStatuses), RatePerSecond: float64(countValues(stats.messageStatuses)) / elapsed, Statuses: cloneCounts(stats.messageStatuses), LatencyMS: summarize(stats.messageLatencies)},
		Approvals:      requestReport{Count: countValues(stats.approvalStatuses), RatePerSecond: float64(countValues(stats.approvalStatuses)) / elapsed, Statuses: cloneCounts(stats.approvalStatuses), LatencyMS: summarize(stats.approvalLatencies)},
		WriteRateLimit: cfg.WriteRate, Errors: cloneCounts(stats.errors), ClientRuntime: stats.peakRuntime,
	}, nil
}

func runConversationUser(ctx context.Context, base *url.URL, client *http.Client, cfg options, user virtualUser, index int, stats *counters) {
	delay := time.Duration(index) * cfg.Ramp / time.Duration(cfg.Connections)
	if !pause(ctx, delay) {
		return
	}
	stats.mu.Lock()
	stats.started++
	stats.mu.Unlock()
	for ctx.Err() == nil {
		pollConversation(ctx, base, client, user, stats)
		if ctx.Err() != nil {
			return
		}
		pollTasks(ctx, base, client, cfg, user, stats)
		if !pause(ctx, cfg.PollInterval) {
			return
		}
	}
}

func pollConversation(ctx context.Context, base *url.URL, client *http.Client, user virtualUser, stats *counters) {
	path := "/api/conversations/" + url.PathEscape(user.ConversationID) + "/messages?limit=1"
	status, latency, err := sendRead(ctx, client, base.String()+path, user.AccessToken)
	if err != nil && ctx.Err() != nil {
		return
	}
	stats.mu.Lock()
	defer stats.mu.Unlock()
	if err != nil {
		stats.conversationStatuses["transport_error"]++
		stats.errors["conversation_poll_failed"]++
		return
	}
	stats.conversationStatuses[fmt.Sprint(status)]++
	stats.conversationLatencies = append(stats.conversationLatencies, latency)
}

func pollTasks(ctx context.Context, base *url.URL, client *http.Client, cfg options, user virtualUser, stats *counters) {
	path := "/api/conversations/" + url.PathEscape(user.ConversationID) + "/tasks?view=active&limit=1"
	if cfg.TaskPollScope == "account" {
		path = "/api/tasks?limit=1"
	}
	status, latency, err := sendRead(ctx, client, base.String()+path, user.AccessToken)
	if err != nil && ctx.Err() != nil {
		return
	}
	stats.mu.Lock()
	defer stats.mu.Unlock()
	if err != nil {
		stats.taskPollStatuses["transport_error"]++
		stats.errors["task_poll_failed"]++
		return
	}
	stats.taskPollStatuses[fmt.Sprint(status)]++
	stats.taskPollLatencies = append(stats.taskPollLatencies, latency)
}

func sendRead(parent context.Context, client *http.Client, target, token string) (int, float64, error) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, 0, errors.New("could not create conversation read request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	started := time.Now()
	response, err := client.Do(request)
	if err != nil {
		return 0, 0, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 256<<10))
	return response.StatusCode, float64(time.Since(started).Microseconds()) / 1000, nil
}

func runTaskStreamUser(ctx context.Context, base *url.URL, client *http.Client, cfg options, user virtualUser, index int, stats *counters) {
	delay := time.Duration(index) * cfg.Ramp / time.Duration(max(1, cfg.TaskStreams))
	if !pause(ctx, delay) {
		return
	}
	var cursor atomic.Int64
	connectedBefore := false
	for ctx.Err() == nil {
		streamCtx, cancel := context.WithCancel(ctx)
		target := base.String() + "/api/tasks/" + url.PathEscape(user.TaskID) + "/events"
		request, err := http.NewRequestWithContext(streamCtx, http.MethodGet, target, nil)
		if err != nil {
			cancel()
			stats.addError("task_stream_request_failed")
			return
		}
		request.Header.Set("Authorization", "Bearer "+user.AccessToken)
		request.Header.Set("Accept", "text/event-stream")
		if after := cursor.Load(); after > 0 {
			request.Header.Set("Last-Event-ID", strconv.FormatInt(after, 10))
		}
		started := time.Now()
		response, err := client.Do(request)
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return
			}
			stats.recordTaskStreamStatus("transport_error", "task_stream_connect_failed")
			if !pause(ctx, time.Second) {
				return
			}
			continue
		}
		if response.StatusCode == http.StatusNoContent {
			stats.recordTaskStreamStatus("204", "")
			response.Body.Close()
			cancel()
			stats.mu.Lock()
			stats.terminalTaskStreams++
			stats.mu.Unlock()
			return
		}
		contentType := strings.ToLower(response.Header.Get("Content-Type"))
		if response.StatusCode != http.StatusOK || !strings.HasPrefix(contentType, "text/event-stream") {
			stats.recordTaskStreamStatus(fmt.Sprint(response.StatusCode), "task_stream_rejected")
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			response.Body.Close()
			cancel()
			if !pause(ctx, time.Second) {
				return
			}
			continue
		}
		stats.mu.Lock()
		stats.taskStreams++
		stats.taskStreamLatencies = append(stats.taskStreamLatencies, float64(time.Since(started).Microseconds())/1000)
		stats.activeTaskStreams++
		if stats.activeTaskStreams > stats.peakTaskStreams {
			stats.peakTaskStreams = stats.activeTaskStreams
		}
		if connectedBefore {
			stats.taskReconnects++
		}
		stats.taskStreamStatuses["200"]++
		stats.mu.Unlock()
		connectedBefore = true

		type streamResult struct {
			events int
			err    error
		}
		readDone := make(chan streamResult, 1)
		go func() {
			events, readErr := readTaskEvents(response.Body, &cursor)
			readDone <- streamResult{events: events, err: readErr}
		}()
		var reconnect <-chan time.Time
		var reconnectTimer *time.Timer
		if cfg.Reconnect > 0 {
			reconnectTimer = time.NewTimer(cfg.Reconnect)
			reconnect = reconnectTimer.C
		}
		intentional := false
		var result streamResult
		select {
		case <-ctx.Done():
			intentional = true
		case <-reconnect:
			intentional = true
		case result = <-readDone:
		}
		response.Body.Close()
		cancel()
		if reconnectTimer != nil {
			reconnectTimer.Stop()
		}
		if intentional {
			result = <-readDone
		}
		stats.mu.Lock()
		stats.taskEvents += result.events
		stats.activeTaskStreams--
		if !intentional && result.err == nil {
			stats.terminalTaskStreams++
		}
		stats.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if intentional {
			continue
		}
		if result.err == nil {
			return
		}
		stats.addError("task_stream_read_failed")
		if !pause(ctx, time.Second) {
			return
		}
	}
}

func (c *counters) recordTaskStreamStatus(status, errorKind string) {
	c.mu.Lock()
	c.taskStreamStatuses[status]++
	if errorKind != "" {
		c.errors[errorKind]++
	}
	c.mu.Unlock()
}

func readTaskEvents(reader io.Reader, cursor *atomic.Int64) (int, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 256<<10)
	events := 0
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "id:") {
			if sequence, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "id:")), 10, 64); err == nil {
				cursor.Store(sequence)
			}
		}
		if strings.HasPrefix(line, "data:") {
			events++
		}
	}
	return events, scanner.Err()
}

type writeAction struct {
	kind string
	user virtualUser
}

func runWrites(ctx context.Context, base *url.URL, client *http.Client, cfg options, users []virtualUser, stats *counters) {
	if cfg.MessageCount+cfg.ApprovalCount == 0 {
		return
	}
	if !pause(ctx, cfg.Ramp) {
		return
	}
	actions := make([]writeAction, 0, cfg.MessageCount*cfg.BurstSize+cfg.ApprovalCount)
	for i := 0; i < cfg.MessageCount; i++ {
		for burst := 0; burst < cfg.BurstSize; burst++ {
			actions = append(actions, writeAction{kind: "message", user: users[i]})
		}
	}
	for i := 0; i < cfg.ApprovalCount; i++ {
		actions = append(actions, writeAction{kind: "approval", user: users[cfg.MessageCount+i]})
	}
	interval := time.Duration(float64(time.Second) / cfg.WriteRate)
	for i, action := range actions {
		if i > 0 && !pause(ctx, interval) {
			return
		}
		started := time.Now()
		status, err := sendWrite(ctx, base, client, cfg, action)
		stats.mu.Lock()
		if action.kind == "message" {
			if err != nil {
				stats.messageStatuses["transport_error"]++
				stats.errors["message_write_failed"]++
			} else {
				stats.messageStatuses[fmt.Sprint(status)]++
				stats.messageLatencies = append(stats.messageLatencies, float64(time.Since(started).Microseconds())/1000)
			}
		} else if err != nil {
			stats.approvalStatuses["transport_error"]++
			stats.errors["approval_write_failed"]++
		} else {
			stats.approvalStatuses[fmt.Sprint(status)]++
			stats.approvalLatencies = append(stats.approvalLatencies, float64(time.Since(started).Microseconds())/1000)
		}
		stats.mu.Unlock()
	}
}

func sendWrite(parent context.Context, base *url.URL, client *http.Client, cfg options, action writeAction) (int, error) {
	key, err := idempotencyKey()
	if err != nil {
		return 0, errors.New("could not create idempotency key")
	}
	var path string
	var body any
	if action.kind == "message" {
		path = "/api/conversations/" + url.PathEscape(action.user.ConversationID) + "/messages"
		body = map[string]any{"client_message_id": key, "content": cfg.MessageText}
	} else {
		approval := action.user.Approval
		path = "/api/tasks/" + url.PathEscape(approval.TaskID) + "/approvals/" + url.PathEscape(approval.ApprovalID) + "/responses"
		body = map[string]any{"version": approval.Version, "decision": cfg.ApprovalDecision}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, errors.New("could not encode load request")
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String()+path, strings.NewReader(string(payload)))
	if err != nil {
		return 0, errors.New("could not create load request")
	}
	request.Header.Set("Authorization", "Bearer "+action.user.AccessToken)
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("Content-Type", "application/json")
	if action.kind == "message" {
		request.Header.Set("X-Morde-Actor-ID", action.user.ActorID)
		request.Header.Set("X-Morde-Service-Instance-ID", action.user.ServiceInstanceID)
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return response.StatusCode, nil
}

func idempotencyKey() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "capacity-" + hex.EncodeToString(value[:]), nil
}

func pause(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func cloneCounts(values map[string]int) map[string]int {
	result := make(map[string]int, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func countValues(values map[string]int) int {
	count := 0
	for _, value := range values {
		count += value
	}
	return count
}

func summarize(values []float64) percentiles {
	if len(values) == 0 {
		return percentiles{}
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return percentiles{P50: percentile(sorted, 0.50), P95: percentile(sorted, 0.95), P99: percentile(sorted, 0.99)}
}

func percentile(sorted []float64, p float64) float64 {
	index := int(float64(len(sorted)-1) * p)
	return sorted[index]
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
