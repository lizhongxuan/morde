package orchestrator

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	execdPort                    = 44772
	defaultTaskExecOutputLimit   = 64 << 10
	maxTaskExecCommandBytes      = 16 << 10
	maxTaskExecEventBytes        = 1 << 20
	maxTaskExecTokenBytes        = 4 << 10
	taskExecHostPortRangeMin     = 46000
	taskExecHostPortRangeMax     = 46999
	minTaskExecTimeout           = time.Second
	maxTaskExecTimeout           = 5 * time.Minute
	taskExecAccessTokenHeader    = "X-EXECD-ACCESS-TOKEN"
	taskExecWorkspaceRoot        = "/workspace"
	taskExecWorkspaceRootWithSep = "/workspace/"
)

// TaskExecRequest is one bounded command request to a Task-owned sandbox.
type TaskExecRequest struct {
	Command        string
	Cwd            string
	Timeout        time.Duration
	MaxOutputBytes int
}

// TaskExecResult is the bounded output of a completed Execd invocation.
type TaskExecResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Completed bool
}

// TaskExecExitError reports a completed command with a nonzero exit code.
type TaskExecExitError struct {
	ExitCode int
}

func (e *TaskExecExitError) Error() string {
	return fmt.Sprintf("task execd command exited with status %d", e.ExitCode)
}

// ExecuteTaskCommand resolves the Task sandbox's Execd endpoint through the
// configured OpenSandbox client and streams one bounded command result.
func (c *Client) ExecuteTaskCommand(ctx context.Context, sandboxID string, input TaskExecRequest) (TaskExecResult, error) {
	if c == nil || c.http == nil || ctx == nil {
		return TaskExecResult{}, errors.New("task execd client is not configured")
	}
	if strings.TrimSpace(sandboxID) == "" {
		return TaskExecResult{}, errors.New("task execd sandbox id is required")
	}
	if err := validateTaskExecRequest(input); err != nil {
		return TaskExecResult{}, err
	}
	if input.MaxOutputBytes == 0 {
		input.MaxOutputBytes = defaultTaskExecOutputLimit
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return TaskExecResult{}, errors.New("task execd command requires a caller deadline")
	}

	endpoint, err := c.Endpoint(ctx, sandboxID, execdPort)
	if err != nil {
		return TaskExecResult{}, err
	}
	target, err := taskExecCommandURL(c.profile.BaseURL, endpoint.BaseURL)
	if err != nil {
		return TaskExecResult{}, err
	}
	remaining := time.Until(deadline)
	if remaining < minTaskExecTimeout {
		if err := ctx.Err(); err != nil {
			return TaskExecResult{}, err
		}
		return TaskExecResult{}, context.DeadlineExceeded
	}
	if input.Timeout > remaining {
		input.Timeout = remaining
	}
	request, err := newTaskExecHTTPRequest(ctx, target, endpoint.Headers, input)
	if err != nil {
		return TaskExecResult{}, err
	}

	client := *c.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return TaskExecResult{}, ctx.Err()
		}
		return TaskExecResult{}, errors.New("connect to task execd endpoint")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return TaskExecResult{}, fmt.Errorf("task execd returned HTTP %d", response.StatusCode)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		return TaskExecResult{}, errors.New("task execd returned an unsupported content type")
	}

	result, err := readTaskExecEvents(response.Body, input.MaxOutputBytes)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, err
	}
	if result.ExitCode != 0 {
		return result, &TaskExecExitError{ExitCode: result.ExitCode}
	}
	return result, nil
}

func validateTaskExecRequest(input TaskExecRequest) error {
	if strings.TrimSpace(input.Command) == "" || len(input.Command) > maxTaskExecCommandBytes || strings.ContainsRune(input.Command, '\x00') {
		return errors.New("task execd command is invalid")
	}
	if input.Timeout < minTaskExecTimeout || input.Timeout > maxTaskExecTimeout {
		return errors.New("task execd timeout is outside the allowed range")
	}
	if input.MaxOutputBytes == 0 {
		input.MaxOutputBytes = defaultTaskExecOutputLimit
	}
	if input.MaxOutputBytes < 1 || input.MaxOutputBytes > defaultTaskExecOutputLimit {
		return errors.New("task execd output limit is outside the allowed range")
	}
	cleanCwd := path.Clean(input.Cwd)
	if !path.IsAbs(input.Cwd) || (cleanCwd != taskExecWorkspaceRoot && !strings.HasPrefix(cleanCwd, taskExecWorkspaceRootWithSep)) {
		return errors.New("task execd working directory must be inside /workspace")
	}
	return nil
}

func validTaskExecToken(value string) bool {
	if value == "" || len(value) > maxTaskExecTokenBytes || strings.TrimSpace(value) != value {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < 0x21 || value[index] > 0x7e {
			return false
		}
	}
	return true
}

func taskExecCommandURL(profileBase, endpointBase string) (*url.URL, error) {
	profileURL, err := url.Parse(strings.TrimSpace(profileBase))
	if err != nil || !validTaskExecBaseURL(profileURL) {
		return nil, errors.New("opensandbox profile URL is invalid")
	}
	endpointURL, err := url.Parse(strings.TrimSpace(endpointBase))
	if err != nil || !validTaskExecBaseURL(endpointURL) {
		return nil, errors.New("opensandbox execd endpoint is invalid")
	}
	proxyEndpoint := sameTaskExecOrigin(profileURL, endpointURL) && endpointURL.Path == "/proxy/44772"
	directLoopbackEndpoint := strings.EqualFold(profileURL.Scheme, endpointURL.Scheme) && endpointURL.Path == "" &&
		validTaskExecDirectLoopback(endpointURL) && !sameTaskExecOrigin(profileURL, endpointURL)
	if !proxyEndpoint && !directLoopbackEndpoint {
		return nil, errors.New("opensandbox execd endpoint is outside the configured proxy or local sandbox route")
	}
	endpointURL.Path = strings.TrimRight(endpointURL.Path, "/") + "/command"
	endpointURL.RawPath = ""
	return endpointURL, nil
}

func validTaskExecDirectLoopback(parsed *url.URL) bool {
	if parsed == nil {
		return false
	}
	hostname := parsed.Hostname()
	ip := net.ParseIP(hostname)
	port, err := strconv.Atoi(parsed.Port())
	return hostname == "127.0.0.1" && ip != nil && ip.Equal(net.IPv4(127, 0, 0, 1)) && err == nil &&
		port >= taskExecHostPortRangeMin && port <= taskExecHostPortRangeMax
}

func validTaskExecBaseURL(parsed *url.URL) bool {
	return parsed != nil && parsed.Opaque == "" && (parsed.Scheme == "http" || parsed.Scheme == "https") &&
		parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && !parsed.ForceQuery && parsed.Fragment == ""
}

func sameTaskExecOrigin(left, right *url.URL) bool {
	if !strings.EqualFold(left.Scheme, right.Scheme) || !strings.EqualFold(left.Hostname(), right.Hostname()) {
		return false
	}
	return taskExecPortFor(left) == taskExecPortFor(right)
}

func taskExecPortFor(parsed *url.URL) string {
	if port := parsed.Port(); port != "" {
		return port
	}
	if parsed.Scheme == "https" {
		return "443"
	}
	return "80"
}

func newTaskExecHTTPRequest(ctx context.Context, target *url.URL, headers map[string]string, input TaskExecRequest) (*http.Request, error) {
	if target == nil {
		return nil, errors.New("task execd endpoint is missing")
	}
	commandTimeout := input.Timeout.Milliseconds()
	if commandTimeout < 1 {
		return nil, errors.New("task execd timeout is invalid")
	}
	body, err := json.Marshal(struct {
		Command    string `json:"command"`
		Cwd        string `json:"cwd"`
		Background bool   `json:"background"`
		Timeout    int64  `json:"timeout"`
	}{Command: input.Command, Cwd: path.Clean(input.Cwd), Timeout: commandTimeout})
	if err != nil {
		return nil, errors.New("encode task execd command")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("build task execd request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	var accessToken string
	foundAccessToken := false
	for key, value := range headers {
		if !validTaskExecHeaderName(key) || len(value) > maxTaskExecTokenBytes || strings.ContainsAny(value, "\r\n\x00") {
			return nil, errors.New("opensandbox returned an invalid execd header")
		}
		if strings.EqualFold(key, taskExecAccessTokenHeader) {
			if foundAccessToken || !validTaskExecToken(value) {
				return nil, errors.New("opensandbox returned an invalid execd access token")
			}
			accessToken, foundAccessToken = value, true
			continue
		}
		// The Task Execd contract needs only its endpoint-scoped access token.
		// Reject every other provider-supplied routing or ambient credential header.
		return nil, errors.New("opensandbox returned an unsupported execd header")
	}
	if !foundAccessToken {
		return nil, errors.New("opensandbox execd access token is missing")
	}
	request.Header.Set(taskExecAccessTokenHeader, accessToken)
	return request, nil
}

func validTaskExecHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= '0' && character <= '9') || (character >= 'A' && character <= 'Z') ||
			(character >= 'a' && character <= 'z') || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(character)) {
			continue
		}
		return false
	}
	return true
}

func readTaskExecEvents(reader io.Reader, maxOutput int) (TaskExecResult, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), maxTaskExecEventBytes)
	var result TaskExecResult
	var eventData []string
	var outputBytes int
	var sawCompletion bool
	processPayload := func(payload string) error {
		if strings.TrimSpace(payload) == "" {
			return nil
		}
		var event struct {
			Type          string `json:"type"`
			Text          string `json:"text"`
			ExitCode      *int   `json:"exit_code"`
			ExitCodeCamel *int   `json:"exitCode"`
			Error         *struct {
				Name     string `json:"ename"`
				ExitCode *int   `json:"exit_code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return errors.New("decode task execd stream event")
		}
		if event.Type == "" || sawCompletion {
			return errors.New("task execd stream event is out of order")
		}
		if event.Type == "execution_complete" && event.ExitCode == nil {
			event.ExitCode = event.ExitCodeCamel
		}
		switch event.Type {
		case "init", "status", "ping":
			return nil
		case "stdout", "stderr":
			outputBytes += len(event.Text)
			if outputBytes > maxOutput {
				return errors.New("task execd output exceeded the allowed limit")
			}
			if event.Type == "stdout" {
				result.Stdout += event.Text
			} else {
				result.Stderr += event.Text
			}
			return nil
		case "error":
			// OpenSandbox Execd terminates a foreground command with an
			// `error` event when it exits nonzero. Only CommandExecError with
			// an explicit exit code proves the process ran and finished; a
			// missing code can also mean that process startup failed.
			if event.Error != nil && event.Error.Name == "CommandExecError" && event.Error.ExitCode != nil && *event.Error.ExitCode != 0 {
				result.ExitCode = *event.Error.ExitCode
				result.Completed = true
				sawCompletion = true
				return nil
			}
			return errors.New("task execd command failed")
		case "execution_complete":
			if event.ExitCode == nil {
				return errors.New("task execd completion event is missing an exit code")
			}
			result.ExitCode = *event.ExitCode
			result.Completed = true
			sawCompletion = true
			return nil
		default:
			return errors.New("task execd stream event type is unsupported")
		}
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if len(eventData) > 0 {
				if err := processPayload(strings.Join(eventData, "\n")); err != nil {
					return result, err
				}
				eventData = eventData[:0]
			}
			continue
		}
		if strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "id:") || strings.HasPrefix(line, "retry:") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			eventData = append(eventData, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "{") {
			if len(eventData) > 0 {
				if err := processPayload(strings.Join(eventData, "\n")); err != nil {
					return result, err
				}
				eventData = eventData[:0]
			}
			if err := processPayload(line); err != nil {
				return result, err
			}
			continue
		}
		return result, errors.New("task execd stream contains an invalid event line")
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return result, errors.New("task execd stream event exceeded the allowed limit")
		}
		return result, errors.New("read task execd stream")
	}
	if len(eventData) > 0 {
		if err := processPayload(strings.Join(eventData, "\n")); err != nil {
			return result, err
		}
	}
	if !result.Completed {
		return result, errors.New("task execd stream ended before command completion")
	}
	return result, nil
}
