package app

import (
	"crypto/sha256"
	"strings"
	"testing"
)

func TestValidateConversationSummaryModelResponseNamesFirstFailure(t *testing.T) {
	cases := []struct {
		name       string
		response   ModelResponse
		wantReason string
	}{
		{"valid summary", ModelResponse{StopReason: "stop", Text: "The user prefers morning meetings."}, ""},
		{"length stop", ModelResponse{StopReason: "length", Text: "partial summary"}, ConversationSummaryFailureStopReason},
		{"tool calls", ModelResponse{StopReason: "stop", Text: "summary", ToolCalls: []ModelToolCall{{}}}, ConversationSummaryFailureToolCalls},
		{"blank text", ModelResponse{StopReason: "stop", Text: "  \n "}, ConversationSummaryFailureBlankText},
		{"invalid utf-8", ModelResponse{StopReason: "stop", Text: "bad\xffsummary"}, ConversationSummaryFailureInvalidUTF8},
		{"nul byte", ModelResponse{StopReason: "stop", Text: "a\x00b"}, ConversationSummaryFailureNULByte},
		{"too large", ModelResponse{StopReason: "stop", Text: strings.Repeat("x", MaxConversationSummaryBytes+1)}, ConversationSummaryFailureTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failure := ValidateConversationSummaryModelResponse(tc.response)
			if tc.wantReason == "" {
				if failure != nil {
					t.Fatalf("valid summary rejected: %v", failure)
				}
				return
			}
			if failure == nil {
				t.Fatal("invalid summary accepted")
			}
			if failure.Reason != tc.wantReason {
				t.Fatalf("failure reason = %q, want %q", failure.Reason, tc.wantReason)
			}
			if failure.TextBytes != len(tc.response.Text) {
				t.Fatalf("failure text bytes = %d, want %d", failure.TextBytes, len(tc.response.Text))
			}
			if !strings.Contains(failure.Error(), tc.wantReason) {
				t.Fatalf("failure error does not name the reason: %v", failure)
			}
		})
	}
}

func TestConversationSummaryFailureDoesNotExposeContent(t *testing.T) {
	const privateText = "private-summary-secret"
	for _, response := range []ModelResponse{
		{StopReason: "length", Text: privateText},
		{StopReason: privateText, Text: privateText},
		{StopReason: "stop", Text: privateText + "\x00"},
	} {
		failure := ValidateConversationSummaryModelResponse(response)
		if failure == nil || strings.Contains(failure.Error(), privateText) {
			t.Fatalf("summary failure exposed content: %v", failure)
		}
	}
}

func TestValidateConversationSummaryModelResponseChecksStopReasonFirst(t *testing.T) {
	failure := ValidateConversationSummaryModelResponse(ModelResponse{StopReason: "length", Text: "  "})
	if failure == nil || failure.Reason != ConversationSummaryFailureStopReason {
		t.Fatalf("stop reason must win over blank text: %+v", failure)
	}
	if failure.StopReason != "length" || failure.TextBytes != 2 {
		t.Fatalf("failure does not carry the stop evidence: %+v", failure)
	}
}

func TestSealConversationSummaryNamesTextFailure(t *testing.T) {
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	identity := ConversationSummaryIdentity{ServiceInstanceID: "00000000-0000-4000-8000-000000000001",
		OwnerID: "u1234567", ActorID: "u1234567", ConversationID: "c1234567",
		ScopeKind: ConversationSummaryPersonalScope, AuthorizationVersion: 1}
	callID, err := DSHConversationSummaryModelCallID(identity.ConversationID, "o1234567", "summary-session-1", 4)
	if err != nil {
		t.Fatal(err)
	}
	base := ConversationSummaryModelOutput{Identity: identity, OperationID: "o1234567", CallID: callID,
		ModelID: "p1234567", Usage: ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}}
	for name, tc := range map[string]struct {
		text string
		want string
	}{
		"blank text":    {"  ", ConversationSummaryFailureBlankText},
		"invalid utf-8": {"bad\xffsummary", ConversationSummaryFailureInvalidUTF8},
		"nul byte":      {"a\x00b", ConversationSummaryFailureNULByte},
		"too large":     {strings.Repeat("x", MaxConversationSummaryBytes+1), ConversationSummaryFailureTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			output := base
			output.Text = tc.text
			if _, _, err := SealConversationSummaryModelOutput(sealer, output); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("seal error = %v, want reason %q", err, tc.want)
			}
		})
	}
	valid := base
	valid.Text = "The user prefers morning meetings."
	if _, _, err := SealConversationSummaryModelOutput(sealer, valid); err != nil {
		t.Fatalf("valid model output rejected by seal: %v", err)
	}

	messageDigest := sha256.Sum256([]byte("original message"))
	document := ConversationSummaryDocument{Identity: identity, Version: 1, OperationID: "o1234567",
		Text: "The user is planning a trip.", Sources: []ConversationSummarySource{{
			Kind: ConversationSummarySourceMessage, ID: "m1234567", Version: 0, Sequence: 4, SHA256: messageDigest[:],
		}}}
	if _, err := SealConversationSummary(sealer, document); err != nil {
		t.Fatalf("valid summary document rejected by seal: %v", err)
	}
	document.Text = "  "
	if _, err := SealConversationSummary(sealer, document); err == nil || !strings.Contains(err.Error(), ConversationSummaryFailureBlankText) {
		t.Fatalf("seal error = %v, want reason %q", err, ConversationSummaryFailureBlankText)
	}
	document.Text = strings.Repeat("x", MaxConversationSummaryBytes+1)
	if _, err := SealConversationSummary(sealer, document); err == nil || !strings.Contains(err.Error(), ConversationSummaryFailureTooLarge) {
		t.Fatalf("seal error = %v, want reason %q", err, ConversationSummaryFailureTooLarge)
	}
}
