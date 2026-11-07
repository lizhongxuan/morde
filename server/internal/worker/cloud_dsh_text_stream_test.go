package worker

import (
	"context"
	"errors"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

type cloudDSHTestStreamCapability struct {
	allowed bool
	err     error
}

func (c cloudDSHTestStreamCapability) TaskTextStreamingCapability(context.Context, task.Task) (bool, error) {
	return c.allowed, c.err
}

type cloudDSHTestStreamStore struct {
	deltas []app.CloudDSHTextDelta
	allow  bool
	err    error
	failAt int
	calls  int
}

func TestCloudDSHTextStreamSuppressesUnverifiedFileClaimWithoutAborting(t *testing.T) {
	store := &cloudDSHTestStreamStore{allow: true}
	stream := newCloudDSHTextStream(context.Background(), store, app.ModelTaskFence{}, "synthetic-call", nil)
	if stream == nil {
		t.Fatal("could not create stream")
	}
	for _, text := range []string{"我已经", "将文件保存到工作区", "，可以下载。"} {
		if err := stream.append(text); err != nil {
			t.Fatalf("stream guard aborted the model call: %v", err)
		}
	}
	if len(store.deltas) != 1 || store.deltas[0].Text != "我已经" {
		t.Fatalf("unverified file claim reached the stream: %+v", store.deltas)
	}
	if !stream.hasVisibleText() {
		t.Fatal("already visible prefix was lost")
	}
	if _, _, ok := stream.matchingResult("我已经将文件保存到工作区，可以下载。"); !ok {
		t.Fatal("partial stream identity was lost before completion checks")
	}
}

func (s *cloudDSHTestStreamStore) AppendCloudDSHTextDelta(_ context.Context, delta app.CloudDSHTextDelta) (bool, error) {
	s.calls++
	if s.failAt > 0 && s.calls == s.failAt {
		return false, errors.New("synthetic stream write failure")
	}
	if s.err != nil || !s.allow {
		return false, s.err
	}
	s.deltas = append(s.deltas, delta)
	return true, nil
}

func TestCloudDSHTextStreamPreservesPartialOnWriteFailureAndRejectsMismatch(t *testing.T) {
	for _, test := range []struct {
		name, result string
		failAt       int
		wantMismatch bool
	}{
		{"write failure retains prefix", "Hello world", 2, false},
		{"provider changed visible prefix", "Different result", 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor, _, instances, _, models, value := cloudDSHFixture(t)
			executor.options.ToolCapabilities = cloudDSHTestToolCapability{allowed: false}
			executor.options.StreamCapability = cloudDSHTestStreamCapability{allowed: true}
			streamStore := &cloudDSHTestStreamStore{allow: true, failAt: test.failAt}
			executor.options.TextStreams = streamStore
			tools := &cloudDSHTestTools{}
			executor.options.Tools = tools
			completer := &cloudDSHTestCompleter{instances: instances}
			executor.options.Completion = completer
			models.complete = func(_ context.Context, call app.ModelCall) (app.ModelResponse, error) {
				if call.Request.OnTextDelta == nil {
					return app.ModelResponse{}, errors.New("stream callback missing")
				}
				_ = call.Request.OnTextDelta("Hello ")
				_ = call.Request.OnTextDelta("world")
				return app.ModelResponse{Text: test.result, StopReason: "stop", Usage: app.ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}}, nil
			}
			err := executor.ExecuteTask(context.Background(), value)
			if test.wantMismatch {
				var category cloudDSHModelFailureCategory
				if !errors.As(err, &category) || category.ModelFailureCategory() != "stream_result_mismatch" ||
					completer.calls != 0 || tools.pauseRecoveryCall != 1 || len(streamStore.deltas) != 2 {
					t.Fatalf("mismatched stream was not interrupted: err=%v completions=%d pauses=%d deltas=%d", err, completer.calls, tools.pauseRecoveryCall, len(streamStore.deltas))
				}
				return
			}
			if err != nil || completer.calls != 1 || len(streamStore.deltas) != 1 ||
				completer.completion.StreamMessageID != streamStore.deltas[0].MessageID ||
				completer.completion.StreamTurnID != streamStore.deltas[0].TurnID {
				t.Fatalf("stream failure did not preserve its prefix: err=%v completion=%+v deltas=%+v", err, completer.completion, streamStore.deltas)
			}
		})
	}
}

func TestCloudDSHTextStreamIsOptionalAndReusesItsIdentity(t *testing.T) {
	for _, test := range []struct {
		name                         string
		streamCap, tools, allow      bool
		capErr, storeErr             bool
		wantCallback, wantCompletion bool
	}{
		{"verified text stream", true, false, true, false, false, true, true},
		{"unknown capability", false, false, true, false, false, false, false},
		{"capability lookup failed", true, false, true, true, false, false, false},
		{"tool capable model", true, true, true, false, false, false, false},
		{"competing task", true, false, false, false, false, true, false},
		{"optional stream store unavailable", true, false, true, false, true, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor, _, instances, _, models, value := cloudDSHFixture(t)
			executor.options.ToolCapabilities = cloudDSHTestToolCapability{allowed: test.tools}
			capability := cloudDSHTestStreamCapability{allowed: test.streamCap}
			if test.capErr {
				capability.err = errors.New("capability unavailable")
			}
			executor.options.StreamCapability = capability
			streamStore := &cloudDSHTestStreamStore{allow: test.allow}
			if test.storeErr {
				streamStore.err = errors.New("stream store unavailable")
			}
			executor.options.TextStreams = streamStore
			completer := &cloudDSHTestCompleter{instances: instances}
			executor.options.Completion = completer
			models.complete = func(_ context.Context, call app.ModelCall) (app.ModelResponse, error) {
				if (call.Request.OnTextDelta != nil) != test.wantCallback {
					t.Errorf("callback present=%t, want %t", call.Request.OnTextDelta != nil, test.wantCallback)
				}
				if call.Request.OnTextDelta != nil {
					if err := call.Request.OnTextDelta("Hello "); err != nil {
						return app.ModelResponse{}, err
					}
					if err := call.Request.OnTextDelta("world"); err != nil {
						return app.ModelResponse{}, err
					}
				}
				return app.ModelResponse{Text: "Hello world", StopReason: "stop", Usage: app.ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}}, nil
			}
			if err := executor.ExecuteTask(context.Background(), value); err != nil {
				t.Fatalf("optional stream broke ordinary completion: %v", err)
			}
			if completer.calls != 1 {
				t.Fatalf("completed %d times", completer.calls)
			}
			if test.wantCompletion {
				if len(streamStore.deltas) != 2 || completer.completion.StreamMessageID != streamStore.deltas[0].MessageID ||
					completer.completion.StreamTurnID != streamStore.deltas[0].TurnID ||
					streamStore.deltas[0].MessageID != streamStore.deltas[1].MessageID ||
					streamStore.deltas[0].TurnID != streamStore.deltas[1].TurnID ||
					streamStore.deltas[0].CallID != models.calls[0].CallID {
					t.Fatalf("stream identities were not preserved: deltas=%+v completion=%+v", streamStore.deltas, completer.completion)
				}
			} else if completer.completion.StreamMessageID != "" || completer.completion.StreamTurnID != "" {
				t.Fatal("unavailable stream was reported as visible")
			}
		})
	}
}
