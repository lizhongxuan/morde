package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	taskExecTestSandboxID = "task-sandbox-test"
	taskExecTestToken     = "task-execd-test-token"
)

func TestTaskExecStreamsBoundedSuccessfulOutput(t *testing.T) {
	var commandBody struct {
		Command    string `json:"command"`
		Cwd        string `json:"cwd"`
		Background bool   `json:"background"`
		Timeout    int64  `json:"timeout"`
	}
	server := newTaskExecTestServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/proxy/44772/command" {
			t.Errorf("unexpected Execd request: %s %s", r.Method, r.URL.Path)
			return
		}
		if got := r.Header.Get(taskExecAccessTokenHeader); got != taskExecTestToken {
			t.Errorf("Execd access token = %q, want caller-owned token", got)
			return
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", got)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&commandBody); err != nil {
			t.Errorf("decode command request: %v", err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = io.WriteString(w, ": heartbeat\r\n\r\ndata: {\"type\":\"init\"}\r\n\r\ndata: {\"type\":\"status\"}\r\n\r\ndata: {\"type\":\"ping\"}\r\n\r\nevent: stdout\r\ndata: {\"type\":\"stdout\",\"text\":\"hello \"}\r\n\r\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"stdout\",\"text\":\"world\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"execution_complete\",\"exit_code\":0}\n\n")
	})
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, err := server.client.ExecuteTaskCommand(ctx, taskExecTestSandboxID, TaskExecRequest{
		Command: "printf hello",
		Cwd:     "/workspace",
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("ExecuteTaskCommand() error = %v", err)
	}
	if !result.Completed || result.ExitCode != 0 || result.Stdout != "hello world" || result.Stderr != "" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if commandBody.Command != "printf hello" || commandBody.Cwd != "/workspace" || commandBody.Background || commandBody.Timeout != 30_000 {
		t.Fatalf("unexpected command request: %#v", commandBody)
	}
}

func TestTaskExecReturnsCompletedNonzeroExitCode(t *testing.T) {
	server := newTaskExecTestServer(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		writeTaskExecEvents(w, `{"type":"stderr","text":"failed"}`, `{"type":"error","error":{"ename":"CommandExecError","evalue":"exit status 7","exit_code":7}}`)
	})
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := server.client.ExecuteTaskCommand(ctx, taskExecTestSandboxID, validTaskExecTestRequest())
	var exitErr *TaskExecExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode != 7 {
		t.Fatalf("ExecuteTaskCommand() error = %v, want exit code 7", err)
	}
	if !result.Completed || result.ExitCode != 7 || result.Stderr != "failed" {
		t.Fatalf("completed output was lost: %#v", result)
	}
}

func TestTaskExecDoesNotTreatCommandStartFailureAsCompletedExit(t *testing.T) {
	for _, event := range []string{
		`{"type":"error","error":{"ename":"CommandExecError","evalue":"executable missing"}}`,
		`{"type":"error","error":{"ename":"CommandExecError","exit_code":0}}`,
	} {
		t.Run(event, func(t *testing.T) {
			server := newTaskExecTestServer(t, nil, func(w http.ResponseWriter, _ *http.Request) {
				writeTaskExecEvents(w, event)
			})
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := server.client.ExecuteTaskCommand(ctx, taskExecTestSandboxID, validTaskExecTestRequest())
			if err == nil || result.Completed {
				t.Fatalf("command start failure = %#v, %v; want incomplete error", result, err)
			}
			if strings.Contains(err.Error(), "executable missing") {
				t.Fatalf("Execd error detail leaked: %v", err)
			}
		})
	}
}

func TestTaskExecRejectsErrorAndMalformedStreamEvents(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "explicit error", body: "data: {\"type\":\"error\",\"text\":\"secret must not leak\"}\n\n"},
		{name: "malformed json", body: "data: {not-json}\n\n"},
		{name: "unknown event", body: "data: {\"type\":\"unknown\"}\n\n"},
		{name: "completion missing exit code", body: "data: {\"type\":\"execution_complete\"}\n\n"},
		{name: "completion absent", body: "data: {\"type\":\"stdout\",\"text\":\"partial\"}\n\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newTaskExecTestServer(t, nil, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.body)
			})
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := server.client.ExecuteTaskCommand(ctx, taskExecTestSandboxID, validTaskExecTestRequest())
			if err == nil {
				t.Fatal("ExecuteTaskCommand() unexpectedly succeeded")
			}
			if strings.Contains(err.Error(), taskExecTestToken) || strings.Contains(err.Error(), "secret must not leak") {
				t.Fatalf("error leaked sensitive content: %v", err)
			}
			if test.name == "completion absent" && (result.Completed || result.Stdout != "partial") {
				t.Fatalf("partial result = %#v, want incomplete output", result)
			}
		})
	}
}

func TestTaskExecEnforcesOutputLimitAcrossStdoutAndStderr(t *testing.T) {
	server := newTaskExecTestServer(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		writeTaskExecEvents(w, `{"type":"stdout","text":"abc"}`, `{"type":"stderr","text":"d"}`, `{"type":"execution_complete","exit_code":0}`)
	})
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request := validTaskExecTestRequest()
	request.MaxOutputBytes = 3
	if _, err := server.client.ExecuteTaskCommand(ctx, taskExecTestSandboxID, request); err == nil || !strings.Contains(err.Error(), "output exceeded") {
		t.Fatalf("ExecuteTaskCommand() error = %v, want output-limit error", err)
	}
}

func TestTaskExecRejectsDangerousEndpointHeadersAndRedirects(t *testing.T) {
	for _, header := range []string{"Host", "Proxy-Authorization", "Authorization", "Content-Length", "Cookie", "X-Forwarded-Host"} {
		t.Run("endpoint-header-"+strings.ToLower(strings.ReplaceAll(header, "-", "_")), func(t *testing.T) {
			headers := map[string]string{header: "injected-value"}
			server := newTaskExecTestServer(t, headers, func(w http.ResponseWriter, _ *http.Request) {
				t.Fatal("unsafe endpoint header reached the Execd request")
			})
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := server.client.ExecuteTaskCommand(ctx, taskExecTestSandboxID, validTaskExecTestRequest())
			if err == nil || strings.Contains(err.Error(), taskExecTestToken) {
				t.Fatalf("ExecuteTaskCommand() error = %v, want safe rejection", err)
			}
		})
	}

	var commandCalls atomic.Int32
	server := newTaskExecTestServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		commandCalls.Add(1)
		http.Redirect(w, r, "http://127.0.0.1:1/untrusted", http.StatusFound)
	})
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := server.client.ExecuteTaskCommand(ctx, taskExecTestSandboxID, validTaskExecTestRequest()); err == nil {
		t.Fatal("ExecuteTaskCommand() unexpectedly followed redirect")
	}
	if got := commandCalls.Load(); got != 1 {
		t.Fatalf("Execd requests = %d, want one request without following redirect", got)
	}
}

func TestTaskExecRequiresExactlyOneValidEndpointToken(t *testing.T) {
	for _, test := range []struct {
		name    string
		headers map[string]string
	}{
		{name: "missing", headers: map[string]string{}},
		{name: "invalid", headers: map[string]string{taskExecAccessTokenHeader: "token with spaces"}},
		{name: "duplicate casing", headers: map[string]string{
			taskExecAccessTokenHeader: taskExecTestToken, "x-execd-access-token": taskExecTestToken,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newTaskExecTestServer(t, test.headers, func(w http.ResponseWriter, _ *http.Request) {
				t.Fatal("invalid endpoint token reached Execd")
			})
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := server.client.ExecuteTaskCommand(ctx, taskExecTestSandboxID, validTaskExecTestRequest()); err == nil ||
				strings.Contains(err.Error(), taskExecTestToken) {
				t.Fatalf("ExecuteTaskCommand() error = %v, want safe token rejection", err)
			}
		})
	}
}

func TestTaskExecRejectsEndpointOutsideConfiguredOrigin(t *testing.T) {
	server := newTaskExecTestServer(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("untrusted endpoint reached")
	})
	defer server.Close()
	server.handlerEndpoint = "http://127.0.0.1:1/proxy/44772"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := server.client.ExecuteTaskCommand(ctx, taskExecTestSandboxID, validTaskExecTestRequest()); err == nil {
		t.Fatal("ExecuteTaskCommand() unexpectedly accepted an endpoint outside the configured origin")
	}
}

func TestTaskExecRejectsLoopbackEndpointOutsideAssignedPortRange(t *testing.T) {
	server := newTaskExecTestServer(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("untrusted loopback port reached the Execd request")
	})
	defer server.Close()
	server.handlerEndpoint = "http://127.0.0.1:1"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := server.client.ExecuteTaskCommand(ctx, taskExecTestSandboxID, validTaskExecTestRequest())
	if err == nil || !strings.Contains(err.Error(), "outside the configured proxy or local sandbox route") {
		t.Fatalf("ExecuteTaskCommand() error = %v, want out-of-range loopback rejection", err)
	}
}

func TestTaskExecRejectsLoopbackEndpointOutsideAssignedAddress(t *testing.T) {
	for _, endpoint := range []string{
		"http://127.0.0.2:46000",
		"http://[::ffff:127.0.0.1]:46000",
	} {
		t.Run(endpoint, func(t *testing.T) {
			if _, err := taskExecCommandURL("http://opensandbox.internal", endpoint); err == nil {
				t.Fatalf("taskExecCommandURL(%q) unexpectedly accepted an unassigned loopback address", endpoint)
			}
		})
	}
}

func TestTaskExecAllowsExplicitLoopbackExecdEndpoint(t *testing.T) {
	direct := newTaskExecDirectTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/command" {
			t.Errorf("unexpected direct Execd request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get(taskExecAccessTokenHeader); got != taskExecTestToken {
			t.Errorf("direct Execd access token = %q, want caller-owned token", got)
			return
		}
		writeTaskExecEvents(w, `{"type":"stdout","text":"local"}`, `{"type":"execution_complete","exit_code":0}`)
	}))
	defer direct.Close()

	server := newTaskExecTestServer(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("direct Execd request was sent to the OpenSandbox control endpoint")
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer server.Close()
	server.handlerEndpoint = direct.URL
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := server.client.ExecuteTaskCommand(ctx, taskExecTestSandboxID, validTaskExecTestRequest())
	if err != nil || result.Stdout != "local" || !result.Completed {
		t.Fatalf("ExecuteTaskCommand() = %#v, %v; want completed local result", result, err)
	}
}

func newTaskExecDirectTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	var listener net.Listener
	for port := taskExecHostPortRangeMin; port <= taskExecHostPortRangeMax; port++ {
		candidate, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			listener = candidate
			break
		}
	}
	if listener == nil {
		t.Fatal("no free Task Execd test port in the configured allocation range")
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	return server
}

func TestTaskExecHonorsContextCancellation(t *testing.T) {
	commandStarted := make(chan struct{}, 1)
	server := newTaskExecTestServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		commandStarted <- struct{}{}
		<-r.Context().Done()
	})
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := server.client.ExecuteTaskCommand(ctx, taskExecTestSandboxID, validTaskExecTestRequest())
		done <- err
	}()
	select {
	case <-commandStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("Execd request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ExecuteTaskCommand() error = %v, want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ExecuteTaskCommand() did not stop after context cancellation")
	}
}

func TestTaskExecRequiresDeadlineAndWorkspaceBoundaries(t *testing.T) {
	server := newTaskExecTestServer(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		writeTaskExecEvents(w, `{"type":"execution_complete","exit_code":0}`)
	})
	defer server.Close()
	if _, err := server.client.ExecuteTaskCommand(context.Background(), taskExecTestSandboxID, validTaskExecTestRequest()); err == nil {
		t.Fatal("ExecuteTaskCommand() accepted a context without a deadline")
	}

	for _, cwd := range []string{"/etc", "/workspace-escape", "/workspace/../../etc", "relative"} {
		request := validTaskExecTestRequest()
		request.Cwd = cwd
		if err := validateTaskExecRequest(request); err == nil {
			t.Fatalf("validateTaskExecRequest(%q) unexpectedly succeeded", cwd)
		}
	}
}

type taskExecTestFixture struct {
	server          *httptest.Server
	client          *Client
	handlerEndpoint string
}

func newTaskExecTestServer(t *testing.T, endpointHeaders map[string]string, commandHandler http.HandlerFunc) *taskExecTestFixture {
	t.Helper()
	if endpointHeaders == nil {
		endpointHeaders = map[string]string{taskExecAccessTokenHeader: taskExecTestToken}
	}
	fixture := &taskExecTestFixture{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/"+taskExecTestSandboxID+"/endpoints/44772":
			endpoint := fixture.handlerEndpoint
			if endpoint == "" {
				endpoint = fixture.server.URL + "/proxy/44772"
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]any{"endpoint": endpoint, "headers": endpointHeaders}); err != nil {
				t.Errorf("encode endpoint response: %v", err)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/proxy/44772/command":
			commandHandler(w, r)
		default:
			t.Errorf("unexpected task exec test request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	fixture.server = httptest.NewServer(handler)
	fixture.client = newTestClient(fixture.server.URL, nil)
	return fixture
}

func (f *taskExecTestFixture) Close() {
	f.server.Close()
}

func validTaskExecTestRequest() TaskExecRequest {
	return TaskExecRequest{
		Command: "true",
		Cwd:     "/workspace",
		Timeout: 10 * time.Second,
	}
}

func writeTaskExecEvents(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		_, _ = io.WriteString(w, "data: "+event+"\n\n")
	}
}
