package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type modelGatewayHTTPTest struct {
	call            app.ModelCall
	response        app.ModelResponse
	err             error
	checkErr        error
	checkKey        string
	checkCapability string
}

func (s *modelGatewayHTTPTest) CheckModelCapability(_ context.Context, _ identity.Principal, _ string, key, capability string) (app.ModelCapabilities, error) {
	s.checkKey, s.checkCapability = key, capability
	if key == "" {
		return app.ModelCapabilities{}, domain.InvalidArgument("idempotency_key", "idempotency key is invalid")
	}
	return app.ModelCapabilities{}, s.checkErr
}

func TestModelProbeSelectsCapabilityAndRequiresIdempotency(t *testing.T) {
	service := &modelGatewayHTTPTest{}
	deps := newServices().deps()
	deps.ModelGateway = service
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: portalCookieName, Value: sessionCredential()}
	withoutKey := requestWithCookieBody(router, http.MethodPost, "/api/model-connections/p1234567/check", `{"capability":"tools"}`, cookie, "https://portal.example.test")
	if withoutKey.Code != http.StatusBadRequest {
		t.Fatalf("probe without key = %d %s", withoutKey.Code, withoutKey.Body)
	}
	response := requestWithCookieBodyAndHeader(router, http.MethodPost, "/api/model-connections/p1234567/check", `{"capability":"tools"}`, cookie,
		"https://portal.example.test", "Idempotency-Key", "probe-key-0001")
	if response.Code != http.StatusOK || service.checkKey != "probe-key-0001" || service.checkCapability != "tools" {
		t.Fatalf("probe = %d %s, key=%q, capability=%q", response.Code, response.Body, service.checkKey, service.checkCapability)
	}
}

func (s *modelGatewayHTTPTest) Complete(_ context.Context, call app.ModelCall) (app.ModelResponse, error) {
	s.call = call
	if call.Request.OnTextDelta != nil {
		if err := call.Request.OnTextDelta("Hello "); err != nil {
			return app.ModelResponse{}, err
		}
		if err := call.Request.OnTextDelta("world"); err != nil {
			return app.ModelResponse{}, err
		}
	}
	return s.response, s.err
}

func TestChatCompletionsConvertsStreamingImagesAndTools(t *testing.T) {
	service := &modelGatewayHTTPTest{response: app.ModelResponse{Text: "Hello world", StopReason: "tool_calls",
		ToolCalls: []app.ModelToolCall{{CallID: "call-2", Name: "lookup", Arguments: `{}`}}, Usage: app.ModelUsage{Known: true, InputTokens: 5, OutputTokens: 3}}}
	deps := newServices().deps()
	deps.ModelGateway = service
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	client, _, err := identity.NewCredential(identity.KindClient, testTokenID)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"p1234567","stream":true,"stream_options":{"include_usage":true},"max_completion_tokens":64,` +
		`"messages":[{"role":"system","content":"system instructions"},{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]},` +
		`{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"call-1","content":"found"}],` +
		`"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`
	result := requestWithHeader(router, http.MethodPost, "/api/model/v1/chat/completions", body, "Bearer "+client, "", "Idempotency-Key", "model-test-001")
	if result.Code != http.StatusOK || !strings.HasPrefix(result.Header().Get("Content-Type"), "text/event-stream") ||
		!strings.Contains(result.Body.String(), `"content":"Hello "`) || !strings.Contains(result.Body.String(), `"tool_calls"`) ||
		!strings.Contains(result.Body.String(), `"total_tokens":8`) || !strings.Contains(result.Body.String(), "data: [DONE]") {
		t.Fatalf("stream = %d %s", result.Code, result.Body)
	}
	if service.call.Principal.Kind != identity.KindClient || service.call.ModelID != "p1234567" || service.call.CallID != "model-test-001" ||
		len(service.call.Request.Input) != 4 || service.call.Request.Input[0].Role != "system" || len(service.call.Request.Input[1].Parts) != 2 ||
		service.call.Request.Input[1].Parts[1].Type != "image" ||
		service.call.Request.Input[2].Type != "function_call" || service.call.Request.Input[3].Type != "function_call_output" || len(service.call.Request.Tools) != 1 {
		t.Fatalf("converted model request = %#v", service.call.Request)
	}
}

func TestChatCompletionsRejectsUnsupportedAndDoesNotMaskErrors(t *testing.T) {
	service := &modelGatewayHTTPTest{}
	deps := newServices().deps()
	deps.ModelGateway = service
	router, err := New(deps, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	client, _, err := identity.NewCredential(identity.KindClient, testTokenID)
	if err != nil {
		t.Fatal(err)
	}
	auth := "Bearer " + client
	for _, body := range []string{
		`{"model":"p1234567","messages":[{"role":"user","content":"hi"}],"top_p":0.4}`,
		`{"model":"p1234567","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/private"}}]}]}`,
		`{"model":"p1234567","messages":[{"role":"user","content":"hi"}],"api_key":"device-secret"}`,
	} {
		result := requestWithHeader(router, http.MethodPost, "/api/model/v1/chat/completions", body, auth, "", "Idempotency-Key", "model-test-001")
		if result.Code != http.StatusBadRequest || strings.Contains(result.Body.String(), "device-secret") {
			t.Fatalf("invalid request = %d %s", result.Code, result.Body)
		}
	}
	service.err = domain.NewError(domain.CodeQuotaExceeded, "secret detail must not leak")
	result := requestWithHeader(router, http.MethodPost, "/api/model/v1/chat/completions", `{"model":"p1234567","messages":[{"role":"user","content":"hi"}]}`, auth, "", "Idempotency-Key", "model-test-002")
	if result.Code != http.StatusTooManyRequests || strings.Contains(result.Body.String(), "secret detail") {
		t.Fatalf("budget error = %d %s", result.Code, result.Body)
	}
	service.err = errors.New("private upstream detail")
	result = requestWithHeader(router, http.MethodPost, "/api/model/v1/chat/completions", `{"model":"p1234567","stream":true,"messages":[{"role":"user","content":"hi"}]}`, auth, "", "Idempotency-Key", "model-test-003")
	if !strings.Contains(result.Body.String(), `"error"`) || strings.Contains(result.Body.String(), "private upstream detail") || strings.Contains(result.Body.String(), "[DONE]") {
		t.Fatalf("interrupted stream = %d %s", result.Code, result.Body)
	}
}
