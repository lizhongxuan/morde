package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

const mcpImageWorkerCallID = "morde-mcp-image-tool-call-001"
const mcpImageWorkerArguments = `{"arguments":{},"connector_id":"c1234567","tool_name":"chart"}`

func TestDSHMCPImageResultReachesNextModelRoundWithinTaskFence(t *testing.T) {
	executor, _, _, _, models, value := cloudDSHFixture(t)
	imageData := cloudDSHTestPNG(t)
	tool := app.AgentToolDefinition{ModelTool: app.ModelTool{
		Name: "mcp_call", Description: "Read a chart from the selected MCP connector.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"connector_id":{"type":"string"},"tool_name":{"type":"string"},"arguments":{"type":"object"}},"required":["connector_id","tool_name","arguments"],"additionalProperties":false}`),
	}}
	broker := &mcpImageWorkerBrokerStub{taskID: value.ID, ownerID: value.OwnerID, initiatorID: value.InitiatorID,
		tool: tool, image: imageData}
	executor.options.Tools = broker
	completion := &desktopChainCompleterStub{done: make(chan struct{}, 1)}
	executor.options.Completion = completion
	model := &mcpImageWorkerSyntheticModel{taskID: value.ID, image: imageData, tool: tool}
	models.complete = model.Complete

	workerID := "morde-mcp-image-worker"
	value.Lease.Owner = workerID
	claimed := false
	leaseStore := &taskLeaseStoreStub{
		claim: func(_ context.Context, gotWorkerID string, _ time.Duration) (task.Task, bool, error) {
			if gotWorkerID != workerID {
				return task.Task{}, false, errors.New("unexpected worker identity")
			}
			if claimed {
				return task.Task{}, false, nil
			}
			claimed = true
			return value, true, nil
		},
		renew: func(_ context.Context, taskID, gotWorkerID string, version int64, lease time.Duration) (task.Task, error) {
			if taskID != value.ID || gotWorkerID != workerID || version != value.Lease.Version {
				return task.Task{}, errors.New("unexpected Task lease renewal")
			}
			expiresAt := time.Now().Add(lease)
			value.Lease.ExpiresAt = &expiresAt
			return value, nil
		},
	}
	executionResult := make(chan error, 1)
	capturingExecutor := taskExecutorFunc(func(ctx context.Context, claimed task.Task) error {
		executionErr := executor.ExecuteTask(ctx, claimed)
		executionResult <- executionErr
		return executionErr
	})
	backgroundWorker, err := NewTaskWorker(TaskWorkerOptions{
		Store: leaseStore, Executor: capturingExecutor, WorkerID: workerID, LeaseDuration: time.Minute,
		PollInterval: time.Millisecond, MaxConcurrentTasks: 1,
	})
	if err != nil {
		t.Fatalf("create local TaskWorker: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	workerDone := make(chan error, 1)
	go func() { workerDone <- backgroundWorker.Run(ctx) }()
	select {
	case <-completion.done:
	case executionErr := <-executionResult:
		t.Fatalf("MCP image Task execution failed before completion: %v; model calls=%d dsh calls=%d tool calls=%d last tool=%#v",
			executionErr, len(models.calls), model.calls, broker.calls, broker.received)
	case <-ctx.Done():
		cancel()
		select {
		case workerErr := <-workerDone:
			t.Fatalf("MCP image Task did not complete: %v; worker=%v model calls=%d dsh calls=%d tool calls=%d last tool=%#v request=%s",
				ctx.Err(), workerErr, len(models.calls), model.calls, broker.calls, broker.received, firstMCPImageRequest(models.calls))
		case <-time.After(2 * time.Second):
			t.Fatalf("MCP image Task did not complete: %v; worker still running model calls=%d dsh calls=%d tool calls=%d last tool=%#v request=%s",
				ctx.Err(), len(models.calls), model.calls, broker.calls, broker.received, firstMCPImageRequest(models.calls))
		}
	}
	cancel()
	select {
	case err := <-workerDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("TaskWorker stopped with %v; want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TaskWorker did not stop after the synthetic Task completed")
	}

	if completion.task.ID != value.ID || completion.completion.Result != "The MCP chart was reviewed." ||
		broker.calls != 1 || !model.imageSeen || len(models.calls) != 2 {
		t.Fatalf("MCP image chain completion=%#v tool calls=%d model=%#v; want exact image passed to the next model round", completion.completion, broker.calls, model)
	}
}

func firstMCPImageRequest(calls []app.ModelCall) string {
	if len(calls) == 0 {
		return ""
	}
	return string(calls[0].RawRequest)
}

type mcpImageWorkerBrokerStub struct {
	taskID, ownerID, initiatorID string
	tool                         app.AgentToolDefinition
	image                        []byte
	calls                        int
	received                     app.ModelToolCall
}

func (b *mcpImageWorkerBrokerStub) Definitions(context.Context, task.Task) ([]app.AgentToolDefinition, error) {
	return []app.AgentToolDefinition{b.tool}, nil
}

func (b *mcpImageWorkerBrokerStub) Execute(_ context.Context, claimed task.Task, call app.ModelToolCall) (app.AgentToolResult, error) {
	b.received = call
	if claimed.ID != b.taskID || claimed.OwnerID != b.ownerID || claimed.InitiatorID != b.initiatorID ||
		call.Name != b.tool.Name || call.CallID != mcpImageWorkerCallID || call.Arguments != mcpImageWorkerArguments {
		return app.AgentToolResult{}, fmt.Errorf("unexpected MCP image tool call: task=%#v call=%#v", claimed, call)
	}
	b.calls++
	return app.AgentToolResult{Status: app.AgentToolSucceeded, Output: `{"text":"verified chart attached"}`,
		Images: []app.ModelImage{{MediaType: "image/png", Data: append([]byte(nil), b.image...)}}}, nil
}

type mcpImageWorkerSyntheticModel struct {
	taskID    string
	image     []byte
	tool      app.AgentToolDefinition
	calls     int
	imageSeen bool
}

func (m *mcpImageWorkerSyntheticModel) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	if call.Task == nil || call.Task.TaskID != m.taskID {
		return app.ModelResponse{}, fmt.Errorf("synthetic model received unexpected Task fence: %#v", call.Task)
	}
	m.calls++
	var request struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"toolCallId"`
			Content    []struct {
				Type       string          `json:"type"`
				Text       string          `json:"text"`
				Attachment json.RawMessage `json:"attachment"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(call.RawRequest, &request); err != nil {
		return app.ModelResponse{}, fmt.Errorf("decode DSH MCP image request: %w", err)
	}
	usage := app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}
	if m.calls == 1 {
		for _, candidate := range request.Tools {
			if candidate.Name == m.tool.Name {
				return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: mcpImageWorkerCallID, Name: m.tool.Name, Arguments: mcpImageWorkerArguments}},
					StopReason: "tool_calls", Usage: usage}, nil
			}
		}
		return app.ModelResponse{}, errors.New("MCP image tool was not offered to DSH")
	}
	if m.calls != 2 || call.ImageResolver == nil {
		return app.ModelResponse{}, fmt.Errorf("unexpected DSH model call #%d or missing image resolver", m.calls)
	}
	for _, message := range request.Messages {
		if message.Role != "tool" || message.ToolCallID != mcpImageWorkerCallID {
			continue
		}
		for _, part := range message.Content {
			if part.Type != "image" {
				continue
			}
			resolved, err := call.ImageResolver.ResolveTaskModelImage(ctx, *call.Task, part.Attachment)
			if err != nil {
				return app.ModelResponse{}, fmt.Errorf("resolve task-bound MCP image: %w", err)
			}
			if resolved.MediaType != "image/png" || !bytes.Equal(resolved.Data, m.image) {
				return app.ModelResponse{}, errors.New("MCP image did not resolve to the exact task-bound bytes")
			}
			m.imageSeen = true
		}
	}
	if !m.imageSeen {
		return app.ModelResponse{}, errors.New("MCP image attachment was missing from the next model round")
	}
	return app.ModelResponse{Text: "The MCP chart was reviewed.", StopReason: "stop", Usage: usage}, nil
}
