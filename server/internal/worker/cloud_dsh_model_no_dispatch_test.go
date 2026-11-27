package worker

import (
	"errors"
	"reflect"
	"testing"

	"morde/server/internal/app"
)

type cloudDSHNoDispatchEvidenceTestError struct {
	evidence app.TaskModelNoDispatchEvidence
}

func (cloudDSHNoDispatchEvidenceTestError) Error() string { return "model call did not dispatch" }

func (e cloudDSHNoDispatchEvidenceTestError) TaskModelNoDispatchEvidence() app.TaskModelNoDispatchEvidence {
	return e.evidence
}

func (e cloudDSHNoDispatchEvidenceTestError) TaskModelRecoveryFailureEvidence() app.TaskModelRecoveryFailureEvidence {
	return app.TaskModelRecoveryFailureEvidence{Kind: app.TaskModelRecoveryFailureNoDispatch, TaskFence: e.evidence.TaskFence,
		CallID: e.evidence.CallID, ModelID: e.evidence.ModelID, RequestDigest: append([]byte(nil), e.evidence.RequestDigest...),
		UsageKnown: e.evidence.UsageKnown, InputTokens: e.evidence.InputTokens, OutputTokens: e.evidence.OutputTokens}
}

type cloudDSHNoDispatchEvidenceResult interface {
	TaskModelNoDispatchEvidence() (app.TaskModelNoDispatchEvidence, bool)
}

type cloudDSHRecoveryFailureEvidenceTestError struct {
	evidence app.TaskModelRecoveryFailureEvidence
}

func (cloudDSHRecoveryFailureEvidenceTestError) Error() string {
	return "model request failed with a confirmed provider rejection"
}

func (e cloudDSHRecoveryFailureEvidenceTestError) TaskModelRecoveryFailureEvidence() app.TaskModelRecoveryFailureEvidence {
	return e.evidence
}

func (e cloudDSHRecoveryFailureEvidenceTestError) ModelFailureCategory() string {
	return string(e.evidence.ProviderErrorKind)
}

func TestCloudDSHModelResultErrorForCallKeepsOnlyMatchingNoDispatchEvidence(t *testing.T) {
	fence := app.ModelTaskFence{TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", LeaseOwner: "worker-1",
		LeaseVersion: 4, AgentInstanceID: "agent-1", AgentGeneration: 2, AgentSessionID: "session-1",
		AgentRootSessionID: "session-1", AuthorizationVersion: 3, ThroughSequence: 7, EngineVersion: "dsh-1", ProfileVersion: "v1"}
	callID, err := app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	call := app.ModelCall{Source: app.ModelCallTask, Task: &fence, ModelID: "p1234567", CallID: callID}
	validEvidence := app.TaskModelNoDispatchEvidence{TaskFence: fence, CallID: callID, ModelID: "p1234567",
		RequestDigest: make([]byte, 32), UsageKnown: true}

	got := cloudDSHModelResultErrorForCall(cloudDSHNoDispatchEvidenceTestError{evidence: validEvidence}, call)
	var result cloudDSHNoDispatchEvidenceResult
	if !errors.As(got, &result) {
		t.Fatalf("result does not expose validated no-dispatch evidence: %T %v", got, got)
	}
	evidence, ok := result.TaskModelNoDispatchEvidence()
	if !ok || !reflect.DeepEqual(evidence, validEvidence) || !errors.Is(got, errCloudDSHModelResultUnavailable) {
		t.Fatalf("validated evidence was not propagated: evidence=%+v ok=%t err=%v", evidence, ok, got)
	}

	badFence := fence
	badFence.LeaseVersion++
	badSequence := fence
	badSequence.ThroughSequence++
	for _, test := range []struct {
		name string
		err  error
		call app.ModelCall
	}{
		{name: "provider error", err: errors.New("provider outcome unknown"), call: call},
		{name: "abandonment failure", err: errors.Join(errors.New("dispatch failed"), errors.New("release failed")), call: call},
		{name: "different fence", err: cloudDSHNoDispatchEvidenceTestError{evidence: app.TaskModelNoDispatchEvidence{
			TaskFence: badFence, CallID: callID, UsageKnown: true}}, call: call},
		{name: "different sequence", err: cloudDSHNoDispatchEvidenceTestError{evidence: app.TaskModelNoDispatchEvidence{
			TaskFence: badSequence, CallID: callID, UsageKnown: true}}, call: call},
		{name: "different call id", err: cloudDSHNoDispatchEvidenceTestError{evidence: app.TaskModelNoDispatchEvidence{
			TaskFence: fence, CallID: "other-call", UsageKnown: true}}, call: call},
		{name: "usage unknown", err: cloudDSHNoDispatchEvidenceTestError{evidence: app.TaskModelNoDispatchEvidence{
			TaskFence: fence, CallID: callID}}, call: call},
		{name: "nonzero usage", err: cloudDSHNoDispatchEvidenceTestError{evidence: app.TaskModelNoDispatchEvidence{
			TaskFence: fence, CallID: callID, UsageKnown: true, OutputTokens: 1}}, call: call},
		{name: "non-task call", err: cloudDSHNoDispatchEvidenceTestError{evidence: validEvidence},
			call: app.ModelCall{Source: app.ModelCallChat, CallID: callID}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := cloudDSHModelResultErrorForCall(test.err, test.call)
			var source cloudDSHNoDispatchEvidenceResult
			if errors.As(got, &source) {
				if _, ok := source.TaskModelNoDispatchEvidence(); ok {
					t.Fatalf("unverified no-dispatch evidence was propagated: %+v", test.name)
				}
			}
		})
	}
}

func TestCloudDSHModelResultErrorForCallKeepsOnlyMatchingProviderRejectEvidence(t *testing.T) {
	fence := app.ModelTaskFence{TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", LeaseOwner: "worker-1",
		LeaseVersion: 4, AgentInstanceID: "agent-1", AgentGeneration: 2, AgentSessionID: "session-1",
		AgentRootSessionID: "session-1", AuthorizationVersion: 3, TaskRevision: 7, ThroughSequence: 7,
		EngineVersion: "dsh-1", ProfileVersion: "v1"}
	callID, err := app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	call := app.ModelCall{Source: app.ModelCallTask, Task: &fence, ModelID: "p1234567", CallID: callID}
	evidence := app.TaskModelRecoveryFailureEvidence{Kind: app.TaskModelRecoveryFailureProviderReject, TaskFence: fence,
		CallID: callID, ModelID: call.ModelID, RequestDigest: make([]byte, 32), UsageKnown: true,
		ProviderErrorKind: app.ModelErrorRateLimited}
	checkpoint := cloudDSHReadOnlyRecoveryCheckpoint{ThroughSequence: 7, Digest: make([]byte, 32), ToolsetDigest: make([]byte, 32)}
	wrapped := cloudDSHModelResultErrorForCall(cloudDSHRecoveryFailureEvidenceTestError{evidence: evidence}, call, checkpoint)
	var failure cloudDSHModelResultError
	if !errors.As(wrapped, &failure) || failure.recoveryFailure == nil || failure.recoveryFailureErr == nil ||
		failure.recoveryCheckpoint == nil || failure.recoveryFailure.Kind != app.TaskModelRecoveryFailureProviderReject ||
		failure.recoveryFailure.ProviderErrorKind != app.ModelErrorRateLimited {
		t.Fatalf("confirmed provider rejection was not bound to exact DSH frame/checkpoint: %+v", wrapped)
	}
	for _, changed := range []app.TaskModelRecoveryFailureEvidence{
		{Kind: app.TaskModelRecoveryFailureProviderReject, TaskFence: fence, CallID: callID, ModelID: call.ModelID,
			RequestDigest: make([]byte, 32), ProviderErrorKind: app.ModelErrorTimeout},
		{Kind: app.TaskModelRecoveryFailureProviderReject, TaskFence: fence, CallID: "mdl_other_123", ModelID: call.ModelID,
			RequestDigest: make([]byte, 32), UsageKnown: true, ProviderErrorKind: app.ModelErrorRateLimited},
		{Kind: app.TaskModelRecoveryFailureProviderReject, TaskFence: fence, CallID: callID, ModelID: call.ModelID,
			RequestDigest: make([]byte, 31), UsageKnown: true, ProviderErrorKind: app.ModelErrorRateLimited},
	} {
		got := cloudDSHModelResultErrorForCall(cloudDSHRecoveryFailureEvidenceTestError{evidence: changed}, call, checkpoint)
		var result cloudDSHModelResultError
		if errors.As(got, &result) && result.recoveryFailure != nil {
			t.Fatalf("invalid provider retry evidence was preserved: %+v", changed)
		}
	}
}
