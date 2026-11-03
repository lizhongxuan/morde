package app

import (
	"crypto/sha256"
	"fmt"
	"testing"
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
	evidence, err := agentFileWriteEvidence(inputs)
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
			got, err := agentFileWriteEvidence(test.inputs)
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
	evidence, err := agentFileWriteEvidence(inputs)
	if err != nil || len(evidence) != 1 || evidence[0].Size != 11 {
		t.Fatalf("agentFileWriteEvidence() = %#v, %v; want only the latest successful write", evidence, err)
	}
	digest := sha256.Sum256([]byte("new content"))
	if evidence[0].SHA256 != fmt.Sprintf("%x", digest[:]) {
		t.Fatalf("latest evidence SHA-256 = %q; want %x", evidence[0].SHA256, digest)
	}
}
