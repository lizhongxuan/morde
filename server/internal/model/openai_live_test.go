package model

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
)

func TestOpenAIResponsesGPT6LunaLive(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("MORDE_TEST_OPENAI_KEY"))
	if key == "" {
		t.Skip("MORDE_TEST_OPENAI_KEY is unset; real model test was not run")
	}
	client, err := NewResponsesClient([]string{"api.openai.com"}, os.Getenv("MORDE_TEST_MODEL_PROXY"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	request := app.ModelRequest{
		Endpoint:        app.ModelEndpoint{BaseURL: "https://api.openai.com/v1", APIKey: key, ModelID: "gpt-6-luna"},
		Input:           []app.ModelInput{{Type: "message", Role: "user", Text: "Use the search tool for the synthetic query ALPHA-24, then report its result."}},
		Tools:           []app.ModelTool{{Name: "morde_web_search", Description: "Find a synthetic result.", Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`), Strict: true}},
		RequiredTool:    "morde_web_search",
		MaxOutputTokens: 64,
		DisableThinking: true,
	}
	first, err := client.Complete(ctx, request)
	if err != nil {
		t.Fatalf("tool request failed: %v", err)
	}
	if len(first.ToolCalls) != 1 || first.ToolCalls[0].Name != "morde_web_search" {
		t.Fatalf("tool request did not produce one search call: stop=%s calls=%d", first.StopReason, len(first.ToolCalls))
	}
	call := first.ToolCalls[0]
	request.Input = append(request.Input,
		app.ModelInput{Type: "function_call", CallID: call.CallID, Name: call.Name, Arguments: call.Arguments},
		app.ModelInput{Type: "function_call_output", CallID: call.CallID, Output: "Synthetic lookup result: BETA-42."},
	)
	request.RequiredTool = ""
	request.MaxOutputTokens = 128
	second, err := client.Complete(ctx, request)
	if err != nil {
		t.Fatalf("tool result request failed: %v", err)
	}
	if !strings.Contains(second.Text, "BETA-42") {
		t.Fatalf("model did not use the synthetic tool result: stop=%s", second.StopReason)
	}
}
