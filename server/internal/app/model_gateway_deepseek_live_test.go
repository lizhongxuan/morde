package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/model"
)

func TestDeepSeekFlashDialogueThroughMordeModelGateway(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("MORDE_TEST_DEEPSEEK_KEY"))
	if key == "" {
		t.Skip("MORDE_TEST_DEEPSEEK_KEY is unset; real DeepSeek dialogue tests were not run")
	}
	client, err := model.NewResponsesClient([]string{"api.deepseek.com"}, os.Getenv("MORDE_TEST_MODEL_PROXY"))
	if err != nil {
		t.Fatal(err)
	}
	store := &liveModelCallStore{}
	resolver := &liveModelResolver{endpoint: app.ModelEndpoint{
		BaseURL: "https://api.deepseek.com", APIKey: key, ModelID: "deepseek-flash",
	}}
	gateway, err := app.NewModelGatewayService(store, resolver, client)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: "t1234567", ClientInstallationID: "i1234567",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	nextCall := 0
	complete := func(request app.ModelRequest) app.ModelResponse {
		t.Helper()
		nextCall++
		response, err := gateway.Complete(ctx, app.ModelCall{
			Source: app.ModelCallClient, Principal: principal, ModelID: "p1234567",
			CallID:  fmt.Sprintf("deepseek-live-%02d", nextCall),
			Request: request,
		})
		if err != nil {
			t.Fatalf("Morde model-gateway call %d failed: %v", nextCall, err)
		}
		if !response.Usage.Known || response.Usage.InputTokens < 1 || response.Usage.OutputTokens < 1 {
			t.Fatalf("Morde model-gateway call %d returned invalid usage: %#v", nextCall, response.Usage)
		}
		return response
	}

	t.Run("Chinese factual question", func(t *testing.T) {
		response := complete(app.ModelRequest{
			Input:           []app.ModelInput{{Type: "message", Role: "user", Text: "请只回答数字：1+1等于几？"}},
			MaxOutputTokens: 32, DisableThinking: true,
		})
		if !strings.Contains(response.Text, "2") {
			t.Fatalf("answer did not contain 2: %q", response.Text)
		}
	})

	t.Run("format instruction", func(t *testing.T) {
		response := complete(app.ModelRequest{
			Input:           []app.ModelInput{{Type: "message", Role: "user", Text: "用中文给出恰好三条降低会议时长的建议，每条单独一行并以数字开头。"}},
			MaxOutputTokens: 96, DisableThinking: true,
		})
		if strings.TrimSpace(response.Text) == "" || !strings.Contains(response.Text, "1") || !strings.Contains(response.Text, "2") || !strings.Contains(response.Text, "3") {
			t.Fatalf("formatted answer did not contain the requested three numbered items: %q", response.Text)
		}
	})

	t.Run("multi-turn context", func(t *testing.T) {
		first := complete(app.ModelRequest{
			Input:           []app.ModelInput{{Type: "message", Role: "user", Text: "请记住暗号是星河-2048，只回复收到。"}},
			MaxOutputTokens: 32, DisableThinking: true,
		})
		second := complete(app.ModelRequest{
			Input: []app.ModelInput{
				{Type: "message", Role: "user", Text: "请记住暗号是星河-2048，只回复收到。"},
				{Type: "message", Role: "assistant", Text: first.Text},
				{Type: "message", Role: "user", Text: "暗号是什么？只回复暗号。"},
			},
			MaxOutputTokens: 32, DisableThinking: true,
		})
		if !strings.Contains(second.Text, "星河-2048") {
			t.Fatalf("second turn did not use the earlier conversation context: %q", second.Text)
		}
	})

	t.Run("streamed reply", func(t *testing.T) {
		var streamed strings.Builder
		response := complete(app.ModelRequest{
			Input:           []app.ModelInput{{Type: "message", Role: "user", Text: "请只输出 STREAM_OK。"}},
			MaxOutputTokens: 32, DisableThinking: true,
			OnTextDelta: func(delta string) error { streamed.WriteString(delta); return nil },
		})
		if !strings.Contains(response.Text, "STREAM_OK") || !strings.Contains(streamed.String(), "STREAM_OK") {
			t.Fatalf("streamed reply missing expected text: final=%q streamed=%q", response.Text, streamed.String())
		}
	})

	t.Run("tool call and result continuation", func(t *testing.T) {
		tool := app.ModelTool{Name: "lookup", Description: "Look up a synthetic test value.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"key":{"type":"string"}},"required":["key"],"additionalProperties":false}`)}
		request := app.ModelRequest{
			Instructions: "Call the lookup function with key set to M-42. Do not answer before calling it.",
			Input:        []app.ModelInput{{Type: "message", Role: "user", Text: "Look up the test value."}},
			Tools:        []app.ModelTool{tool}, RequiredTool: tool.Name,
			MaxOutputTokens: 64, DisableThinking: true,
		}
		first := complete(request)
		if len(first.ToolCalls) != 1 || first.ToolCalls[0].Name != tool.Name {
			t.Fatalf("model did not return the requested tool call: %#v", first.ToolCalls)
		}
		call := first.ToolCalls[0]
		request.RequiredTool = ""
		request.Input = append(request.Input,
			app.ModelInput{Type: "function_call", CallID: call.CallID, Name: call.Name, Arguments: call.Arguments},
			app.ModelInput{Type: "function_call_output", CallID: call.CallID, Output: "Synthetic lookup result: DELTA-73."},
		)
		second := complete(request)
		if !strings.Contains(second.Text, "DELTA-73") {
			t.Fatalf("model did not incorporate the tool result: %q", second.Text)
		}
	})

	if len(store.reservations) != nextCall || len(store.settlements) != nextCall {
		t.Fatalf("usage ledger calls: calls=%d reservations=%d settlements=%d", nextCall, len(store.reservations), len(store.settlements))
	}
}
