package worker

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"morde/server/internal/app"
)

type h01SummaryTimeoutModels struct {
	*summaryRouteModelsStub
}

func (s *h01SummaryTimeoutModels) CompactConversationSummary(context.Context, CloudDSHConversationSummaryRequest) (CloudDSHConversationSummaryResult, error) {
	return CloudDSHConversationSummaryResult{}, context.DeadlineExceeded
}

func TestH01SummaryFailedPartialCompactionPreservesPriorSummary(t *testing.T) {
	for _, test := range []struct {
		name    string
		text    string
		timeout bool
	}{
		{name: "empty"},
		{name: "notice-only", text: conversationSummaryIncompleteNotice},
		{name: "timeout", timeout: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			worker, route, models, store, sealer, identity, _ := newSummaryRouteWorkerHarness(t, "", 16_384,
				CloudDSHConversationSummaryResult{Text: test.text, ModelID: "p1234567", SessionID: "summary-session", Through: 12})
			route.claim.Message.Sequence = 4
			priorText := "An earlier verified summary."
			priorDigest := sha256.Sum256([]byte("earlier history"))
			priorSource := app.ConversationSummarySource{Kind: app.ConversationSummarySourceMessage, ID: "m1234567", Sequence: 1, SHA256: priorDigest[:]}
			prior, err := app.SealConversationSummary(sealer, app.ConversationSummaryDocument{
				Identity: identity, Version: 1, OperationID: "o1234567", Text: priorText,
				Sources: []app.ConversationSummarySource{priorSource},
			})
			if err != nil {
				t.Fatal(err)
			}
			firstText, secondText := strings.Repeat("x", 8_000), strings.Repeat("y", 6_000)
			firstDigest, secondDigest := sha256.Sum256([]byte(firstText)), sha256.Sum256([]byte(secondText))
			worker.options.SummaryInputs = summaryInputPageStub{page: app.ConversationSummaryInputPage{
				Identity: identity, BoundarySequence: 3, Head: app.ConversationSummaryHead{Identity: identity, Version: 1, Digest: prior.SummarySHA256}, Summary: &prior,
				Sources: []app.ConversationSummaryPromptSource{
					{Source: app.ConversationSummarySource{Kind: app.ConversationSummarySourceMessage, ID: "m1234568", Sequence: 2, SHA256: firstDigest[:]}, Role: "user", Content: firstText},
					{Source: app.ConversationSummarySource{Kind: app.ConversationSummarySourceMessage, ID: "m1234569", Sequence: 3, SHA256: secondDigest[:]}, Role: "user", Content: secondText},
				},
			}}
			if test.timeout {
				worker.options.Models = &h01SummaryTimeoutModels{summaryRouteModelsStub: models}
			}
			committed, err := worker.compactConversationSummary(context.Background(), route.claim, "p1234567")
			if err == nil || committed || len(store.commit.Ciphertext) != 0 || len(store.releases) != 1 || store.releases[0].State != "failed" {
				t.Fatalf("failed compaction advanced the summary: committed=%t commit=%+v releases=%+v err=%v", committed, store.commit, store.releases, err)
			}
			if test.timeout && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("timeout error = %v", err)
			}
			opened, err := app.OpenConversationSummary(sealer, prior)
			if err != nil || opened.Text != priorText || opened.Version != 1 {
				t.Fatalf("prior summary changed: %+v err=%v", opened, err)
			}
		})
	}
}
