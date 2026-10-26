package model

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func newTestClient(t *testing.T, transport http.RoundTripper) *ResponsesClient {
	t.Helper()
	client, err := newResponsesClient([]string{"api.example.test"}, transport)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func modelErrorKind(t *testing.T, err error) app.ModelClientErrorKind {
	t.Helper()
	var clientErr *app.ModelClientError
	if !errors.As(err, &clientErr) {
		t.Fatalf("error = %v, want *app.ModelClientError", err)
	}
	return clientErr.Kind
}

func testEndpoint() app.ModelEndpoint {
	return app.ModelEndpoint{BaseURL: "https://api.example.test/v1", APIKey: "test-secret-key", ModelID: "model-test"}
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    &http.Request{},
	}
}

func TestCompleteEncodesMessagesToolsAndImagesAndParsesCallsAndUsage(t *testing.T) {
	imageBytes := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
	var requestBody map[string]json.RawMessage
	client := newTestClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://api.example.test/v1/responses" {
			t.Fatalf("URL = %q", request.URL)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer test-secret-key" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := request.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q", got)
		}
		if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
			t.Fatal(err)
		}
		if _, leaked := requestBody["api_key"]; leaked {
			t.Fatal("API key was included in the request body")
		}
		return jsonResponse(http.StatusOK, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Done"}]},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"query\":\"weather\"}"}],"usage":{"input_tokens":21,"output_tokens":7}}`), nil
	}))
	request := app.ModelRequest{
		Endpoint:     testEndpoint(),
		Instructions: "be useful",
		Input: []app.ModelInput{
			{Type: "message", Role: "user", Text: "inspect", Images: []app.ModelImage{{MediaType: "image/png", Data: imageBytes}}},
			{Type: "function_call", CallID: "call_prior", Name: "lookup", Arguments: `{"query":"weather"}`},
			{Type: "function_call_output", CallID: "call_prior", Output: "sunny"},
		},
		Tools:           []app.ModelTool{{Name: "lookup", Description: "Find data", Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`), Strict: true}},
		MaxOutputTokens: 128,
	}
	got, err := client.Complete(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "Done" || got.StopReason != "tool_calls" || !got.Usage.Known || got.Usage.InputTokens != 21 || got.Usage.OutputTokens != 7 {
		t.Fatalf("response = %#v", got)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0] != (app.ModelToolCall{CallID: "call_1", Name: "lookup", Arguments: `{"query":"weather"}`}) {
		t.Fatalf("tool calls = %#v", got.ToolCalls)
	}
	var input []map[string]json.RawMessage
	if err := json.Unmarshal(requestBody["input"], &input); err != nil {
		t.Fatal(err)
	}
	if len(input) != 3 {
		t.Fatalf("input item count = %d", len(input))
	}
	var content []map[string]string
	if err := json.Unmarshal(input[0]["content"], &content); err != nil {
		t.Fatal(err)
	}
	if len(content) != 2 || content[0]["type"] != "input_text" || content[1]["type"] != "input_image" {
		t.Fatalf("first input content = %#v", content)
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(content[1]["image_url"], prefix) {
		t.Fatalf("image URL prefix = %q", content[1]["image_url"])
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(content[1]["image_url"], prefix))
	if err != nil || string(decoded) != string(imageBytes) {
		t.Fatalf("image bytes round trip = %v, %v", decoded, err)
	}
	if string(input[1]["type"]) != `"function_call"` || string(input[2]["type"]) != `"function_call_output"` {
		t.Fatalf("function history types = %s, %s", input[1]["type"], input[2]["type"])
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(requestBody["tools"], &tools); err != nil {
		t.Fatal(err)
	}
	var strict bool
	if err := json.Unmarshal(tools[0]["strict"], &strict); err != nil || !strict {
		t.Fatalf("strict tool option = %v, err = %v", strict, err)
	}
}

func TestCompleteHandlesUnknownUsageAndIncompleteStopReason(t *testing.T) {
	client := newTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}`), nil
	}))
	got, err := client.Complete(context.Background(), app.ModelRequest{
		Endpoint: testEndpoint(), Input: []app.ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "partial" || got.StopReason != "length" || got.Usage.Known {
		t.Fatalf("response = %#v", got)
	}
}

func TestCompleteStreamsOnlyUserVisibleTextDeltasAndReturnsFinalResponse(t *testing.T) {
	body := strings.Join([]string{
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"Hello "}`,
		"",
		"event: response.reasoning_summary_text.delta",
		`data: {"type":"response.reasoning_summary_text.delta","delta":"private reasoning"}`,
		"",
		"event: response.function_call_arguments.delta",
		`data: {"type":"response.function_call_arguments.delta","delta":"private tool args"}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"world."}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Hello world."}]},{"type":"function_call","call_id":"call_stream","name":"lookup","arguments":"{\"query\":\"provider-only\"}"}],"usage":{"input_tokens":3,"output_tokens":4}}}`,
		"",
	}, "\n")
	var requestPayload struct {
		Stream bool `json:"stream"`
	}
	var deltas []string
	client := newTestClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("Accept = %q, want text/event-stream", request.Header.Get("Accept"))
		}
		if err := json.NewDecoder(request.Body).Decode(&requestPayload); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: &http.Request{},
		}, nil
	}))
	response, err := client.Complete(context.Background(), app.ModelRequest{
		Endpoint: testEndpoint(), Input: []app.ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 10,
		OnTextDelta: func(delta string) error { deltas = append(deltas, delta); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !requestPayload.Stream || response.Text != "Hello world." || response.StopReason != "tool_calls" || !response.Usage.Known || response.Usage.InputTokens != 3 {
		t.Fatalf("request stream=%t response=%#v", requestPayload.Stream, response)
	}
	if strings.Join(deltas, "") != response.Text || len(deltas) != 2 || deltas[0] != "Hello " || deltas[1] != "world." {
		t.Fatalf("user-visible deltas = %#v", deltas)
	}
	if len(response.ToolCalls) != 1 || response.ToolCalls[0].Arguments != `{"query":"provider-only"}` {
		t.Fatalf("full completed response tool call = %#v", response.ToolCalls)
	}
}

func TestCompleteReturnsIncompleteStreamAndRejectsMissingTerminalResponse(t *testing.T) {
	incomplete := strings.Join([]string{
		"event: response.output_text.delta", `data: {"type":"response.output_text.delta","delta":"Partial"}`, "",
		"event: response.incomplete", `data: {"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","content":[{"type":"output_text","text":"Partial"}]}]}}`, "",
	}, "\n")
	client := newTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(incomplete)), Request: &http.Request{}}, nil
	}))
	response, err := client.Complete(context.Background(), app.ModelRequest{
		Endpoint: testEndpoint(), Input: []app.ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 10,
		OnTextDelta: func(string) error { return nil },
	})
	if err != nil || response.StopReason != "length" || response.Text != "Partial" {
		t.Fatalf("incomplete response = %#v, %v", response, err)
	}

	client = newTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Partial\"}\n\n")), Request: &http.Request{}}, nil
	}))
	_, err = client.Complete(context.Background(), app.ModelRequest{
		Endpoint: testEndpoint(), Input: []app.ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 10,
		OnTextDelta: func(string) error { return nil },
	})
	if modelErrorKind(t, err) != app.ModelErrorInvalidResponse {
		t.Fatalf("unterminated stream error = %v, want invalid response", err)
	}
}

func TestCompleteAbortsStreamingWhenDeltaPersistenceCallbackFails(t *testing.T) {
	body := strings.Join([]string{
		"event: response.output_text.delta", `data: {"type":"response.output_text.delta","delta":"first"}`, "",
		"event: response.output_text.delta", `data: {"type":"response.output_text.delta","delta":"second"}`, "",
		"event: response.completed", `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"firstsecond"}]}]}}`, "",
	}, "\n")
	client := newTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: &http.Request{}}, nil
	}))
	failure := errors.New("synthetic persisted event failure")
	callbackCount := 0
	_, err := client.Complete(context.Background(), app.ModelRequest{
		Endpoint: testEndpoint(), Input: []app.ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 10,
		OnTextDelta: func(string) error { callbackCount++; return failure },
	})
	if !errors.Is(err, failure) || callbackCount != 1 {
		t.Fatalf("Complete() error=%v callback_count=%d; want immediate callback failure", err, callbackCount)
	}
}

func TestParseResponseMapsRefusalAndRejectsInvalidProviderResults(t *testing.T) {
	for _, result := range []responseBodyJSON{
		{Status: "failed"},
		{Output: []responseOutputItem{{Type: "function_call", CallID: "c1", Name: "lookup", Arguments: "not-json"}}},
	} {
		if _, err := parseResponse(result); err == nil {
			t.Fatalf("parseResponse(%#v) unexpectedly succeeded", result)
		}
	}
	refused, err := parseResponse(responseBodyJSON{Status: "completed", Output: []responseOutputItem{{Type: "message", Content: []responseContent{{Type: "refusal", Refusal: "cannot help"}}}}})
	if err != nil || refused.Text != "cannot help" || refused.StopReason != "refusal" {
		t.Fatalf("refusal response = %#v, %v", refused, err)
	}
	filtered, err := parseResponse(responseBodyJSON{Status: "incomplete", IncompleteDetails: &incompleteDetails{Reason: "content_filter"}})
	if err != nil || filtered.StopReason != "content_filter" {
		t.Fatalf("filtered response = %#v, %v", filtered, err)
	}
	unknown, err := parseResponse(responseBodyJSON{Status: "incomplete", IncompleteDetails: &incompleteDetails{Reason: "provider-secret"}, Usage: &responseUsage{InputTokens: int64Pointer(-1), OutputTokens: int64Pointer(0)}})
	if err != nil || unknown.StopReason != "incomplete" || unknown.Usage.Known {
		t.Fatalf("unknown incomplete response = %#v, %v", unknown, err)
	}
}

func int64Pointer(value int64) *int64 { return &value }

func TestCheckProbesToolAndImageCapabilities(t *testing.T) {
	client := newTestClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var payload struct {
			Input      []json.RawMessage `json:"input"`
			Tools      []json.RawMessage `json:"tools"`
			ToolChoice toolChoice        `json:"tool_choice"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Input) != 1 || len(payload.Tools) != 1 || payload.ToolChoice.Name != "morde_capability_probe" {
			t.Fatalf("probe payload = %#v", payload)
		}
		return jsonResponse(http.StatusOK, `{"status":"completed","output":[{"type":"function_call","call_id":"probe_1","name":"morde_capability_probe","arguments":"{\"color\":\"red\"}"}],"usage":{"input_tokens":4,"output_tokens":2}}`), nil
	}))
	got, err := client.Check(context.Background(), testEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Text || !got.Tools || !got.Vision || got.ModelID != "model-test" {
		t.Fatalf("capabilities = %#v", got)
	}
}

func TestCheckRejectsIncorrectImageProbeResult(t *testing.T) {
	client := newTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"status":"completed","output":[{"type":"function_call","call_id":"probe_1","name":"morde_capability_probe","arguments":"{\"color\":\"blue\"}"}]}`), nil
	}))
	if _, err := client.Check(context.Background(), testEndpoint()); modelErrorKind(t, err) != app.ModelErrorInvalidResponse {
		t.Fatalf("Check error = %v", err)
	}
}

func TestCheckPreservesProviderErrorKindWithoutLeakingInput(t *testing.T) {
	client := newTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusUnauthorized, `{"error":{"message":"test-secret-key private request body"}}`), nil
	}))
	_, err := client.Check(context.Background(), testEndpoint())
	if got := modelErrorKind(t, err); got != app.ModelErrorAuthentication {
		t.Fatalf("Check error kind = %q", got)
	}
	if strings.Contains(err.Error(), "test-secret-key") || strings.Contains(err.Error(), "private request body") {
		t.Fatalf("Check error leaked provider details: %v", err)
	}
}

func TestEndpointValidationBlocksUnsafeURLsAndAllowlistMisses(t *testing.T) {
	client := newTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unsafe endpoint reached the transport")
		return nil, nil
	}))
	for _, baseURL := range []string{
		"http://api.example.test/v1",
		"https://user:pass@api.example.test/v1",
		"https://api.example.test/v1?query=1",
		"https://api.example.test/v1#fragment",
		"https://api.example.test/v1#",
		"https://other.example.test/v1",
		"https://api.example.test:8443/v1",
		"https://api.example.test:/v1",
		"https://127.0.0.1/v1",
		"https://api.example.test/a/../v1",
	} {
		t.Run(baseURL, func(t *testing.T) {
			endpoint := testEndpoint()
			endpoint.BaseURL = baseURL
			_, err := client.Complete(context.Background(), app.ModelRequest{Endpoint: endpoint, Input: []app.ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 10})
			if modelErrorKind(t, err) != app.ModelErrorInvalidRequest {
				t.Fatalf("Complete(%q) unexpectedly succeeded", baseURL)
			}
		})
	}
	if _, err := newResponsesClient([]string{"api.example.test:443"}, roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, nil })); err == nil {
		t.Fatal("allowlist entry with a port was accepted")
	}
	if _, err := newResponsesClient(nil, roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, nil })); err == nil {
		t.Fatal("empty allowlist was accepted")
	}
	if _, err := newResponsesClient([]string{"api.example.test"}, nil); err == nil {
		t.Fatal("nil transport was accepted")
	}
	if _, err := newResponsesClient([]string{"api.example.test/evil"}, roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, nil })); err == nil {
		t.Fatal("invalid allowlist host was accepted")
	}
}

func TestProductionClientUsesTLSAndGuardedDialer(t *testing.T) {
	client, err := NewResponsesClient([]string{"api.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || transport.TLSClientConfig == nil || transport.TLSClientConfig.InsecureSkipVerify || transport.DialContext == nil {
		t.Fatalf("production transport is not guarded: %#v", client.httpClient.Transport)
	}
	if _, err := transport.DialContext(context.Background(), "tcp", "other.example.test:443"); err == nil || !strings.Contains(err.Error(), "allowlisted") {
		t.Fatalf("unlisted host dial error = %v", err)
	}
	if _, err := transport.DialContext(context.Background(), "tcp", "api.example.test:8443"); err == nil || !strings.Contains(err.Error(), "address is invalid") {
		t.Fatalf("non-HTTPS port dial error = %v", err)
	}
	if _, err := transport.DialContext(context.Background(), "tcp", "api.example.test:"); err == nil || !strings.Contains(err.Error(), "address is invalid") {
		t.Fatalf("empty port dial error = %v", err)
	}
}

type staticResolver struct {
	addresses []net.IPAddr
	err       error
}

func (resolver staticResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return resolver.addresses, resolver.err
}

func TestResolvePublicAddressesRejectsPrivateOrMixedAnswers(t *testing.T) {
	for _, resolver := range []staticResolver{
		{},
		{err: errors.New("dns failed")},
		{addresses: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}},
		{addresses: []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("169.254.169.254")}}},
	} {
		if _, err := resolvePublicAddresses(context.Background(), resolver, "api.example.test"); err == nil {
			t.Errorf("resolvePublicAddresses(%#v) unexpectedly succeeded", resolver)
		}
	}
	addresses, err := resolvePublicAddresses(context.Background(), staticResolver{addresses: []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("2001:4860:4860::8888")}}}, "api.example.test")
	if err != nil || len(addresses) != 2 {
		t.Fatalf("public DNS answers = %#v, %v", addresses, err)
	}
}

func TestIsPublicIPRejectsPrivateAndSpecialAddresses(t *testing.T) {
	for _, address := range []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "168.63.129.16", "192.0.2.4", "198.18.0.1", "::1", "fd00::1", "fe80::1", "2001:db8::1", "2002::1",
	} {
		if isPublicIP(net.ParseIP(address)) {
			t.Errorf("isPublicIP(%s) = true, want false", address)
		}
	}
	for _, address := range []string{"8.8.8.8", "1.1.1.1", "2001:4860:4860::8888"} {
		if !isPublicIP(net.ParseIP(address)) {
			t.Errorf("isPublicIP(%s) = false, want true", address)
		}
	}
}

func TestProviderErrorsAreClassifiedAndDoNotExposeCredentialsOrBody(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   app.ModelClientErrorKind
	}{
		{name: "unauthorized", status: 401, body: `{"error":{"message":"test-secret-key user prompt"}}`, want: app.ModelErrorAuthentication},
		{name: "forbidden", status: 403, body: `{}`, want: app.ModelErrorAccess},
		{name: "rate limited", status: 429, body: `{"error":{"type":"rate_limit_exceeded"}}`, want: app.ModelErrorRateLimited},
		{name: "quota", status: 400, body: `{"error":{"code":"insufficient_quota","message":"test-secret-key"}}`, want: app.ModelErrorQuota},
		{name: "other client error", status: 400, body: `{"error":{"code":"invalid_request_error","message":"test-secret-key"}}`, want: app.ModelErrorInvalidRequest},
		{name: "server", status: 503, body: `{"error":{"message":"test-secret-key"}}`, want: app.ModelErrorUnavailable},
		{name: "untrusted response header", status: 401, body: `{}`, want: app.ModelErrorAuthentication},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				response := jsonResponse(test.status, test.body)
				response.Header.Set("x-request-id", "private prompt test-secret-key")
				return response, nil
			}))
			_, err := client.Complete(context.Background(), app.ModelRequest{
				Endpoint: testEndpoint(), Input: []app.ModelInput{{Type: "message", Role: "user", Text: "private prompt"}}, MaxOutputTokens: 10,
			})
			if got := modelErrorKind(t, err); got != test.want {
				t.Fatalf("error kind = %q, want %q", got, test.want)
			}
			if strings.Contains(err.Error(), "test-secret-key") || strings.Contains(err.Error(), "private prompt") || strings.Contains(err.Error(), "user prompt") {
				t.Fatalf("error leaked sensitive data: %v", err)
			}
		})
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	calls := 0
	client := newTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		response := jsonResponse(http.StatusFound, `{}`)
		response.Header.Set("Location", "https://169.254.169.254/latest/meta-data/")
		return response, nil
	}))
	_, err := client.Complete(context.Background(), app.ModelRequest{
		Endpoint: testEndpoint(), Input: []app.ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 10,
	})
	if got := modelErrorKind(t, err); got != app.ModelErrorInvalidResponse || calls != 1 {
		t.Fatalf("redirect result = %v, calls = %d", err, calls)
	}
}

func TestCompleteTimeoutAndResponseBounds(t *testing.T) {
	t.Run("context deadline", func(t *testing.T) {
		client := newTestClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		_, err := client.Complete(ctx, app.ModelRequest{
			Endpoint: testEndpoint(), Input: []app.ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 10,
		})
		if got := modelErrorKind(t, err); got != app.ModelErrorTimeout || strings.Contains(err.Error(), "test-secret-key") {
			t.Fatalf("timeout error = %v", err)
		}
	})
	t.Run("transport error redaction", func(t *testing.T) {
		client := newTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("test-secret-key private request body")
		}))
		_, err := client.Complete(context.Background(), app.ModelRequest{
			Endpoint: testEndpoint(), Input: []app.ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 10,
		})
		if got := modelErrorKind(t, err); got != app.ModelErrorTransport || strings.Contains(err.Error(), "test-secret-key") || strings.Contains(err.Error(), "private request body") {
			t.Fatalf("transport error = %v", err)
		}
	})
	t.Run("response too large", func(t *testing.T) {
		client := newTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(strings.Repeat("x", maxResponseBytes+1)))}, nil
		}))
		_, err := client.Complete(context.Background(), app.ModelRequest{
			Endpoint: testEndpoint(), Input: []app.ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 10,
		})
		if got := modelErrorKind(t, err); got != app.ModelErrorInvalidResponse {
			t.Fatalf("oversize response error = %v", err)
		}
	})
}

func TestRequestInputAndOutputAreBoundedAndValidated(t *testing.T) {
	client := newTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid request reached the transport")
		return nil, nil
	}))
	for _, test := range []struct {
		name string
		edit func(*app.ModelRequest)
	}{
		{name: "invalid role", edit: func(r *app.ModelRequest) { r.Input[0].Role = "tool" }},
		{name: "invalid function json", edit: func(r *app.ModelRequest) {
			r.Input[0] = app.ModelInput{Type: "function_call", CallID: "x", Name: "lookup", Arguments: "not-json"}
		}},
		{name: "invalid image media type", edit: func(r *app.ModelRequest) {
			r.Input[0].Images = []app.ModelImage{{MediaType: "text/html", Data: []byte("x")}}
		}},
		{name: "oversize request text", edit: func(r *app.ModelRequest) { r.Input[0].Text = strings.Repeat("x", maxRequestBytes) }},
		{name: "invalid tool schema", edit: func(r *app.ModelRequest) {
			r.Tools = []app.ModelTool{{Name: "lookup", Parameters: json.RawMessage(`[]`)}}
		}},
		{name: "invalid function output", edit: func(r *app.ModelRequest) {
			r.Input[0] = app.ModelInput{Type: "function_call_output", Output: "orphan"}
		}},
		{name: "invalid output token cap", edit: func(r *app.ModelRequest) { r.MaxOutputTokens = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := app.ModelRequest{Endpoint: testEndpoint(), Input: []app.ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 10}
			test.edit(&request)
			_, err := client.Complete(context.Background(), request)
			if got := modelErrorKind(t, err); got != app.ModelErrorInvalidRequest {
				t.Fatalf("Complete error kind = %q", got)
			}
		})
	}
	request := app.ModelRequest{Endpoint: testEndpoint(), Input: []app.ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 10}
	request.Endpoint.APIKey = "bad\nsecret"
	_, err := client.Complete(context.Background(), request)
	if got := modelErrorKind(t, err); got != app.ModelErrorInvalidRequest {
		t.Fatalf("invalid API key error = %v", err)
	}
}

func TestEndpointHostMatchingIsCaseInsensitiveButExact(t *testing.T) {
	client := newTestClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if got := request.URL.Hostname(); got != "api.example.test" && got != "API.EXAMPLE.TEST" {
			t.Fatalf("host = %q", got)
		}
		return jsonResponse(http.StatusOK, `{"status":"completed","output":[]}`), nil
	}))
	endpoint := testEndpoint()
	endpoint.BaseURL = "https://API.EXAMPLE.TEST/v1"
	if _, err := client.Complete(context.Background(), app.ModelRequest{Endpoint: endpoint, Input: []app.ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 10}); err != nil {
		t.Fatal(err)
	}
	if isPublicIP(net.ParseIP("169.254.169.254")) {
		t.Fatal("link-local metadata IP unexpectedly passed public address check")
	}
}
