package app

import (
	"context"
	"crypto/sha256"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

type summaryCapacityStoreTest struct{ *modelGatewayStoreTest }

func (*summaryCapacityStoreTest) RecoverConversationSummaryModelOutput(context.Context, ModelCallReservation) (ConversationSummaryModelOutput, bool, error) {
	return ConversationSummaryModelOutput{}, false, nil
}

func TestConversationSummaryGatewayUsesMinimumWindowWhenCapacityIsUnknown(t *testing.T) {
	for _, test := range []struct {
		name      string
		capacity  *int
		wantError domain.ErrorCode
		wantCalls int
	}{
		{name: "unknown", wantCalls: 1},
		{name: "capacity increased", capacity: intPointer(8192), wantCalls: 1},
		{name: "capacity shrank below fence", capacity: intPointer(2048), wantError: domain.CodeConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			claim, _ := routeGatewayFixture()
			claim.Authorization = task.Authorization{Kind: "session", TokenID: "t1234567", Version: 1}
			identity, err := ConversationSummaryScopeForClaim(claim)
			if err != nil {
				t.Fatal(err)
			}
			operationID, err := domain.NewOperationID()
			if err != nil {
				t.Fatal(err)
			}
			inputDigest := sha256.Sum256([]byte("authorized summary input"))
			fence := ConversationSummaryModelFence{Claim: claim, Identity: identity, OperationID: operationID,
				InputSHA256: inputDigest[:], LeaseOwner: claim.LeaseOwner, LeaseVersion: claim.LeaseVersion,
				SessionID: "summary_session_1", ThroughSequence: 1, ContextWindowTokens: 4096}
			store := &summaryCapacityStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}
			client := &modelGatewayClientTest{response: ModelResponse{Text: "short summary", StopReason: "stop",
				Usage: ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}}}
			service, err := NewModelGatewayService(store, &modelGatewayResolverTest{contextWindow: test.capacity}, client)
			if err != nil {
				t.Fatal(err)
			}
			setTestModelResponseSealer(t, service)

			response, err := service.CompleteConversationSummaryModel(context.Background(), fence, "p1234567", ModelRequest{
				Instructions:    "Summarize authorized history.",
				Input:           []ModelInput{{Type: "message", Role: "user", Text: "Summarize this conversation."}},
				MaxOutputTokens: 64,
			})
			window := 0
			if store.reservation.Summary != nil {
				window = store.reservation.Summary.ContextWindowTokens
			}
			failed := false
			if test.wantError == "" {
				failed = err != nil || response.Text != "short summary" || window != 4096
			} else {
				failed = err == nil || domain.CodeOf(err) != test.wantError
			}
			if failed || client.calls != test.wantCalls {
				t.Fatalf("summary response=%+v calls=%d window=%d error=%v", response, client.calls, window, err)
			}
		})
	}
}

func intPointer(value int) *int { return &value }
