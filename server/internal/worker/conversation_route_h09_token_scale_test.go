package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"morde/server/internal/app"
)

const (
	h09SyntheticTokensPerSource = 1_000
	h09SyntheticSourceBytes     = 8 << 10
	h09SyntheticPreviousTokens  = 512
	h09SyntheticContextWindow   = 4_096
)

// This is synthetic estimator evidence only. The fixture assigns 1,000 tokens
// to each source and 512 tokens to any non-empty prior summary; it does not
// use a real model tokenizer, DSH runtime, provider, or PostgreSQL store.
// Estimate calls here are deterministic stubs: production collects a byte-capped
// candidate prefix before token selection, so this test bounds final compact
// prompts but does not claim that a real measure-only request fits the window.
func TestConversationRouteH09SyntheticTokenScaleCompaction(t *testing.T) {
	for _, corpus := range []struct {
		name       string
		cumulative int
	}{
		{name: "4K", cumulative: 4_000},
		{name: "8K", cumulative: 8_000},
		{name: "100K", cumulative: 100_000},
		{name: "200K", cumulative: 200_000},
	} {
		t.Run(corpus.name, func(t *testing.T) {
			h09RunSyntheticTokenCorpus(t, corpus.cumulative)
		})
	}
}

func h09RunSyntheticTokenCorpus(t *testing.T, cumulativeTokens int) {
	t.Helper()
	if cumulativeTokens%h09SyntheticTokensPerSource != 0 {
		t.Fatalf("synthetic corpus %d is not divisible by per-source token count", cumulativeTokens)
	}
	sourceCount := cumulativeTokens / h09SyntheticTokensPerSource
	worker, route, _, _, sealer, identity, _ := newSummaryRouteWorkerHarness(t, "", h09SyntheticContextWindow,
		CloudDSHConversationSummaryResult{Text: "unused", ModelID: "p1234567", SessionID: "unused", Through: 0})
	route.claim.Message.Sequence = int64(sourceCount + 1)
	fixture := newH09SyntheticSourceFixture(sourceCount)
	store := &h09SyntheticSummaryStore{identity: identity,
		head: app.ConversationSummaryHead{Identity: identity}}
	inputs := &h09SyntheticSummaryInputs{identity: identity, fixture: fixture, store: store}
	models := &h09SyntheticSummaryModels{routeWorkerStub: route, window: h09SyntheticContextWindow}
	worker.options.SummaryInputs = inputs
	worker.options.SummaryStore = store
	worker.options.Models = models

	tokenBudget := conversationSummaryMaintenanceTokenBudget(h09SyntheticContextWindow)
	if tokenBudget != 2_765 {
		t.Fatalf("synthetic test context token budget = %d, want 2765", tokenBudget)
	}
	compactions := 0
	for len(store.summary.Sources) < sourceCount {
		before := len(store.summary.Sources)
		committed, err := worker.compactConversationSummary(context.Background(), route.claim, "p1234567")
		if err != nil || !committed {
			t.Fatalf("synthetic %d-token corpus compaction %d: committed=%t covered=%d/%d err=%v",
				cumulativeTokens, compactions+1, committed, len(store.summary.Sources), sourceCount, err)
		}
		if len(store.summary.Sources) <= before {
			t.Fatalf("synthetic compaction made no source-proof progress: before=%d after=%d",
				before, len(store.summary.Sources))
		}
		compactions++
	}
	if compactions == 0 || len(models.compactRequests) != compactions {
		t.Fatalf("synthetic compaction calls = %d, recorded requests = %d", compactions, len(models.compactRequests))
	}

	for index, request := range models.compactRequests {
		requestTokens := h09SyntheticPreviousTokensIfPresent(request.PreviousSummaryText) +
			len(request.Sources)*h09SyntheticTokensPerSource
		if requestTokens > tokenBudget || requestTokens > h09SyntheticContextWindow {
			t.Fatalf("compact request %d exceeded synthetic token budget: %d > %d", index+1, requestTokens, tokenBudget)
		}
		if len(request.Sources) == 0 || len(request.Sources) >= sourceCount {
			t.Fatalf("compact request %d contained %d sources from %d-source corpus; want a bounded non-empty subset",
				index+1, len(request.Sources), sourceCount)
		}
		if got := conversationSummaryMaintenanceInputBytes(request.PreviousSummaryText, request.Sources); got > conversationSummaryMaintenanceMaxInputBytes {
			t.Fatalf("compact request %d exceeded encoded input byte cap: %d", index+1, got)
		}
	}

	document, err := app.OpenConversationSummary(sealer, store.summary)
	if err != nil {
		t.Fatalf("open final synthetic summary: %v", err)
	}
	if document.Version != int64(compactions) || len(document.Sources) != sourceCount {
		t.Fatalf("final synthetic summary version/proofs = %d/%d, want %d/%d",
			document.Version, len(document.Sources), compactions, sourceCount)
	}
	anchors := []int{1, max(1, sourceCount/2), sourceCount}
	for _, sequence := range anchors {
		anchor := h09SyntheticAnchor(sequence)
		if !strings.Contains(document.Text, anchor) {
			t.Errorf("final synthetic summary lost anchor %s", anchor)
		}
		original, ok := inputs.ReadOriginalHistory(sequence)
		if !ok || !strings.Contains(original.Content, anchor) || !bytes.Equal(original.Source.SHA256, h09SHA256Bytes(original.Content)) {
			t.Errorf("synthetic original-history fixture cannot read back %s with its source digest", anchor)
		}
		proof, ok := h09FindProof(document.Sources, original.Source)
		if !ok || !bytes.Equal(proof.SHA256, original.Source.SHA256) {
			t.Errorf("final summary cannot recover proof for %s", anchor)
		}
	}

	for index, original := range fixture {
		if index >= len(document.Sources) || !h09SameSummarySource(original.Source, document.Sources[index]) {
			t.Fatalf("synthetic proof %d = %+v, want original source %+v", index, document.Sources[index], original.Source)
		}
		readBack, ok := inputs.ReadOriginalHistory(index + 1)
		if !ok || readBack.Content != original.Content || !bytes.Equal(readBack.Source.SHA256, h09SHA256Bytes(readBack.Content)) {
			t.Fatalf("synthetic original source %d changed or became unreadable", index+1)
		}
	}
	if len(models.estimateRequests) == 0 {
		t.Fatal("synthetic estimator was not used by incremental summary selection")
	}
}

func TestConversationSummaryBudgetsScaleWithModelWindow(t *testing.T) {
	for _, test := range []struct {
		window int
		input  int
		output int
	}{
		{window: 4_096, input: 2_765, output: 819},
		{window: 8_192, input: 5_530, output: 1_638},
		{window: 16_384, input: 11_060, output: 3_276},
		{window: 32_768, input: 22_119, output: 6_553},
		{window: 65_536, input: 53_248, output: 8_192},
	} {
		if got := conversationSummaryMaintenanceTokenBudget(test.window); got != test.input {
			t.Errorf("window %d input budget = %d, want %d", test.window, got, test.input)
		}
		if got := conversationSummaryMaintenanceOutputTokens(test.window); got != test.output {
			t.Errorf("window %d output budget = %d, want %d", test.window, got, test.output)
		}
	}
	if conversationSummaryMaintenanceOutputTokens(0) != 0 || conversationSummaryMaintenanceTokenBudget(app.MaxModelContextWindowTokens+1) != 0 {
		t.Fatal("invalid model windows must not receive a summary budget")
	}
}

type h09SyntheticSummaryInputs struct {
	identity app.ConversationSummaryIdentity
	fixture  []app.ConversationSummaryPromptSource
	store    *h09SyntheticSummaryStore
	pageSize int
}

func (s *h09SyntheticSummaryInputs) LoadConversationSummaryInputPage(_ context.Context,
	request app.ConversationSummaryInputPageRequest) (app.ConversationSummaryInputPage, error) {
	if request.ContextWindowTokens != h09SyntheticContextWindow || request.Claim.Message.Sequence < 2 {
		return app.ConversationSummaryInputPage{}, fmt.Errorf("unexpected synthetic source page request")
	}
	pageSize := s.pageSize
	if pageSize < 1 {
		pageSize = 7 // Seven 8 KiB sources stay within the production 64 KiB page cap.
	}
	page := app.ConversationSummaryInputPage{Identity: s.identity,
		BoundarySequence: request.Claim.Message.Sequence - 1, Head: s.store.head}
	if request.Cursor != nil {
		if request.Cursor.SummaryVersion != s.store.head.Version || !bytes.Equal(request.Cursor.SummarySHA256, s.store.head.Digest) {
			return app.ConversationSummaryInputPage{}, fmt.Errorf("synthetic source cursor summary version changed")
		}
	} else if s.store.summary.Version > 0 {
		stored := s.store.summary
		page.Summary = &stored
	}
	covered := make(map[string]struct{}, len(s.store.summary.Sources))
	for _, proof := range s.store.summary.Sources {
		covered[h09SummarySourceKey(proof)] = struct{}{}
	}
	startAfter := int64(0)
	if request.Cursor != nil {
		startAfter = request.Cursor.Sequence
	}
	var candidates []app.ConversationSummaryPromptSource
	for _, source := range s.fixture {
		if source.Source.Sequence <= startAfter {
			continue
		}
		if _, ok := covered[h09SummarySourceKey(source.Source)]; ok {
			continue
		}
		candidates = append(candidates, source)
	}
	if len(candidates) > pageSize {
		candidates = candidates[:pageSize]
	}
	page.Sources = append(page.Sources, candidates...)
	remaining := 0
	if len(candidates) > 0 {
		last := candidates[len(candidates)-1]
		for _, source := range s.fixture {
			if source.Source.Sequence > last.Source.Sequence {
				if _, covered := covered[h09SummarySourceKey(source.Source)]; !covered {
					remaining++
				}
			}
		}
		if remaining > 0 {
			page.Next = &app.ConversationSummaryInputCursor{Identity: s.identity,
				BoundarySequence: page.BoundarySequence, SummaryVersion: s.store.head.Version,
				SummarySHA256: append([]byte(nil), s.store.head.Digest...), Sequence: last.Source.Sequence,
				KindOrder: 0, ID: last.Source.ID, ByteOffset: int64(len(last.Content))}
		}
	}
	return page, nil
}

func (s *h09SyntheticSummaryInputs) ReadOriginalHistory(sequence int) (app.ConversationSummaryPromptSource, bool) {
	if sequence < 1 || sequence > len(s.fixture) {
		return app.ConversationSummaryPromptSource{}, false
	}
	return s.fixture[sequence-1], true
}

type h09SyntheticSummaryStore struct {
	app.ConversationSummaryStore
	identity     app.ConversationSummaryIdentity
	head         app.ConversationSummaryHead
	summary      app.StoredConversationSummary
	operationSeq int
	operation    app.ConversationSummaryOperation
}

func (s *h09SyntheticSummaryStore) ClaimConversationSummaryOperation(_ context.Context,
	command app.ClaimConversationSummaryOperation) (app.ConversationSummaryOperation, bool, error) {
	s.operationSeq++
	operationID := fmt.Sprintf("o%07d", s.operationSeq)
	s.operation = app.ConversationSummaryOperation{Identity: s.identity, OperationID: operationID,
		BaselineVersion: command.BaselineVersion, InputSHA256: append([]byte(nil), command.InputSHA256...),
		State: "claimed", LeaseOwner: command.LeaseOwner, LeaseVersion: 1, CheckpointThrough: -1}
	return s.operation, false, nil
}

func (s *h09SyntheticSummaryStore) CommitConversationSummary(_ context.Context, command app.ConversationSummaryCommit) error {
	if command.Identity != s.identity || command.BaselineVersion != s.head.Version || command.OperationID != s.operation.OperationID {
		return fmt.Errorf("synthetic summary commit does not match current head or operation")
	}
	s.summary = app.StoredConversationSummary{Identity: s.identity, Version: command.BaselineVersion + 1,
		OperationID: command.OperationID, SummarySHA256: append([]byte(nil), command.SummarySHA256...),
		SourcesSHA256: append([]byte(nil), command.SourcesSHA256...), Ciphertext: append([]byte(nil), command.Ciphertext...),
		Sources: append([]app.ConversationSummarySource(nil), command.Sources...)}
	s.head = app.ConversationSummaryHead{Identity: s.identity, Version: s.summary.Version,
		Digest: append([]byte(nil), command.SummarySHA256...)}
	return nil
}

func (*h09SyntheticSummaryStore) ReleaseConversationSummaryOperation(context.Context,
	app.ConversationSummaryOperationRelease) error {
	return nil
}

type h09SyntheticSummaryModels struct {
	*routeWorkerStub
	window           int
	estimateRequests []CloudDSHConversationSummaryEstimateRequest
	compactRequests  []CloudDSHConversationSummaryRequest
}

func (s *h09SyntheticSummaryModels) ResolveConversationSummaryModel(context.Context, app.ConversationRouteClaim) (string, int, error) {
	return "p1234567", s.window, nil
}

func (s *h09SyntheticSummaryModels) EstimateConversationSummaryTokenPressure(_ context.Context,
	request CloudDSHConversationSummaryEstimateRequest) ([]int, error) {
	s.estimateRequests = append(s.estimateRequests, request)
	base := h09SyntheticPreviousTokensIfPresent(request.PreviousSummaryText)
	counts := make([]int, len(request.Sources))
	for index := range request.Sources {
		base += h09SyntheticTokensPerSource
		counts[index] = base
	}
	return counts, nil
}

func (s *h09SyntheticSummaryModels) CompactConversationSummary(_ context.Context,
	request CloudDSHConversationSummaryRequest) (CloudDSHConversationSummaryResult, error) {
	s.compactRequests = append(s.compactRequests, request)
	anchors := h09SyntheticAnchorsInText(request.PreviousSummaryText)
	seen := make(map[string]struct{}, len(anchors)+len(request.Sources))
	for _, anchor := range anchors {
		seen[anchor] = struct{}{}
	}
	for _, source := range request.Sources {
		for _, anchor := range h09SyntheticAnchorsInText(source.Content) {
			if _, ok := seen[anchor]; !ok {
				anchors = append(anchors, anchor)
				seen[anchor] = struct{}{}
			}
		}
	}
	text := strings.Join(anchors, "\n")
	if len(text) < h09SyntheticSourceBytes {
		text += strings.Repeat("s", h09SyntheticSourceBytes-len(text))
	}
	return CloudDSHConversationSummaryResult{Text: text, ModelID: "p1234567", SessionID: request.Operation.OperationID,
		Through: int64(len(request.Sources) - 1)}, nil
}

func newH09SyntheticSourceFixture(count int) []app.ConversationSummaryPromptSource {
	sources := make([]app.ConversationSummaryPromptSource, 0, count)
	for sequence := 1; sequence <= count; sequence++ {
		anchor := h09SyntheticAnchor(sequence)
		content := anchor + strings.Repeat("x", h09SyntheticSourceBytes-len(anchor))
		digest := h09SHA256Bytes(content)
		sources = append(sources, app.ConversationSummaryPromptSource{Source: app.ConversationSummarySource{
			Kind: app.ConversationSummarySourceMessage, ID: fmt.Sprintf("m%07d", sequence), Sequence: int64(sequence),
			SHA256: digest}, Role: "user", Content: content})
	}
	return sources
}

func h09SyntheticAnchor(sequence int) string {
	return fmt.Sprintf("H09_ANCHOR_%06d", sequence)
}

func h09SyntheticAnchorsInText(value string) []string {
	var anchors []string
	for offset := 0; offset+len("H09_ANCHOR_000000") <= len(value); offset++ {
		if strings.HasPrefix(value[offset:], "H09_ANCHOR_") {
			candidate := value[offset : offset+len("H09_ANCHOR_000000")]
			if candidate[len("H09_ANCHOR_"):] != "000000" && h09AllDecimalDigits(candidate[len("H09_ANCHOR_"):]) {
				anchors = append(anchors, candidate)
				offset += len(candidate) - 1
			}
		}
	}
	return anchors
}

func h09SyntheticPreviousTokensIfPresent(summary string) int {
	if strings.TrimSpace(summary) == "" {
		return 0
	}
	return h09SyntheticPreviousTokens
}

func h09FindProof(proofs []app.ConversationSummarySource, source app.ConversationSummarySource) (app.ConversationSummarySource, bool) {
	for _, proof := range proofs {
		if h09SummarySourceKey(proof) == h09SummarySourceKey(source) {
			return proof, true
		}
	}
	return app.ConversationSummarySource{}, false
}

func h09SameSummarySource(left, right app.ConversationSummarySource) bool {
	return h09SummarySourceKey(left) == h09SummarySourceKey(right) && bytes.Equal(left.SHA256, right.SHA256)
}

func h09SummarySourceKey(source app.ConversationSummarySource) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d", source.Kind, source.ID, source.ObjectType, source.Version, source.Sequence)
}

func h09SHA256Bytes(value string) []byte {
	digest := sha256.Sum256([]byte(value))
	return append([]byte(nil), digest[:]...)
}

func h09AllDecimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
