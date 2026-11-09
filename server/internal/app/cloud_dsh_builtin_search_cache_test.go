package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"morde/server/internal/domain/task"
)

func TestCloudDSHBuiltinSearchCacheIsTaskScopedAndExpires(t *testing.T) {
	now := time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC)
	cache := newCloudDSHBuiltinSearchCache()
	cache.now = func() time.Time { return now }
	delegate := &cloudDSHMCPClientStub{result: MCPConnectorToolResult{
		Text: []string{"result"}, StructuredContent: json.RawMessage(`{"count":1}`),
	}}
	current := task.Task{
		ID: "task-1", OwnerID: "owner-1", InitiatorID: "actor-1", Authorization: task.Authorization{Version: 3},
	}
	client := cloudDSHCachedBuiltinSearchClient{delegate: delegate, cache: cache, task: current}
	arguments := json.RawMessage(`{"query":"AI news","time_range":"week"}`)

	first, err := client.CallTool(context.Background(), "", nil, "searxng_web_search", arguments)
	if err != nil || delegate.callCalls != 1 || len(first.Text) != 1 || first.Text[0] != "result" {
		t.Fatalf("first search = %#v, err=%v, calls=%d", first, err, delegate.callCalls)
	}
	first.Text[0] = "mutated by the caller"
	first.StructuredContent[2] = 'x'
	second, err := client.CallTool(context.Background(), "", nil, "searxng_web_search", arguments)
	if err != nil || delegate.callCalls != 1 || second.Text[0] != "result" || string(second.StructuredContent) != `{"count":1}` {
		t.Fatalf("cached result was not an isolated copy: %#v, err=%v, calls=%d", second, err, delegate.callCalls)
	}
	cancelledContext, cancelCachedCall := context.WithCancel(context.Background())
	cancelCachedCall()
	if _, err := client.CallTool(cancelledContext, "", nil, "searxng_web_search", arguments); !errors.Is(err, context.Canceled) || delegate.callCalls != 1 {
		t.Fatalf("cancelled cache hit = err %v, remote calls %d; want context cancellation and no dispatch", err, delegate.callCalls)
	}

	for name, changed := range map[string]task.Task{
		"other owner":         func() task.Task { value := current; value.OwnerID = "owner-2"; return value }(),
		"other initiator":     func() task.Task { value := current; value.InitiatorID = "actor-2"; return value }(),
		"other project":       func() task.Task { value := current; value.ProjectID = "project-2"; return value }(),
		"other task":          func() task.Task { value := current; value.ID = "task-2"; return value }(),
		"other authorization": func() task.Task { value := current; value.Authorization.Version++; return value }(),
	} {
		t.Run(name, func(t *testing.T) {
			other := cloudDSHCachedBuiltinSearchClient{delegate: delegate, cache: cache, task: changed}
			if _, err := other.CallTool(context.Background(), "", nil, "searxng_web_search", arguments); err != nil {
				t.Fatal(err)
			}
		})
	}
	if delegate.callCalls != 6 {
		t.Fatalf("task/authorization scope reused another scope's result: calls=%d, want 6", delegate.callCalls)
	}
	if _, err := client.CallTool(context.Background(), "", nil, "searxng_web_search", json.RawMessage(`{"query":"different"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CallTool(context.Background(), "", nil, "web_url_read", json.RawMessage(`{"url":"https://example.com"}`)); err != nil {
		t.Fatal(err)
	}
	if delegate.callCalls != 8 {
		t.Fatalf("different query or page read used cached news: calls=%d, want 8", delegate.callCalls)
	}

	now = now.Add(cloudDSHBuiltinSearchCacheTTL + time.Nanosecond)
	if _, err := client.CallTool(context.Background(), "", nil, "searxng_web_search", arguments); err != nil {
		t.Fatal(err)
	}
	if delegate.callCalls != 9 {
		t.Fatalf("expired news result was reused: calls=%d, want 9", delegate.callCalls)
	}
}

func TestCloudDSHBuiltinSearchCacheSkipsErrorsEmptyResultsAndCancelledCalls(t *testing.T) {
	for _, test := range []struct {
		name   string
		result MCPConnectorToolResult
		err    error
	}{
		{name: "transport error", result: MCPConnectorToolResult{Text: []string{"not confirmed"}}, err: errors.New("transport failed")},
		{name: "upstream error", result: MCPConnectorToolResult{IsError: true, Text: []string{"rate limited"}}},
		{name: "empty result", result: MCPConnectorToolResult{}},
		{name: "blank result", result: MCPConnectorToolResult{Text: []string{"  \n  "}}},
		{name: "unsupported content", result: MCPConnectorToolResult{UnsupportedContent: true, Text: []string{"unsupported"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			delegate := &cloudDSHMCPClientStub{result: test.result, err: test.err}
			client := cloudDSHCachedBuiltinSearchClient{
				delegate: delegate, cache: newCloudDSHBuiltinSearchCache(),
				task: task.Task{ID: "task", OwnerID: "owner", Authorization: task.Authorization{Version: 1}},
			}
			arguments := json.RawMessage(`{"query":"AI news"}`)
			for range 2 {
				_, err := client.CallTool(context.Background(), "", nil, "searxng_web_search", arguments)
				if test.err != nil && err == nil {
					t.Fatal("expected the transport error to be returned")
				}
				if test.err == nil && err != nil {
					t.Fatal(err)
				}
			}
			if delegate.callCalls != 2 {
				t.Fatalf("non-cacheable result was reused: calls=%d, want 2", delegate.callCalls)
			}
		})
	}

	delegate := &cloudDSHMCPClientStub{result: MCPConnectorToolResult{Text: []string{"result"}}}
	client := cloudDSHCachedBuiltinSearchClient{
		delegate: delegate, cache: newCloudDSHBuiltinSearchCache(),
		task: task.Task{ID: "task", OwnerID: "owner", Authorization: task.Authorization{Version: 1}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	delegate.onCall = cancel
	arguments := json.RawMessage(`{"query":"AI news"}`)
	if _, err := client.CallTool(ctx, "", http.Header(nil), "searxng_web_search", arguments); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CallTool(context.Background(), "", nil, "searxng_web_search", arguments); err != nil {
		t.Fatal(err)
	}
	if delegate.callCalls != 2 {
		t.Fatalf("cancelled result was cached: calls=%d, want 2", delegate.callCalls)
	}
}
