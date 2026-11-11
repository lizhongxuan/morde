package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

const (
	conversationSummaryMaintenanceLease          = 2 * time.Minute
	conversationSummaryMaintenanceMaxPages       = 512
	conversationSummaryMaintenanceMaxSources     = 256
	conversationSummaryMaintenanceOverheadBytes  = 1024
	conversationSummaryMaintenanceMaxInputBytes  = 96 << 10
	conversationSummaryMaintenanceMaxOutputBytes = 16 << 10
)

const conversationSummaryIncompleteNotice = "【历史摘要不完整】当前摘要尚未覆盖全部可访问历史；需要更早的细节时请检索会话原文。"

type conversationSummaryMaintainer interface {
	ResolveConversationSummaryModel(context.Context, app.ConversationRouteClaim) (string, int, error)
	CompactConversationSummary(context.Context, CloudDSHConversationSummaryRequest) (CloudDSHConversationSummaryResult, error)
}

type conversationSummaryMaintenanceInput struct {
	identity        app.ConversationSummaryIdentity
	head            app.ConversationSummaryHead
	previousText    string
	previousProofs  []app.ConversationSummarySource
	previousDigest  []byte
	previousVersion int64
	historyComplete bool
	sources         []app.ConversationSummaryPromptSource
	proofs          []app.ConversationSummarySource
	inputBytes      int
}

func (w *ConversationRouteWorker) compactConversationSummary(ctx context.Context, claim app.ConversationRouteClaim, routeModelID string) (bool, error) {
	if w.options.SummaryStore == nil || claim.Input.ResultReviewID != "" {
		return false, nil
	}
	maintainer, ok := w.options.Models.(conversationSummaryMaintainer)
	if !ok {
		return false, nil
	}
	modelID, contextWindow, err := maintainer.ResolveConversationSummaryModel(ctx, claim)
	if err != nil {
		return false, err
	}
	if modelID != routeModelID {
		return false, domain.NewError(domain.CodeConflict, "conversation summary model changed during routing")
	}
	page, err := w.options.SummaryInputs.LoadConversationSummaryInputPage(ctx, app.ConversationSummaryInputPageRequest{
		Claim: claim, ContextWindowTokens: contextWindow,
	})
	if err != nil {
		return false, err
	}
	input, ready, err := collectConversationSummaryMaintenanceInput(ctx, w.options.SummaryInputs, claim, contextWindow, page, w.options.SummarySealer)
	if err != nil || !ready {
		return false, err
	}
	inputDigest, err := conversationSummaryOperationInputDigest(input.identity, input.head.Version,
		input.previousVersion, input.previousDigest, modelID, contextWindow, input.sources)
	if err != nil {
		return false, err
	}
	leaseOwner := conversationSummaryMaintenanceLeaseOwner(w.options.WorkerID, claim)
	operation, _, err := w.options.SummaryStore.ClaimConversationSummaryOperation(ctx, app.ClaimConversationSummaryOperation{
		Claim: claim, InputSHA256: inputDigest, BaselineVersion: input.head.Version,
		LeaseOwner: leaseOwner, LeaseDuration: conversationSummaryMaintenanceLease,
	})
	if err != nil {
		return false, err
	}
	if operation.State == "committed" {
		return true, nil
	}
	if operation.LeaseOwner != leaseOwner || operation.LeaseVersion < 1 {
		return false, domain.NewError(domain.CodeConflict, "conversation summary operation lease is unavailable")
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		state := "failed"
		if ctx.Err() != nil {
			state = "cancelled"
		}
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = w.options.SummaryStore.ReleaseConversationSummaryOperation(releaseCtx, app.ConversationSummaryOperationRelease{
			Claim: claim, OperationID: operation.OperationID, LeaseOwner: leaseOwner,
			LeaseVersion: operation.LeaseVersion, State: state,
		})
	}()
	result, err := maintainer.CompactConversationSummary(ctx, CloudDSHConversationSummaryRequest{
		Claim: claim, Operation: operation, Sources: input.sources,
		PreviousSummaryText: input.previousText, PreviousSummaryVersion: input.previousVersion,
		PreviousSummarySHA256: input.previousDigest,
	})
	if err != nil {
		return false, err
	}
	text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(result.Text), conversationSummaryIncompleteNotice+"\n\n"))
	if !input.historyComplete {
		text = conversationSummaryIncompleteNotice + "\n\n" + text
	}
	if strings.TrimSpace(text) == "" || len(text) > conversationSummaryMaintenanceMaxOutputBytes ||
		len(text) >= input.inputBytes || result.ModelID != modelID || result.SessionID == "" || result.Through < 0 {
		return false, domain.NewError(domain.CodeInvalidState, "conversation summary did not produce a smaller bounded result")
	}
	proofs, err := mergeConversationSummaryProofs(input.previousProofs, input.proofs)
	if err != nil {
		return false, err
	}
	stored, err := app.SealConversationSummary(w.options.SummarySealer, app.ConversationSummaryDocument{
		Identity: input.identity, Version: input.head.Version + 1, OperationID: operation.OperationID,
		Text: text, Sources: proofs,
	})
	if err != nil {
		return false, err
	}
	if err := w.options.SummaryStore.CommitConversationSummary(ctx, app.ConversationSummaryCommit{
		Claim: claim, Identity: input.identity, OperationID: operation.OperationID, LeaseOwner: leaseOwner,
		LeaseVersion: operation.LeaseVersion, BaselineVersion: input.head.Version,
		SummarySHA256: stored.SummarySHA256, SourcesSHA256: stored.SourcesSHA256,
		Ciphertext: stored.Ciphertext, Sources: proofs,
	}); err != nil {
		return false, err
	}
	committed = true
	return true, nil
}

func collectConversationSummaryMaintenanceInput(ctx context.Context, pages app.ConversationSummaryInputPageStore,
	claim app.ConversationRouteClaim, contextWindow int, first app.ConversationSummaryInputPage, sealer *app.Sealer) (conversationSummaryMaintenanceInput, bool, error) {
	input := conversationSummaryMaintenanceInput{identity: first.Identity, head: first.Head}
	if first.BoundarySequence != claim.Message.Sequence-1 || first.Head.Identity != first.Identity {
		return input, false, domain.NewError(domain.CodeConflict, "conversation summary input boundary changed")
	}
	expected, err := app.ConversationSummaryScopeForClaim(claim)
	if err != nil || !sameSummaryMaintenanceIdentity(expected, first.Identity) {
		return input, false, domain.NewError(domain.CodeConflict, "conversation summary authorization changed")
	}
	if first.Summary != nil {
		document, err := app.OpenConversationSummary(sealer, *first.Summary)
		if err != nil || document.Identity != first.Identity || document.Version != first.Head.Version ||
			!bytes.Equal(first.Head.Digest, first.Summary.SummarySHA256) || first.Head.Version < 1 {
			return input, false, domain.NewError(domain.CodeInvalidState, "stored conversation summary cannot be restored")
		}
		input.previousText, input.previousVersion = document.Text, document.Version
		input.previousProofs = append([]app.ConversationSummarySource(nil), document.Sources...)
		input.previousDigest = append([]byte(nil), first.Summary.SummarySHA256...)
	} else if first.Head.Version != 0 {
		return input, false, domain.NewError(domain.CodeInvalidState, "conversation summary head has no stored version")
	}
	budget := conversationSummaryMaintenanceInputBudget(contextWindow)
	if budget <= 0 {
		return input, false, domain.NewError(domain.CodeUnsupported, "conversation summary model capacity is unavailable")
	}
	var cursor *app.ConversationSummaryInputCursor
	page := first
	var current []app.ConversationSummaryPromptSource
	currentKey := ""
	currentEnd := int64(0)
	selectedCount := 0
	inspectedCount := 0
	ready, stopped, exhausted := false, false, false
	appendCurrent := func() error {
		if len(current) == 0 {
			return nil
		}
		candidate := append(append([]app.ConversationSummaryPromptSource(nil), input.sources...), current...)
		if selectedCount+1 > conversationSummaryMaintenanceMaxSources || len(candidate) > app.MaxConversationSummarySources ||
			conversationSummaryMaintenanceEstimate(input.previousText, candidate) > budget {
			stopped = true
			return nil
		}
		input.sources = candidate
		input.proofs = append(input.proofs, current[0].Source)
		selectedCount++
		input.inputBytes = conversationSummaryMaintenanceEstimate(input.previousText, input.sources)
		current = nil
		currentKey, currentEnd = "", 0
		if input.inputBytes >= budget*3/4 {
			ready, stopped = true, true
		}
		return nil
	}
	for pageIndex := 0; pageIndex < conversationSummaryMaintenanceMaxPages; pageIndex++ {
		if page.Identity != input.identity || page.Head.Version != input.head.Version ||
			!bytes.Equal(page.Head.Digest, input.head.Digest) || page.BoundarySequence != first.BoundarySequence {
			return input, false, domain.NewError(domain.CodeConflict, "conversation summary input changed while paging")
		}
		if len(page.Sources) == 0 && page.Next != nil {
			return input, false, domain.NewError(domain.CodeInvalidState, "conversation summary input cursor made no progress")
		}
		for _, item := range page.Sources {
			inspectedCount++
			if inspectedCount > app.MaxConversationSummarySources {
				stopped = true
				break
			}
			key := summaryMaintenanceSourceKey(item.Source)
			if currentKey != "" && key != currentKey {
				if err := appendCurrent(); err != nil {
					return input, false, err
				}
				if stopped {
					break
				}
			}
			if currentKey == "" {
				if item.ByteOffset != 0 {
					return input, false, domain.NewError(domain.CodeInvalidState, "conversation summary source page starts mid-message")
				}
				currentKey = key
				currentEnd = 0
			}
			if item.ByteOffset != currentEnd {
				return input, false, domain.NewError(domain.CodeInvalidState, "conversation summary source chunks are not contiguous")
			}
			current = append(current, item)
			currentEnd += int64(len(item.Content))
			if conversationSummaryMaintenanceEstimate(input.previousText,
				append(append([]app.ConversationSummaryPromptSource(nil), input.sources...), current...)) > budget {
				stopped = true
				break
			}
		}
		if stopped {
			break
		}
		if page.Next == nil {
			if err := appendCurrent(); err != nil {
				return input, false, err
			}
			exhausted = true
			break
		}
		cursor = page.Next
		page, err = pages.LoadConversationSummaryInputPage(ctx, app.ConversationSummaryInputPageRequest{
			Claim: claim, ContextWindowTokens: contextWindow, Cursor: cursor,
		})
		if err != nil {
			return input, false, err
		}
	}
	if !stopped && !exhausted {
		return input, false, domain.NewError(domain.CodeInvalidState, "conversation summary input exceeded the page limit")
	}
	if !ready && !stopped && len(current) > 0 {
		if err := appendCurrent(); err != nil {
			return input, false, err
		}
	}
	input.historyComplete = exhausted
	if len(input.sources) == 0 || input.inputBytes < budget*3/4 {
		return input, false, nil
	}
	if _, err := app.ConversationSummaryPromptDigest(input.sources); err != nil {
		return input, false, err
	}
	input.proofs, err = app.ConversationSummaryPromptProofs(input.sources)
	return input, err == nil, err
}

func conversationSummaryMaintenanceInputBudget(contextWindow int) int {
	if contextWindow < 4096 || contextWindow > 1_000_000 {
		return 0
	}
	headroom := min(4096, contextWindow/8)
	maxTokens := min(2048, max(256, contextWindow/16))
	return min(conversationSummaryMaintenanceMaxInputBytes, contextWindow-headroom-maxTokens-conversationSummaryMaintenanceOverheadBytes)
}

func conversationSummaryMaintenanceEstimate(previous string, sources []app.ConversationSummaryPromptSource) int {
	encoded, err := json.Marshal(cloudDSHConversationSummarySources(sources))
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return len(encoded) + len(previous) + conversationSummaryMaintenanceOverheadBytes
}

func conversationSummaryOperationInputDigest(identity app.ConversationSummaryIdentity, baseline, previousVersion int64,
	previousDigest []byte, modelID string, contextWindow int, sources []app.ConversationSummaryPromptSource) ([]byte, error) {
	promptDigest, err := app.ConversationSummaryPromptDigest(sources)
	if err != nil {
		return nil, err
	}
	if previousVersion == 0 && (len(previousDigest) != 0) || previousVersion > 0 && len(previousDigest) != sha256.Size {
		return nil, domain.InvalidArgument("conversation_summary_source", "prior summary proof is invalid")
	}
	encoded, err := json.Marshal(struct {
		Version       int                             `json:"version"`
		Identity      app.ConversationSummaryIdentity `json:"identity"`
		Baseline      int64                           `json:"baseline_version"`
		Previous      int64                           `json:"previous_summary_version"`
		PreviousSHA   []byte                          `json:"previous_summary_sha256"`
		ModelID       string                          `json:"model_id"`
		ContextWindow int                             `json:"context_window_tokens"`
		PromptSHA     []byte                          `json:"prompt_sha256"`
	}{1, identity, baseline, previousVersion, previousDigest, modelID, contextWindow, promptDigest})
	if err != nil {
		return nil, fmt.Errorf("encode conversation summary operation input: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return append([]byte(nil), digest[:]...), nil
}

func conversationSummaryMaintenanceLeaseOwner(workerID string, claim app.ConversationRouteClaim) string {
	digest := sha256.Sum256([]byte(workerID + "\n" + claim.Message.ID + "\n" + fmt.Sprint(claim.LeaseVersion)))
	return "summary_" + hex.EncodeToString(digest[:16])
}

func summaryMaintenanceSourceKey(source app.ConversationSummarySource) string {
	return strings.Join([]string{source.Kind, source.ID, source.ObjectType, fmt.Sprint(source.Version),
		fmt.Sprint(source.Sequence)}, "\x00")
}

func mergeConversationSummaryProofs(existing, fresh []app.ConversationSummarySource) ([]app.ConversationSummarySource, error) {
	merged := append([]app.ConversationSummarySource(nil), existing...)
	seen := make(map[string]app.ConversationSummarySource, len(merged)+len(fresh))
	for _, source := range merged {
		seen[summaryMaintenanceSourceKey(source)] = source
	}
	for _, source := range fresh {
		key := summaryMaintenanceSourceKey(source)
		if prior, ok := seen[key]; ok {
			if !bytes.Equal(prior.SHA256, source.SHA256) {
				return nil, domain.NewError(domain.CodeConflict, "conversation summary source proof changed")
			}
			continue
		}
		seen[key] = source
		merged = append(merged, source)
	}
	if _, err := app.ConversationSummarySourcesDigest(merged); err != nil {
		return nil, err
	}
	return merged, nil
}

func sameSummaryMaintenanceIdentity(expected, actual app.ConversationSummaryIdentity) bool {
	return expected.ServiceInstanceID == actual.ServiceInstanceID && expected.OwnerID == actual.OwnerID &&
		expected.ActorID == actual.ActorID && expected.ConversationID == actual.ConversationID &&
		expected.ScopeKind == actual.ScopeKind && expected.ScopeID == actual.ScopeID &&
		expected.AuthorizationVersion == actual.AuthorizationVersion &&
		(expected.ScopeKind == app.ConversationSummaryPersonalScope && actual.ScopeVersion == 0 ||
			expected.ScopeKind == app.ConversationSummaryProjectScope && actual.ScopeVersion > 0)
}
