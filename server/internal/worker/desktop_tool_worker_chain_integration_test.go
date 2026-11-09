package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

const (
	desktopChainToolCallID = "morde-desktop-screenshot-call-001"
	desktopChainToolOutput = `{"mime_type":"image/jpeg","width":3,"height":2,"screen_width":3,"screen_height":2,"screenshot_token":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","display":"synthetic-local-desktop"}`
	desktopChainAnswer     = "I inspected the local desktop screenshot."
)

// TestDSHDesktopToolCompletesThroughWorkerAndRecordsOperationAndStep exercises
// a queued Task through local DSH, a synthetic model, and a desktop Broker
// substitute. No provider, Host, or external service is contacted.
func TestDSHDesktopToolCompletesThroughWorkerAndRecordsOperationAndStep(t *testing.T) {
	executor, _, _, _, models, value := cloudDSHFixture(t)
	tool := app.AgentToolDefinition{ModelTool: app.ModelTool{
		Name: "desktop_screenshot", Description: "Capture a bounded local desktop screenshot.",
		Parameters: json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`),
	}}
	screenshot := cloudDSHTestJPEG(t)
	broker := &desktopChainBrokerStub{
		definitions: []app.AgentToolDefinition{tool},
		result: app.AgentToolResult{Status: app.AgentToolSucceeded, Output: desktopChainToolOutput,
			Images: []app.ModelImage{{MediaType: "image/jpeg", Data: screenshot}}},
	}
	executor.options.Tools = broker
	completion := &desktopChainCompleterStub{done: make(chan struct{}, 1)}
	executor.options.Completion = completion
	models.responses = []app.ModelResponse{
		{ToolCalls: []app.ModelToolCall{{CallID: desktopChainToolCallID, Name: tool.Name, Arguments: `{}`}},
			StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 5, OutputTokens: 3}},
		{Text: desktopChainAnswer, StopReason: "stop", Usage: app.ModelUsage{Known: true, InputTokens: 9, OutputTokens: 7}},
	}

	workerID := "morde-desktop-tool-worker"
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
	backgroundWorker, err := NewTaskWorker(TaskWorkerOptions{
		Store: leaseStore, Executor: executor, WorkerID: workerID, LeaseDuration: time.Minute,
		PollInterval: time.Millisecond, MaxConcurrentTasks: 1,
	})
	if err != nil {
		t.Fatalf("create local TaskWorker: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	workerDone := make(chan error, 1)
	go func() { workerDone <- backgroundWorker.Run(ctx) }()

	select {
	case <-completion.done:
	case <-ctx.Done():
		t.Fatalf("Task did not complete through DSH: %v", ctx.Err())
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

	if len(models.calls) != 2 || !validCloudDSHModelRequest(models.calls[0].RawRequest, value.ModelID, []app.AgentToolDefinition{tool}) ||
		!validCloudDSHModelRequest(models.calls[1].RawRequest, value.ModelID, []app.AgentToolDefinition{tool}) {
		t.Fatalf("DSH model rounds=%d; want tool call then result-consuming reply", len(models.calls))
	}
	if len(broker.calls) != 1 || broker.calls[0].TaskID != value.ID || broker.calls[0].OwnerID != value.OwnerID ||
		broker.calls[0].CallID != desktopChainToolCallID || broker.calls[0].ToolName != tool.Name || broker.calls[0].Arguments != `{}` {
		t.Fatalf("local desktop Broker calls = %#v; want one fenced screenshot call", broker.calls)
	}
	if len(broker.operations) != 1 || broker.operations[0].TaskID != value.ID ||
		broker.operations[0].Key != app.AgentOperationKey(desktopChainToolCallID) ||
		broker.operations[0].Status != task.OperationSucceeded || broker.operations[0].Result != desktopChainToolOutput {
		t.Fatalf("local operation record = %#v; want the same successful Task-bound tool result", broker.operations)
	}
	if len(broker.steps) != 1 || broker.steps[0].TaskID != value.ID || broker.steps[0].Sequence != 1 ||
		broker.steps[0].Kind != "tool" || broker.steps[0].Status != task.StepSucceeded ||
		broker.steps[0].Title != tool.Name || broker.steps[0].Summary != desktopChainToolOutput {
		t.Fatalf("local work-chain steps = %#v; want one successful step matching the operation result", broker.steps)
	}
	if completion.completion.Result != desktopChainAnswer || completion.task.ID != value.ID ||
		completion.completion.AgentSessionID == "" || completion.completion.CheckpointThroughSequence < 0 {
		t.Fatalf("completed DSH result = %#v for Task %#v; want the final synthetic answer and checkpoint", completion.completion, completion.task)
	}

	var secondRequest struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"toolCallId"`
			Content    []struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				Attachment struct {
					MediaType  string `json:"mediaType"`
					Width      int    `json:"width"`
					Height     int    `json:"height"`
					DataBase64 string `json:"data_base64"`
				} `json:"attachment"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(models.calls[1].RawRequest, &secondRequest); err != nil {
		t.Fatalf("decode second DSH model request: %v", err)
	}
	wantImage := base64.StdEncoding.EncodeToString(screenshot)
	foundToolResult, foundScreenshot := false, false
	for _, message := range secondRequest.Messages {
		if message.Role != "tool" || message.ToolCallID != desktopChainToolCallID {
			continue
		}
		for _, block := range message.Content {
			if block.Type == "text" && block.Text == desktopChainToolOutput {
				foundToolResult = true
			}
			if block.Type == "image" && block.Attachment.MediaType == "image/jpeg" && block.Attachment.Width == 3 &&
				block.Attachment.Height == 2 && block.Attachment.DataBase64 == wantImage {
				foundScreenshot = true
			}
		}
	}
	if !foundToolResult || !foundScreenshot {
		t.Fatalf("second model request did not contain the exact Broker text and screenshot: text=%t image=%t raw=%s",
			foundToolResult, foundScreenshot, models.calls[1].RawRequest)
	}
}

type desktopChainBrokerStub struct {
	definitions []app.AgentToolDefinition
	result      app.AgentToolResult
	calls       []desktopChainBrokerCall
	operations  []task.Operation
	steps       []task.Step
}

type desktopChainBrokerCall struct {
	TaskID    string
	OwnerID   string
	CallID    string
	ToolName  string
	Arguments string
}

func (b *desktopChainBrokerStub) Definitions(context.Context, task.Task) ([]app.AgentToolDefinition, error) {
	return append([]app.AgentToolDefinition(nil), b.definitions...), nil
}

func (b *desktopChainBrokerStub) Execute(_ context.Context, claimed task.Task, call app.ModelToolCall) (app.AgentToolResult, error) {
	if call.Name != "desktop_screenshot" || call.CallID == "" {
		return app.AgentToolResult{}, errors.New("unexpected synthetic desktop tool call")
	}
	b.calls = append(b.calls, desktopChainBrokerCall{
		TaskID: claimed.ID, OwnerID: claimed.OwnerID, CallID: call.CallID, ToolName: call.Name, Arguments: call.Arguments,
	})
	b.operations = append(b.operations, task.Operation{
		ID: "o1234567", TaskID: claimed.ID, Key: app.AgentOperationKey(call.CallID),
		Status: task.OperationSucceeded, Result: b.result.Output,
	})
	b.steps = append(b.steps, task.Step{
		ID: "s1234567", TaskID: claimed.ID, Sequence: int64(len(b.steps) + 1), Kind: "tool",
		Status: task.StepSucceeded, Title: call.Name, Summary: b.result.Output, CreatedAt: time.Now(),
	})
	return b.result, nil
}

type desktopChainCompleterStub struct {
	done       chan struct{}
	task       task.Task
	completion app.CloudDSHTaskCompletion
}

func (c *desktopChainCompleterStub) Complete(_ context.Context, value task.Task, completion app.CloudDSHTaskCompletion) error {
	c.task, c.completion = value, completion
	select {
	case c.done <- struct{}{}:
	default:
	}
	return nil
}
