package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func TestAgentFileWriteEvidenceRequiresSuccessfulMatchingCallOutputPair(t *testing.T) {
	content := "hello, 世界"
	digest := sha256.Sum256([]byte(content))
	output := fmt.Sprintf(`{"path":"notes/today.txt","bytes_written":%d,"sha256":"%x"}`, len([]byte(content)), digest[:])
	wrongDigest := sha256.Sum256([]byte("different content"))
	wrongOutput := fmt.Sprintf(`{"path":"notes/today.txt","bytes_written":%d,"sha256":"%x"}`, len([]byte(content)), wrongDigest[:])
	inputs := []ModelInput{
		{Type: "function_call", CallID: "write-1", Name: runtimeToolFileWrite, Arguments: `{"workspace_id":"w1234567","path":"notes/today.txt","content":"hello, 世界"}`},
		{Type: "function_call_output", CallID: "write-1", Output: output},
	}
	evidence, err := agentFileWriteEvidence(inputs, nil)
	if err != nil || len(evidence) != 1 {
		t.Fatalf("agentFileWriteEvidence() = %#v, %v; want one verified candidate", evidence, err)
	}
	if evidence[0].Path != "notes/today.txt" || evidence[0].Size != int64(len([]byte(content))) || evidence[0].SHA256 != fmt.Sprintf("%x", digest[:]) ||
		evidence[0].WorkspaceID != "w1234567" || evidence[0].OperationKey != agentOperationKey("write-1") || len(evidence[0].RequestHash) != sha256.Size {
		t.Fatalf("evidence = %#v; want path, UTF-8 byte size, and SHA-256 from the file_write arguments", evidence[0])
	}

	cases := []struct {
		name   string
		inputs []ModelInput
	}{
		{
			name: "missing output",
			inputs: []ModelInput{{Type: "function_call", CallID: "write-1", Name: runtimeToolFileWrite,
				Arguments: `{"workspace_id":"w1234567","path":"notes/today.txt","content":"hello, 世界"}`}},
		},
		{
			name: "mismatched byte count",
			inputs: []ModelInput{
				{Type: "function_call", CallID: "write-1", Name: runtimeToolFileWrite,
					Arguments: `{"workspace_id":"w1234567","path":"notes/today.txt","content":"hello, 世界"}`},
				{Type: "function_call_output", CallID: "write-1", Output: `{"path":"notes/today.txt","bytes_written":1}`},
			},
		},
		{
			name: "mismatched result digest",
			inputs: []ModelInput{
				{Type: "function_call", CallID: "write-1", Name: runtimeToolFileWrite,
					Arguments: `{"workspace_id":"w1234567","path":"notes/today.txt","content":"hello, 世界"}`},
				{Type: "function_call_output", CallID: "write-1", Output: wrongOutput},
			},
		},
		{
			name: "mismatched path",
			inputs: []ModelInput{
				{Type: "function_call", CallID: "write-1", Name: runtimeToolFileWrite,
					Arguments: `{"workspace_id":"w1234567","path":"notes/today.txt","content":"hello, 世界"}`},
				{Type: "function_call_output", CallID: "write-1", Output: `{"path":"other.txt","bytes_written":13}`},
			},
		},
		{
			name: "failed tool output",
			inputs: []ModelInput{
				{Type: "function_call", CallID: "write-1", Name: runtimeToolFileWrite,
					Arguments: `{"workspace_id":"w1234567","path":"notes/today.txt","content":"hello, 世界"}`},
				{Type: "function_call_output", CallID: "write-1", Output: `{"error":"runtime_operation_failed"}`},
			},
		},
		{
			name: "non-file-write call",
			inputs: []ModelInput{
				{Type: "function_call", CallID: "write-1", Name: runtimeToolFileRead,
					Arguments: `{"workspace_id":"w1234567","path":"notes/today.txt"}`},
				{Type: "function_call_output", CallID: "write-1", Output: output},
			},
		},
		{
			name: "duplicate call id",
			inputs: []ModelInput{
				{Type: "function_call", CallID: "write-1", Name: runtimeToolFileWrite,
					Arguments: `{"workspace_id":"w1234567","path":"notes/today.txt","content":"hello, 世界"}`},
				{Type: "function_call", CallID: "write-1", Name: runtimeToolFileWrite,
					Arguments: `{"workspace_id":"w1234567","path":"notes/today.txt","content":"hello, 世界"}`},
				{Type: "function_call_output", CallID: "write-1", Output: output},
			},
		},
		{
			name: "output precedes call",
			inputs: []ModelInput{
				{Type: "function_call_output", CallID: "write-1", Output: output},
				{Type: "function_call", CallID: "write-1", Name: runtimeToolFileWrite,
					Arguments: `{"workspace_id":"w1234567","path":"notes/today.txt","content":"hello, 世界"}`},
			},
		},
		{
			name: "unsafe path",
			inputs: []ModelInput{
				{Type: "function_call", CallID: "write-1", Name: runtimeToolFileWrite,
					Arguments: `{"workspace_id":"w1234567","path":"../escape.txt","content":"hello, 世界"}`},
				{Type: "function_call_output", CallID: "write-1", Output: `{"path":"../escape.txt","bytes_written":13}`},
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := agentFileWriteEvidence(test.inputs, nil)
			if err != nil || len(got) != 0 {
				t.Fatalf("agentFileWriteEvidence() = %#v, %v; want no evidence", got, err)
			}
		})
	}
}

func TestAgentFileWriteEvidenceUsesLatestSuccessfulWritePerPath(t *testing.T) {
	inputs := []ModelInput{
		{Type: "function_call", CallID: "write-1", Name: runtimeToolFileWrite, Arguments: `{"workspace_id":"w1234567","path":"same.txt","content":"old"}`},
		{Type: "function_call_output", CallID: "write-1", Output: `{"path":"same.txt","bytes_written":3}`},
		{Type: "function_call", CallID: "write-2", Name: runtimeToolFileWrite, Arguments: `{"workspace_id":"w1234567","path":"same.txt","content":"new content"}`},
		{Type: "function_call_output", CallID: "write-2", Output: `{"path":"same.txt","bytes_written":11}`},
	}
	evidence, err := agentFileWriteEvidence(inputs, nil)
	if err != nil || len(evidence) != 1 || evidence[0].Size != 11 {
		t.Fatalf("agentFileWriteEvidence() = %#v, %v; want only the latest successful write", evidence, err)
	}
	digest := sha256.Sum256([]byte("new content"))
	if evidence[0].SHA256 != fmt.Sprintf("%x", digest[:]) {
		t.Fatalf("latest evidence SHA-256 = %q; want %x", evidence[0].SHA256, digest)
	}
}

func TestAgentPassesOnlySuccessfulFileWriteEvidenceToVerifier(t *testing.T) {
	content := "hello"
	fixture := newAgentFixture(t,
		ModelResponse{
			ToolCalls: []ModelToolCall{{CallID: "call-file-write", Name: runtimeToolFileWrite,
				Arguments: `{"workspace_id":"w1234567","path":"report.txt","content":"hello"}`}},
			Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2},
		},
		ModelResponse{Text: "The report is complete.", Usage: ModelUsage{Known: true, InputTokens: 5, OutputTokens: 2}},
	)
	fixture.tools.definitions = append(fixture.tools.definitions, AgentToolDefinition{
		ModelTool: ModelTool{
			Name: runtimeToolFileWrite, Description: "Write a Workspace file.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"workspace_id":{"type":"string"},"path":{"type":"string"},"content":{"type":"string"}},"required":["workspace_id","path","content"],"additionalProperties":false}`),
		},
		RequiredScope: identity.ScopeComputerOperate,
	})
	fixture.tools.result = AgentToolResult{Status: AgentToolSucceeded, Output: `{"path":"report.txt","bytes_written":5}`}
	fixture.tools.verification = AgentVerificationResult{Status: AgentVerified, Summary: "verified"}

	result, err := fixture.agent.Run(context.Background(), fixture.claimed)
	if err != nil || result.Status != task.StatusCompleted {
		t.Fatalf("Run() = %+v, %v; want completed through the fake independent verifier", result, err)
	}
	verifyRequest := fixture.tools.lastVerifyReq
	digest := sha256.Sum256([]byte(content))
	if !domain.ValidStepID(verifyRequest.StepID) || len(verifyRequest.Evidence) != 1 ||
		verifyRequest.Evidence[0].Path != "report.txt" || verifyRequest.Evidence[0].Size != 5 ||
		verifyRequest.Evidence[0].SHA256 != fmt.Sprintf("%x", digest[:]) {
		t.Fatalf("verification request evidence = %#v (step %q); want the successful file_write path/bytes/hash", verifyRequest.Evidence, verifyRequest.StepID)
	}
}
