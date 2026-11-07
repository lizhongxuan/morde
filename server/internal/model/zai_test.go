package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"morde/server/internal/app"
)

func zaiTestClient(t *testing.T, transport http.RoundTripper) *ResponsesClient {
	t.Helper()
	client, err := newResponsesClient([]string{"api.z.ai"}, transport)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func zaiTestEndpoint() app.ModelEndpoint {
	return app.ModelEndpoint{BaseURL: "https://api.z.ai/api/paas/v4", APIKey: "local-test-key", ModelID: "glm-5.1"}
}

func TestZAICompletePreservesReasoningToolsAndUsage(t *testing.T) {
	var wire struct {
		Model    string `json:"model"`
		Messages []struct {
			Role             string `json:"role"`
			Content          any    `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCallID       string `json:"tool_call_id"`
			ToolCalls        []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice string `json:"tool_choice"`
		Thinking   struct {
			Type          string `json:"type"`
			ClearThinking bool   `json:"clear_thinking"`
		} `json:"thinking"`
	}
	client := zaiTestClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://api.z.ai/api/paas/v4/chat/completions" || request.Header.Get("Authorization") != "Bearer local-test-key" {
			t.Fatalf("wrong endpoint or authorization: %s", request.URL)
		}
		if err := json.NewDecoder(request.Body).Decode(&wire); err != nil {
			t.Fatal(err)
		}
		return jsonResponse(http.StatusOK, `{"choices":[{"index":0,"message":{"role":"assistant","content":"正在搜索","reasoning_content":"next private thought","tool_calls":[{"id":"call_2","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"ai\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":37,"completion_tokens":9}}`), nil
	}))
	request := app.ModelRequest{Endpoint: zaiTestEndpoint(), Instructions: "Morde persona", MaxOutputTokens: 256,
		Input: []app.ModelInput{
			{Type: "message", Role: "user", Text: "look up news"},
			{Type: "message", Role: "assistant", Parts: []app.ModelContentPart{{Type: "reasoning", Text: "private thought"}, {Type: "text", Text: "looking"}}},
			{Type: "function_call", CallID: "call_1", Name: "lookup", Arguments: `{"q":"news"}`},
			{Type: "function_call_output", CallID: "call_1", Output: "search result"},
		},
		Tools: []app.ModelTool{{Name: "lookup", Description: "Find news", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	response, err := client.Complete(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if wire.Model != "glm-5.1" || len(wire.Messages) != 4 || wire.Messages[0].Role != "system" ||
		wire.Messages[1].Role != "user" || wire.Messages[2].ReasoningContent != "private thought" ||
		wire.Messages[2].Content != "looking" || len(wire.Messages[2].ToolCalls) != 1 ||
		wire.Messages[2].ToolCalls[0].ID != "call_1" || wire.Messages[3].ToolCallID != "call_1" ||
		len(wire.Tools) != 1 || wire.Tools[0].Function.Name != "lookup" || wire.ToolChoice != "auto" ||
		wire.Thinking.Type != "enabled" || wire.Thinking.ClearThinking {
		t.Fatalf("unexpected Z.ai request: %#v", wire)
	}
	if response.Text != "正在搜索" || response.Reasoning != "next private thought" || response.StopReason != "tool_calls" ||
		len(response.ToolCalls) != 1 || response.ToolCalls[0].Arguments != `{"q":"ai"}` ||
		!response.Usage.Known || response.Usage.InputTokens != 37 || response.Usage.OutputTokens != 9 {
		t.Fatalf("unexpected Z.ai response: %#v", response)
	}
}

func TestZAIStreamKeepsReasoningPrivateAndAssemblesToolCalls(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"private thought"},"finish_reason":null}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"content":"搜索中"},"finish_reason":null}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]},"finish_reason":null}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ai\"}"}}]},"finish_reason":null}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":4}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	client := zaiTestClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Accept") != "text/event-stream" {
			t.Fatal("streaming request did not ask for an event stream")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	}))
	var visible strings.Builder
	response, err := client.Complete(context.Background(), app.ModelRequest{Endpoint: zaiTestEndpoint(), MaxOutputTokens: 128,
		Input:       []app.ModelInput{{Type: "message", Role: "user", Text: "news"}},
		Tools:       []app.ModelTool{{Name: "lookup", Description: "Find news", Parameters: json.RawMessage(`{"type":"object"}`)}},
		OnTextDelta: func(text string) error { visible.WriteString(text); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if visible.String() != "搜索中" || response.Text != "搜索中" || response.Reasoning != "private thought" ||
		response.StopReason != "tool_calls" || len(response.ToolCalls) != 1 || response.ToolCalls[0].Arguments != `{"q":"ai"}` ||
		!response.Usage.Known || response.Usage.InputTokens != 10 || response.Usage.OutputTokens != 4 {
		t.Fatalf("unexpected streamed result: visible=%q response=%#v", visible.String(), response)
	}
}

func TestZAICheckDisablesThinkingAndUsesAutoToolChoice(t *testing.T) {
	client := zaiTestClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var payload struct {
			Thinking struct {
				Type string `json:"type"`
			} `json:"thinking"`
			Choice string `json:"tool_choice"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Thinking.Type != "disabled" || payload.Choice != "auto" {
			t.Fatalf("unexpected probe settings: %#v", payload)
		}
		return jsonResponse(http.StatusOK, `{"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"probe","type":"function","function":{"name":"morde_capability_probe","arguments":{"color":"red"}}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`), nil
	}))
	result, err := client.Check(context.Background(), zaiTestEndpoint())
	if err != nil || !result.Text || !result.Tools || !result.Vision {
		t.Fatalf("capability result = %#v, %v", result, err)
	}
}

func TestZAIToolImagesRemainAfterMatchingToolOutputs(t *testing.T) {
	request := app.ModelRequest{Endpoint: zaiTestEndpoint(), MaxOutputTokens: 100, Input: []app.ModelInput{
		{Type: "message", Role: "user", Text: "check both"},
		{Type: "function_call", CallID: "call_1", Name: "screen", Arguments: `{}`},
		{Type: "function_call", CallID: "call_2", Name: "screen", Arguments: `{}`},
		{Type: "function_call_output", CallID: "call_1", Output: "screen 1"},
		{Type: "message", Role: "user", Parts: []app.ModelContentPart{{Type: "text", Text: "visual"}, {Type: "image", Image: &app.ModelImage{MediaType: "image/png", Data: []byte("image")}}}},
		{Type: "function_call_output", CallID: "call_2", Output: "screen 2"},
	}}
	body, err := encodeZAIRequest(request, nil)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Messages) != 5 || payload.Messages[1].Role != "assistant" || payload.Messages[2].Role != "tool" ||
		payload.Messages[3].Role != "tool" || payload.Messages[4].Role != "user" ||
		!strings.Contains(string(body), "data:image/png;base64,aW1hZ2U=") {
		t.Fatalf("tool output/image ordering was lost: %s", body)
	}
}

func TestZAIRejectsMalformedStreamsAndMapsQuotaError(t *testing.T) {
	client := zaiTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusTooManyRequests, `{"error":{"code":"1113","message":"billing and local-test-key"}}`), nil
	}))
	request := app.ModelRequest{Endpoint: zaiTestEndpoint(), MaxOutputTokens: 10, Input: []app.ModelInput{{Type: "message", Role: "user", Text: "hello"}}}
	_, err := client.Complete(context.Background(), request)
	if modelErrorKind(t, err) != app.ModelErrorQuota || strings.Contains(err.Error(), "local-test-key") {
		t.Fatalf("quota error was misclassified or leaked a credential: %v", err)
	}
	for _, stream := range []string{
		`data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1}}` + "\n\ndata: [DONE]\n\n",
	} {
		if _, err := readZAIStream(strings.NewReader(stream), func(string) error { return nil }); err == nil {
			t.Fatal("malformed or incomplete stream was accepted")
		}
	}
	want := errors.New("persist failed")
	_, err = readZAIStream(strings.NewReader(`data: {"choices":[{"index":0,"delta":{"content":"text"}}]}`+"\n\n"), func(string) error { return want })
	var callbackErr *streamCallbackError
	if !errors.As(err, &callbackErr) || !errors.Is(err, want) {
		t.Fatalf("callback failure = %v", err)
	}
}

func TestZAIRejectsUnsafeHostsAndUnmatchedToolOutputs(t *testing.T) {
	client := zaiTestClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unsafe request reached the transport")
		return nil, nil
	}))
	request := app.ModelRequest{Endpoint: zaiTestEndpoint(), MaxOutputTokens: 10, Input: []app.ModelInput{{Type: "message", Role: "user", Text: "hello"}}}
	request.Endpoint.BaseURL = "https://api.z.ai.evil.test/api/paas/v4"
	if _, err := client.Complete(context.Background(), request); modelErrorKind(t, err) != app.ModelErrorInvalidRequest {
		t.Fatalf("unsafe host accepted: %v", err)
	}
	request.Endpoint = zaiTestEndpoint()
	request.Input = append(request.Input, app.ModelInput{Type: "function_call_output", CallID: "missing", Output: "not linked"})
	if _, err := client.Complete(context.Background(), request); modelErrorKind(t, err) != app.ModelErrorInvalidRequest {
		t.Fatalf("unmatched tool output accepted: %v", err)
	}
}
