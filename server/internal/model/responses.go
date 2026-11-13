package model

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"morde/server/internal/app"
)

const (
	maxRequestBytes  = 8 << 20
	maxResponseBytes = 8 << 20
	maxStreamLine    = 1 << 20
	maxImageBytes    = 2 << 20
	maxTotalImages   = 4 << 20
	maxInputs        = 128
	maxTools         = 64
	maxTextBytes     = 1 << 20
	maxSchemaBytes   = 64 << 10
	maxModelIDBytes  = 256
	maxAPIKeyBytes   = 4096
	requestTimeout   = app.ModelCallTimeout
)

var (
	toolNameRE          = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	errResponseTooLarge = errors.New("model provider response exceeds size limit")
	errResponseRead     = errors.New("could not read model provider response")
)

// ResponsesClient calls allowed model providers without following redirects
// or allowing requests to resolve to non-public addresses.
type ResponsesClient struct {
	allowedHosts map[string]struct{}
	httpClient   *http.Client
}

// NewResponsesClient creates a production Responses API client restricted to
// the exact DNS hostnames in allowedHosts. Each new connection re-resolves and
// validates every returned address before dialing it.
func NewResponsesClient(allowedHosts []string, proxyURL string) (*ResponsesClient, error) {
	proxyAddress, err := modelProxyAddress(proxyURL)
	if err != nil {
		return nil, err
	}
	return newResponsesClient(allowedHosts, newPublicHTTPTransport(allowedHosts, proxyAddress))
}

func newResponsesClient(allowedHosts []string, transport http.RoundTripper) (*ResponsesClient, error) {
	hosts := make(map[string]struct{}, len(allowedHosts))
	for _, host := range allowedHosts {
		name, err := normalizeAllowedHost(host)
		if err != nil {
			return nil, fmt.Errorf("invalid model provider allowlist: %w", err)
		}
		hosts[name] = struct{}{}
	}
	if len(hosts) == 0 {
		return nil, errors.New("model provider allowlist is empty")
	}
	if transport == nil {
		return nil, errors.New("model provider transport is nil")
	}
	return &ResponsesClient{
		allowedHosts: hosts,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   requestTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (c *ResponsesClient) Complete(ctx context.Context, request app.ModelRequest) (app.ModelResponse, error) {
	return c.complete(ctx, request, nil)
}

func (c *ResponsesClient) complete(ctx context.Context, request app.ModelRequest, choice *toolChoice) (app.ModelResponse, error) {
	endpointURL, err := c.endpointURL(request.Endpoint)
	if err != nil {
		return app.ModelResponse{}, modelClientErrorAt(app.ModelErrorInvalidRequest, "client_validation", "")
	}
	zai := isZAIEndpoint(request.Endpoint)
	var body []byte
	if zai {
		body, err = encodeZAIRequest(request, choice)
	} else {
		body, err = encodeRequest(request, choice)
	}
	if err != nil {
		return app.ModelResponse{}, modelClientErrorAt(app.ModelErrorInvalidRequest, "client_validation", "")
	}
	if len(body) > maxRequestBytes {
		return app.ModelResponse{}, modelClientErrorAt(app.ModelErrorInvalidRequest, "client_validation", "")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(body))
	if err != nil {
		return app.ModelResponse{}, modelClientErrorAt(app.ModelErrorInvalidRequest, "client_validation", "")
	}
	req.Header.Set("Authorization", "Bearer "+request.Endpoint.APIKey)
	req.Header.Set("Content-Type", "application/json")
	if request.OnTextDelta != nil {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return app.ModelResponse{}, modelClientError(app.ModelErrorTimeout)
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return app.ModelResponse{}, ctx.Err()
		}
		var urlErr *url.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &urlErr) && urlErr.Timeout()) {
			return app.ModelResponse{}, modelClientError(app.ModelErrorTimeout)
		}
		return app.ModelResponse{}, modelClientError(app.ModelErrorTransport)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		responseBody, err := readResponseBody(resp.Body, maxResponseBytes)
		if err != nil {
			if errors.Is(err, errResponseTooLarge) {
				return app.ModelResponse{}, modelClientError(app.ModelErrorInvalidResponse)
			}
			return app.ModelResponse{}, modelClientError(app.ModelErrorTransport)
		}
		return app.ModelResponse{}, providerStatusError(resp.StatusCode, responseBody, request.Endpoint.APIKey, zai)
	}
	if request.OnTextDelta != nil {
		if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			return app.ModelResponse{}, modelClientError(app.ModelErrorInvalidResponse)
		}
		var response app.ModelResponse
		if zai {
			response, err = readZAIStream(resp.Body, request.OnTextDelta)
		} else {
			response, err = readStreamingResponse(resp.Body, request.OnTextDelta)
		}
		if err != nil {
			if ctx.Err() != nil {
				return app.ModelResponse{}, ctx.Err()
			}
			var callbackErr *streamCallbackError
			if errors.As(err, &callbackErr) {
				return app.ModelResponse{}, callbackErr.err
			}
			if errors.Is(err, errResponseTooLarge) {
				return app.ModelResponse{}, modelClientError(app.ModelErrorInvalidResponse)
			}
			return app.ModelResponse{}, modelClientError(app.ModelErrorInvalidResponse)
		}
		return response, nil
	}
	responseBody, err := readResponseBody(resp.Body, maxResponseBytes)
	if err != nil {
		if errors.Is(err, errResponseTooLarge) {
			return app.ModelResponse{}, modelClientError(app.ModelErrorInvalidResponse)
		}
		return app.ModelResponse{}, modelClientError(app.ModelErrorTransport)
	}

	var response app.ModelResponse
	if zai {
		response, err = parseZAIResponse(responseBody)
	} else {
		var result responseBodyJSON
		if err := json.Unmarshal(responseBody, &result); err != nil {
			return app.ModelResponse{}, modelClientError(app.ModelErrorInvalidResponse)
		}
		response, err = parseResponse(result)
	}
	if err != nil {
		return app.ModelResponse{}, modelClientError(app.ModelErrorInvalidResponse)
	}
	return response, nil
}

func (c *ResponsesClient) Check(ctx context.Context, endpoint app.ModelEndpoint) (app.ModelCapabilities, error) {
	imageData, err := redSquarePNG()
	if err != nil {
		return app.ModelCapabilities{}, modelClientError(app.ModelErrorInvalidRequest)
	}
	const probeTool = "morde_capability_probe"
	request := app.ModelRequest{
		Endpoint: endpoint,
		Instructions: "Inspect the supplied image. It is a single solid red square. " +
			"Call the provided function with color set to red if the image is visible.",
		Input: []app.ModelInput{{
			Type:   "message",
			Role:   "user",
			Text:   "Identify the square's color from the image and report it through the function.",
			Images: []app.ModelImage{{MediaType: "image/png", Data: imageData}},
		}},
		Tools: []app.ModelTool{{
			Name:        probeTool,
			Description: "Report the color visible in the capability probe image.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"color":{"type":"string","enum":["red"]}},"required":["color"],"additionalProperties":false}`),
			Strict:      true,
		}},
		MaxOutputTokens: 64,
		DisableThinking: true,
	}
	response, err := c.complete(ctx, request, &toolChoice{Type: "function", Name: probeTool})
	if err != nil {
		return app.ModelCapabilities{}, err
	}
	if len(response.ToolCalls) != 1 || response.ToolCalls[0].Name != probeTool {
		return app.ModelCapabilities{}, modelClientError(app.ModelErrorInvalidResponse)
	}
	var args struct {
		Color string `json:"color"`
	}
	if err := json.Unmarshal([]byte(response.ToolCalls[0].Arguments), &args); err != nil || args.Color != "red" {
		return app.ModelCapabilities{}, modelClientError(app.ModelErrorInvalidResponse)
	}
	return app.ModelCapabilities{
		Text:    true,
		Tools:   true,
		Vision:  true,
		ModelID: endpoint.ModelID,
	}, nil
}

// endpointURL treats BaseURL as the provider API base, such as
// https://api.openai.com/v1, and appends the matching provider endpoint.
func (c *ResponsesClient) endpointURL(endpoint app.ModelEndpoint) (string, error) {
	if endpoint.APIKey == "" || len(endpoint.APIKey) > maxAPIKeyBytes || strings.IndexFunc(endpoint.APIKey, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return "", errors.New("model API key is missing or invalid")
	}
	if endpoint.ModelID == "" || len(endpoint.ModelID) > maxModelIDBytes || strings.TrimSpace(endpoint.ModelID) != endpoint.ModelID {
		return "", errors.New("model ID is missing or invalid")
	}
	if len(endpoint.BaseURL) == 0 || len(endpoint.BaseURL) > 2048 {
		return "", errors.New("model base URL is missing or invalid")
	}
	u, err := url.Parse(endpoint.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || strings.HasSuffix(u.Host, ":") || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || strings.Contains(endpoint.BaseURL, "#") {
		return "", errors.New("model base URL must be an HTTPS URL without credentials, query, or fragment")
	}
	if u.Port() != "" && u.Port() != "443" {
		return "", errors.New("model base URL must use HTTPS port 443")
	}
	host := strings.ToLower(u.Hostname())
	if _, ok := c.allowedHosts[host]; !ok {
		return "", errors.New("model provider host is not allowlisted")
	}
	if net.ParseIP(host) != nil || strings.HasSuffix(host, ".") || hasInvalidPathSegment(u.Path) {
		return "", errors.New("model base URL is invalid")
	}
	if host == "api.z.ai" {
		u.Path = strings.TrimRight(u.Path, "/") + "/chat/completions"
	} else {
		u.Path = strings.TrimRight(u.Path, "/") + "/responses"
	}
	u.RawPath = ""
	return u.String(), nil
}

func encodeRequest(request app.ModelRequest, choice *toolChoice) ([]byte, error) {
	if request.MaxOutputTokens < 1 || request.MaxOutputTokens > 32768 {
		return nil, errors.New("model output token limit is invalid")
	}
	if request.Temperature != nil && (*request.Temperature < 0 || *request.Temperature > 2) {
		return nil, errors.New("model temperature is invalid")
	}
	if len(request.Input) == 0 || len(request.Input) > maxInputs || len(request.Tools) > maxTools {
		return nil, errors.New("model request input or tool count is invalid")
	}
	if len(request.Instructions) > maxTextBytes {
		return nil, errors.New("model instructions exceed size limit")
	}
	if estimateRequestBytes(request) > maxRequestBytes {
		return nil, errors.New("model request exceeds size limit")
	}
	input := make([]any, 0, len(request.Input))
	imageBytes := 0
	for _, item := range request.Input {
		mapped, used, err := encodeInput(item)
		if err != nil {
			return nil, err
		}
		imageBytes += used
		if imageBytes > maxTotalImages {
			return nil, errors.New("model image input exceeds size limit")
		}
		input = append(input, mapped)
	}
	tools := make([]responseTool, 0, len(request.Tools))
	seen := make(map[string]struct{}, len(request.Tools))
	for _, tool := range request.Tools {
		if !toolNameRE.MatchString(tool.Name) || len(tool.Description) > 4096 || len(tool.Parameters) == 0 || len(tool.Parameters) > maxSchemaBytes || !isJSONObject(tool.Parameters) {
			return nil, errors.New("model tool definition is invalid")
		}
		if _, exists := seen[tool.Name]; exists {
			return nil, errors.New("model tool names must be unique")
		}
		seen[tool.Name] = struct{}{}
		tools = append(tools, responseTool{
			Type:        "function",
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  tool.Parameters,
			Strict:      tool.Strict,
		})
	}
	if request.RequiredTool != "" {
		if _, ok := seen[request.RequiredTool]; !ok || choice != nil {
			return nil, errors.New("required model tool is invalid")
		}
		choice = &toolChoice{Type: "function", Name: request.RequiredTool}
	}
	payload := responseRequest{
		Model:           request.Endpoint.ModelID,
		Instructions:    request.Instructions,
		Input:           input,
		Tools:           tools,
		MaxOutputTokens: request.MaxOutputTokens,
		Temperature:     request.Temperature,
		Stream:          request.OnTextDelta != nil,
		ToolChoice:      choice,
	}
	// GPT-6 Luna spends a small capability probe's entire output budget on
	// its default reasoning effort unless the probe explicitly selects none.
	// Keep this opt-in limited to the verified first-party model: OpenAI-
	// compatible endpoints and older models may reject the Responses field.
	if request.ReasoningEffort != "" {
		if request.ReasoningEffort != "max" || !isOpenAIGPT6Luna(request.Endpoint) {
			return nil, errors.New("model reasoning effort is invalid")
		}
		payload.Reasoning = &responseReasoning{Effort: request.ReasoningEffort}
	} else if request.DisableThinking && isOpenAIGPT6Luna(request.Endpoint) {
		payload.Reasoning = &responseReasoning{Effort: "none"}
	}
	if len(tools) == 0 {
		payload.Tools = nil
		payload.ToolChoice = nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("could not encode model request")
	}
	return encoded, nil
}

func encodeInput(item app.ModelInput) (any, int, error) {
	switch item.Type {
	case "message":
		if item.Role != "user" && item.Role != "assistant" && item.Role != "system" && item.Role != "developer" {
			return nil, 0, errors.New("model message role is invalid")
		}
		if len(item.Text) > maxTextBytes || len(item.Images) > 8 || len(item.Parts) > 128 {
			return nil, 0, errors.New("model message exceeds size limit")
		}
		if len(item.Parts) != 0 && (item.Text != "" || len(item.Images) != 0) {
			return nil, 0, errors.New("model message content is ambiguous")
		}
		content := make([]responseInputContent, 0, len(item.Images)+len(item.Parts)+1)
		total := 0
		imageCount := 0
		appendImage := func(image app.ModelImage) error {
			imageCount++
			if imageCount > 8 || len(image.Data) == 0 || len(image.Data) > maxImageBytes || !validImageMediaType(image.MediaType) {
				return errors.New("model image input is invalid")
			}
			total += len(image.Data)
			content = append(content, responseInputContent{
				Type:     "input_image",
				ImageURL: "data:" + image.MediaType + ";base64," + base64.StdEncoding.EncodeToString(image.Data),
			})
			return nil
		}
		if item.Text != "" {
			content = append(content, responseInputContent{Type: "input_text", Text: item.Text})
		}
		for _, image := range item.Images {
			if err := appendImage(image); err != nil {
				return nil, 0, err
			}
		}
		for _, part := range item.Parts {
			switch part.Type {
			case "text":
				if part.Image != nil || len(part.Text) > maxTextBytes {
					return nil, 0, errors.New("model text content is invalid")
				}
				content = append(content, responseInputContent{Type: "input_text", Text: part.Text})
			case "image":
				if part.Image == nil || part.Text != "" {
					return nil, 0, errors.New("model image content is invalid")
				}
				if err := appendImage(*part.Image); err != nil {
					return nil, 0, err
				}
			default:
				return nil, 0, errors.New("model message content is invalid")
			}
		}
		if len(content) == 0 {
			return nil, 0, errors.New("model message is empty")
		}
		return responseInputItem{Type: "message", Role: item.Role, Content: content}, total, nil
	case "function_call":
		if item.CallID == "" || len(item.CallID) > 256 || !toolNameRE.MatchString(item.Name) || !isJSONObject([]byte(item.Arguments)) {
			return nil, 0, errors.New("model function call input is invalid")
		}
		return responseFunctionCall{Type: item.Type, CallID: item.CallID, Name: item.Name, Arguments: item.Arguments}, 0, nil
	case "function_call_output":
		if item.CallID == "" || len(item.CallID) > 256 || len(item.Output) > maxTextBytes {
			return nil, 0, errors.New("model function output input is invalid")
		}
		return responseFunctionOutput{Type: item.Type, CallID: item.CallID, Output: item.Output}, 0, nil
	default:
		return nil, 0, errors.New("model input type is invalid")
	}
}

func parseResponse(result responseBodyJSON) (app.ModelResponse, error) {
	if result.Status == "failed" {
		return app.ModelResponse{}, errors.New("model provider returned a failed response")
	}
	response := app.ModelResponse{Usage: app.ModelUsage{Known: false}, StopReason: "stop"}
	if result.Usage != nil && result.Usage.InputTokens != nil && result.Usage.OutputTokens != nil && *result.Usage.InputTokens >= 0 && *result.Usage.OutputTokens >= 0 {
		response.Usage = app.ModelUsage{InputTokens: *result.Usage.InputTokens, OutputTokens: *result.Usage.OutputTokens, Known: true}
	}
	for _, item := range result.Output {
		switch item.Type {
		case "message":
			for _, content := range item.Content {
				switch content.Type {
				case "output_text":
					response.Text += content.Text
				case "refusal":
					response.Text += content.Refusal
					response.StopReason = "refusal"
				}
			}
		case "function_call":
			if item.CallID == "" || !toolNameRE.MatchString(item.Name) || !isJSONObject([]byte(item.Arguments)) {
				return app.ModelResponse{}, errors.New("model provider returned an invalid function call")
			}
			response.ToolCalls = append(response.ToolCalls, app.ModelToolCall{CallID: item.CallID, Name: item.Name, Arguments: item.Arguments})
		}
	}
	if result.Status == "incomplete" {
		if result.IncompleteDetails != nil && result.IncompleteDetails.Reason == "max_output_tokens" {
			response.StopReason = "length"
		} else if result.IncompleteDetails != nil && result.IncompleteDetails.Reason == "content_filter" {
			response.StopReason = "content_filter"
		} else {
			response.StopReason = "incomplete"
		}
	} else if len(response.ToolCalls) > 0 && response.StopReason != "refusal" {
		response.StopReason = "tool_calls"
	}
	return response, nil
}

type streamCallbackError struct{ err error }

func (e *streamCallbackError) Error() string { return "persist model text delta" }
func (e *streamCallbackError) Unwrap() error { return e.err }

type responseStreamEvent struct {
	Type     string          `json:"type"`
	Delta    string          `json:"delta"`
	Response json.RawMessage `json:"response"`
}

func readStreamingResponse(body io.Reader, onTextDelta func(string) error) (app.ModelResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 32*1024), maxStreamLine)
	var total int64
	var eventName string
	var data []byte
	var result app.ModelResponse
	terminal := false
	dispatch := func() error {
		if len(data) == 0 {
			eventName = ""
			return nil
		}
		var event responseStreamEvent
		if err := json.Unmarshal(data, &event); err != nil || event.Type == "" || (eventName != "" && eventName != event.Type) {
			return errors.New("model provider returned an invalid stream event")
		}
		switch event.Type {
		case "response.output_text.delta":
			if event.Delta != "" {
				if err := onTextDelta(event.Delta); err != nil {
					return &streamCallbackError{err: err}
				}
			}
		case "response.completed", "response.incomplete":
			if len(event.Response) == 0 || !json.Valid(event.Response) {
				return errors.New("model provider returned an invalid terminal response")
			}
			var responseBody responseBodyJSON
			if err := json.Unmarshal(event.Response, &responseBody); err != nil {
				return errors.New("model provider returned an invalid terminal response")
			}
			if (event.Type == "response.completed" && responseBody.Status != "completed") ||
				(event.Type == "response.incomplete" && responseBody.Status != "incomplete") {
				return errors.New("model provider stream terminal status is invalid")
			}
			response, err := parseResponse(responseBody)
			if err != nil {
				return errors.New("model provider returned an invalid terminal response")
			}
			result, terminal = response, true
		case "response.failed", "error":
			return errors.New("model provider stream failed")
		}
		eventName = ""
		data = data[:0]
		return nil
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		total += int64(len(line)) + 1
		if total > maxResponseBytes {
			return app.ModelResponse{}, errResponseTooLarge
		}
		if len(line) == 0 {
			if err := dispatch(); err != nil {
				return app.ModelResponse{}, err
			}
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value, found := strings.Cut(string(line), ":")
		if !found {
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			eventName = value
		case "data":
			if len(data)+len(value)+1 > maxResponseBytes {
				return app.ModelResponse{}, errResponseTooLarge
			}
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, value...)
		}
	}
	if err := scanner.Err(); err != nil {
		return app.ModelResponse{}, err
	}
	if err := dispatch(); err != nil {
		return app.ModelResponse{}, err
	}
	if !terminal {
		return app.ModelResponse{}, errors.New("model provider stream ended before a final response")
	}
	return result, nil
}

func providerStatusError(status int, body []byte, apiKey string, zai bool) error {
	var parsed struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	if zai && parsed.Error.Code == "1113" {
		return modelClientErrorAt(app.ModelErrorQuota, "provider_status", "")
	}
	code := safeProviderCode(parsed.Error.Code)
	if code == "" {
		code = safeProviderCode(parsed.Error.Type)
	}
	if strings.Contains(code, apiKey) {
		code = ""
	}
	var kind app.ModelClientErrorKind
	switch {
	case code == "insufficient_quota" || code == "billing_hard_limit_reached":
		kind = app.ModelErrorQuota
	case status == http.StatusUnauthorized:
		kind = app.ModelErrorAuthentication
	case status == http.StatusForbidden:
		kind = app.ModelErrorAccess
	case status == http.StatusTooManyRequests:
		kind = app.ModelErrorRateLimited
	case status >= 400 && status < 500:
		kind = app.ModelErrorInvalidRequest
	case status >= 500:
		kind = app.ModelErrorUnavailable
	default:
		kind = app.ModelErrorInvalidResponse
	}
	return modelClientErrorAt(kind, "provider_status", code)
}

func readResponseBody(body io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, errResponseRead
	}
	if int64(len(data)) > limit {
		return nil, errResponseTooLarge
	}
	return data, nil
}

func normalizeAllowedHost(host string) (string, error) {
	if host == "" || strings.TrimSpace(host) != host || strings.ContainsAny(host, "/:@?#[]") || strings.HasSuffix(host, ".") {
		return "", errors.New("host must be a DNS hostname without a port")
	}
	host = strings.ToLower(host)
	if net.ParseIP(host) != nil || len(host) > 253 {
		return "", errors.New("host must be a DNS hostname")
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("host must be a valid DNS hostname")
		}
		for _, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			default:
				return "", errors.New("host must be a valid ASCII DNS hostname")
			}
		}
	}
	return host, nil
}

func hasInvalidPathSegment(path string) bool {
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

func validImageMediaType(mediaType string) bool {
	switch mediaType {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

func isJSONObject(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return false
	}
	var object map[string]json.RawMessage
	return json.Unmarshal(trimmed, &object) == nil && object != nil
}

func safeProviderCode(value string) string {
	switch value {
	case "insufficient_quota", "billing_hard_limit_reached", "invalid_api_key", "rate_limit_exceeded", "model_not_found", "invalid_request_error", "unsupported_parameter", "context_length_exceeded", "permission_denied", "server_error":
		return value
	}
	return ""
}

func estimateRequestBytes(request app.ModelRequest) int64 {
	// JSON escaping expands arbitrary string bytes by at most six, while base64
	// image URLs have a predictable four-to-three expansion.
	size := int64(4096 + 6*(len(request.Endpoint.ModelID)+len(request.Instructions)))
	for _, input := range request.Input {
		size += int64(1024 + 6*(len(input.Text)+len(input.CallID)+len(input.Name)+len(input.Arguments)+len(input.Output)))
		for _, image := range input.Images {
			if len(image.Data) > maxImageBytes || !validImageMediaType(image.MediaType) {
				return maxRequestBytes + 1
			}
			size += int64(128 + 4*((len(image.Data)+2)/3))
		}
		for _, part := range input.Parts {
			size += int64(1024 + 6*len(part.Text))
			if part.Image != nil {
				if len(part.Image.Data) > maxImageBytes || !validImageMediaType(part.Image.MediaType) {
					return maxRequestBytes + 1
				}
				size += int64(128 + 4*((len(part.Image.Data)+2)/3))
			}
		}
	}
	for _, tool := range request.Tools {
		size += int64(1024 + 6*(len(tool.Name)+len(tool.Description)+len(tool.Parameters)))
	}
	return size
}

func redSquarePNG() ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

func modelProxyAddress(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", errors.New("model proxy must be an HTTP URL with a host and port")
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil || host == "" {
		return "", errors.New("model proxy must have a host and port")
	}
	if net.ParseIP(host) == nil {
		if _, err := normalizeAllowedHost(host); err != nil {
			return "", errors.New("model proxy host is invalid")
		}
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", errors.New("model proxy port is invalid")
	}
	return net.JoinHostPort(host, port), nil
}

func newPublicHTTPTransport(allowedHosts []string, proxyAddress string) http.RoundTripper {
	allowed := make(map[string]struct{}, len(allowedHosts))
	for _, host := range allowedHosts {
		name, err := normalizeAllowedHost(host)
		if err == nil {
			allowed[name] = struct{}{}
		}
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil || port != "443" {
				return nil, errors.New("model provider address is invalid")
			}
			host = strings.ToLower(host)
			if _, ok := allowed[host]; !ok || net.ParseIP(host) != nil {
				return nil, errors.New("model provider host is not allowlisted")
			}
			if proxyAddress != "" && host == "api.openai.com" {
				// Some managed desktop resolvers deliberately return synthetic
				// non-public records for public APIs. For this fixed, allowlisted
				// provider only, delegate DNS to the explicitly configured proxy;
				// TLS still verifies api.openai.com against a public certificate.
				return dialModelProxy(ctx, dialer, proxyAddress, net.JoinHostPort(host, port))
			}
			ips, err := resolvePublicAddresses(ctx, net.DefaultResolver, host)
			if err != nil {
				return nil, err
			}
			var lastErr error
			for _, result := range ips {
				target := net.JoinHostPort(result.IP.String(), port)
				var conn net.Conn
				var dialErr error
				if proxyAddress == "" {
					conn, dialErr = dialer.DialContext(ctx, network, target)
				} else {
					conn, dialErr = dialModelProxy(ctx, dialer, proxyAddress, target)
				}
				if dialErr == nil {
					return conn, nil
				}
				lastErr = dialErr
			}
			return nil, lastErr
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   2,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return transport
}

// dialModelProxy sends only a validated public IP to the explicitly configured proxy;
// the HTTP transport still verifies TLS against the original provider hostname.
func dialModelProxy(ctx context.Context, dialer *net.Dialer, proxyAddress, target string) (net.Conn, error) {
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline := time.Now().Add(10 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, err
	}
	request := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: make(http.Header)}
	if err := request.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(io.LimitReader(conn, 64<<10))
	response, err := http.ReadResponse(reader, request) //nolint:bodyclose // A successful CONNECT turns this connection into the tunnel.
	if err != nil || response.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, errors.New("model proxy refused the connection")
	}
	buffered := make([]byte, reader.Buffered())
	if _, err := io.ReadFull(reader, buffered); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if len(buffered) != 0 {
		return &bufferedModelProxyConn{Conn: conn, reader: io.MultiReader(bytes.NewReader(buffered), conn)}, nil
	}
	return conn, nil
}

type bufferedModelProxyConn struct {
	net.Conn
	reader io.Reader
}

func (conn *bufferedModelProxyConn) Read(buffer []byte) (int, error) {
	return conn.reader.Read(buffer)
}

type addressResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

func resolvePublicAddresses(ctx context.Context, resolver addressResolver, host string) ([]net.IPAddr, error) {
	ips, err := resolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("model provider DNS lookup failed")
	}
	for _, result := range ips {
		if !isPublicIP(result.IP) {
			return nil, errors.New("model provider DNS resolved to a non-public address")
		}
	}
	return ips, nil
}

var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("168.63.129.16/32"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

func isPublicIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

type responseRequest struct {
	Model           string             `json:"model"`
	Instructions    string             `json:"instructions,omitempty"`
	Input           []any              `json:"input"`
	Tools           []responseTool     `json:"tools,omitempty"`
	ToolChoice      *toolChoice        `json:"tool_choice,omitempty"`
	MaxOutputTokens int                `json:"max_output_tokens"`
	Temperature     *float64           `json:"temperature,omitempty"`
	Stream          bool               `json:"stream,omitempty"`
	Reasoning       *responseReasoning `json:"reasoning,omitempty"`
}

type responseReasoning struct {
	Effort string `json:"effort"`
}

func isOpenAIGPT6Luna(endpoint app.ModelEndpoint) bool {
	if endpoint.ModelID != "gpt-6-luna" {
		return false
	}
	parsed, err := url.Parse(endpoint.BaseURL)
	return err == nil && strings.EqualFold(parsed.Hostname(), "api.openai.com")
}

type responseInputItem struct {
	Type    string                 `json:"type"`
	Role    string                 `json:"role"`
	Content []responseInputContent `json:"content"`
}

type responseInputContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type responseFunctionCall struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type responseFunctionOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type responseTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

type toolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type responseBodyJSON struct {
	Status            string               `json:"status"`
	Output            []responseOutputItem `json:"output"`
	Usage             *responseUsage       `json:"usage"`
	IncompleteDetails *incompleteDetails   `json:"incomplete_details"`
}

type responseOutputItem struct {
	Type      string            `json:"type"`
	CallID    string            `json:"call_id"`
	Name      string            `json:"name"`
	Arguments string            `json:"arguments"`
	Content   []responseContent `json:"content"`
}

type responseContent struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

type responseUsage struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
}

type incompleteDetails struct {
	Reason string `json:"reason"`
}

func modelClientError(kind app.ModelClientErrorKind) error {
	return &app.ModelClientError{Kind: kind}
}

func modelClientErrorAt(kind app.ModelClientErrorKind, stage, providerCode string) error {
	return &app.ModelClientError{Kind: kind, Stage: stage, ProviderCode: providerCode}
}
