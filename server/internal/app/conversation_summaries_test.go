package app

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

func TestSealAndOpenConversationSummaryBindsIdentityAndSourceProof(t *testing.T) {
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	identity := ConversationSummaryIdentity{
		ServiceInstanceID: "00000000-0000-4000-8000-000000000001", OwnerID: "u1234567", ActorID: "u1234567",
		ConversationID: "c1234567", ScopeKind: ConversationSummaryPersonalScope, AuthorizationVersion: 1,
	}
	messageDigest := sha256.Sum256([]byte("original message"))
	document := ConversationSummaryDocument{Identity: identity, Version: 1, OperationID: "o1234567",
		Text: "The user is planning a trip.", Sources: []ConversationSummarySource{{
			Kind: ConversationSummarySourceMessage, ID: "m1234567", Version: 0, Sequence: 4, SHA256: messageDigest[:],
		}}}
	stored, err := SealConversationSummary(sealer, document)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored.Ciphertext) == document.Text || len(stored.Ciphertext) == 0 {
		t.Fatal("summary was not sealed")
	}
	opened, err := OpenConversationSummary(sealer, stored)
	if err != nil || opened.Text != document.Text || opened.Identity != identity || !sameConversationSummarySources(opened.Sources, document.Sources) {
		t.Fatalf("open summary = %+v, %v", opened, err)
	}

	changedIdentity := stored
	changedIdentity.Identity.ActorID = "u7654321"
	if _, err := OpenConversationSummary(sealer, changedIdentity); err == nil {
		t.Fatal("opened summary under another actor")
	}
	changedSources := stored
	changedSources.Sources = []ConversationSummarySource{{Kind: ConversationSummarySourceMessage, ID: "m7654321", Sequence: 4, SHA256: messageDigest[:]}}
	if _, err := OpenConversationSummary(sealer, changedSources); err == nil {
		t.Fatal("opened summary with changed stored source proof")
	}
}

func TestSummaryModelCallIdentityAndEncryptedResultAreBound(t *testing.T) {
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	identity := ConversationSummaryIdentity{ServiceInstanceID: "00000000-0000-4000-8000-000000000001",
		OwnerID: "u1234567", ActorID: "u1234567", ConversationID: "c1234567",
		ScopeKind: ConversationSummaryPersonalScope, AuthorizationVersion: 1}
	callID, err := DSHConversationSummaryModelCallID(identity.ConversationID, "o1234567", "summary-session-1", 4)
	if err != nil || callID == "" {
		t.Fatalf("summary model call ID = %q, %v", callID, err)
	}
	replayedID, err := DSHConversationSummaryModelCallID(identity.ConversationID, "o1234567", "summary-session-1", 4)
	if err != nil || replayedID != callID {
		t.Fatalf("summary model call ID changed across replay: %q/%q %v", callID, replayedID, err)
	}
	output := ConversationSummaryModelOutput{Identity: identity, OperationID: "o1234567", CallID: callID,
		ModelID: "p1234567", Text: "The user prefers morning meetings.",
		Usage: ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}}
	ciphertext, digest, err := SealConversationSummaryModelOutput(sealer, output)
	if err != nil || len(ciphertext) == 0 || len(digest) != sha256.Size || bytes.Contains(ciphertext, []byte(output.Text)) {
		t.Fatalf("sealed model output bytes=%d digest=%d err=%v", len(ciphertext), len(digest), err)
	}
	opened, err := OpenConversationSummaryModelOutput(sealer, ciphertext, digest, identity, output.OperationID, callID, output.ModelID)
	if err != nil || opened.Text != output.Text || opened.Usage != output.Usage {
		t.Fatalf("open model output = %+v, %v", opened, err)
	}
	if _, err := OpenConversationSummaryModelOutput(sealer, ciphertext, digest, identity, output.OperationID, "cmp_changed", output.ModelID); err == nil {
		t.Fatal("opened a summary result under a different call ID")
	}
}

func TestConversationSummaryScopeAndSourcesRejectInvalidClaims(t *testing.T) {
	claim := ConversationRouteClaim{
		Message: conversation.Message{ID: "m1234567", OwnerID: "u1234567", ActorID: "u1234567", ConversationID: "c1234567",
			Role: conversation.RoleUser, Sequence: 1},
		ServiceInstanceID: "00000000-0000-4000-8000-000000000001", Authorization: taskAuthorizationForSummary(),
		LeaseOwner: "route-worker", LeaseVersion: 1,
	}
	identity, err := ConversationSummaryScopeForClaim(claim)
	if err != nil || identity.ScopeKind != ConversationSummaryPersonalScope || identity.ScopeID != "" {
		t.Fatalf("personal summary scope = %+v, %v", identity, err)
	}
	claim.Message.ActorID = "u7654321"
	if _, err := ConversationSummaryScopeForClaim(claim); err == nil {
		t.Fatal("accepted a personal summary for a non-owner actor")
	}
	claim.Message.ActorID = claim.Message.OwnerID
	claim.Input.ProjectID = "j1234567"
	identity, err = ConversationSummaryScopeForClaim(claim)
	if err != nil || identity.ScopeKind != ConversationSummaryProjectScope || identity.ScopeID != "j1234567" {
		t.Fatalf("project summary scope = %+v, %v", identity, err)
	}
	claim.Input.ResultReviewID = "m7654321"
	if _, err := ConversationSummaryScopeForClaim(claim); err == nil {
		t.Fatal("accepted an internal result review as a user summary route")
	}

	digest := sha256.Sum256([]byte("message"))
	source := ConversationSummarySource{Kind: ConversationSummarySourceMessage, ID: "m1234567", Sequence: 1, SHA256: digest[:]}
	if _, err := ConversationSummarySourcesDigest([]ConversationSummarySource{source, source}); err == nil {
		t.Fatal("accepted a duplicated summary source")
	}
	if _, err := ConversationSummarySourcesDigest([]ConversationSummarySource{source}); err != nil {
		t.Fatalf("valid summary source rejected: %v", err)
	}
}

func TestConversationSummaryPromptDigestBindsTextAndProof(t *testing.T) {
	firstDigest := sha256.Sum256([]byte("first"))
	secondDigest := sha256.Sum256([]byte("second"))
	one := ConversationSummaryPromptSource{Source: ConversationSummarySource{Kind: ConversationSummarySourceMessage,
		ID: "m1234567", Sequence: 1, SHA256: firstDigest[:]}, Role: "user", Content: "first"}
	two := ConversationSummaryPromptSource{Source: ConversationSummarySource{Kind: ConversationSummarySourceMessage,
		ID: "m2345678", Sequence: 2, SHA256: secondDigest[:]}, Role: "assistant", Content: "second"}
	digest, err := ConversationSummaryPromptDigest([]ConversationSummaryPromptSource{one, two})
	if err != nil || len(digest) != sha256.Size {
		t.Fatalf("conversation summary prompt digest = %x, %v", digest, err)
	}
	changed := two
	changed.Content = "changed"
	changedDigest, err := ConversationSummaryPromptDigest([]ConversationSummaryPromptSource{one, changed})
	if err != nil || bytes.Equal(digest, changedDigest) {
		t.Fatalf("prompt content was not bound to the digest: %x/%x, %v", digest, changedDigest, err)
	}
	firstPart := one
	firstPart.Content = "前半"
	secondPart := one
	secondPart.Content, secondPart.ByteOffset = "后半", int64(len(firstPart.Content))
	chunkDigest, err := ConversationSummaryPromptDigest([]ConversationSummaryPromptSource{firstPart, secondPart})
	if err != nil || len(chunkDigest) != sha256.Size {
		t.Fatalf("same source split on a UTF-8 byte boundary was rejected: %x, %v", chunkDigest, err)
	}
	proofs, err := ConversationSummaryPromptProofs([]ConversationSummaryPromptSource{firstPart, secondPart})
	if err != nil || len(proofs) != 1 || !bytes.Equal(proofs[0].SHA256, firstDigest[:]) {
		t.Fatalf("chunked source proofs = %+v, %v", proofs, err)
	}
	secondPart.ByteOffset++
	if _, err := ConversationSummaryPromptDigest([]ConversationSummaryPromptSource{firstPart, secondPart}); err == nil {
		t.Fatal("accepted non-contiguous chunks from one source")
	}
	if page, err := ConversationSummaryInputPageBudget(4096); err != nil || page != MaxConversationSummaryInputPageBytes {
		t.Fatalf("minimum-window page budget = %d, %v", page, err)
	}
	if page, err := ConversationSummaryInputPageBudget(32_768); err != nil || page != MaxConversationSummaryInputPageBytes {
		t.Fatalf("standard-window page budget = %d, %v", page, err)
	}
}

func TestCheckConversationSummaryRestoreNamesEachCondition(t *testing.T) {
	identity := ConversationSummaryIdentity{
		ServiceInstanceID: "00000000-0000-4000-8000-000000000001", OwnerID: "u1234567", ActorID: "u1234567",
		ConversationID: "c1234567", ScopeKind: ConversationSummaryPersonalScope, AuthorizationVersion: 1,
	}
	textDigest := sha256.Sum256([]byte("summary text"))
	otherDigest := sha256.Sum256([]byte("other text"))
	baseHead := ConversationSummaryHead{Identity: identity, Version: 2, Digest: textDigest[:]}
	baseStored := &StoredConversationSummary{Identity: identity, Version: 2, SummarySHA256: textDigest[:], Ciphertext: []byte("sealed-bytes")}
	baseDocument := &ConversationSummaryDocument{Identity: identity, Version: 2, Text: "summary text"}
	if failure := CheckConversationSummaryRestore(identity, baseHead, baseStored, baseDocument, nil); failure != nil {
		t.Fatalf("matching stored summary must restore cleanly, got %v", failure)
	}
	if failure := CheckConversationSummaryRestore(identity, ConversationSummaryHead{Identity: identity}, nil, nil, nil); failure != nil {
		t.Fatalf("head without a version and without a stored summary must restore cleanly, got %v", failure)
	}
	cases := []struct {
		name      string
		head      ConversationSummaryHead
		stored    *StoredConversationSummary
		document  *ConversationSummaryDocument
		openErr   error
		condition string
	}{
		{name: "open failed", head: baseHead, stored: baseStored, openErr: errors.New("seal open failed"), condition: ConversationSummaryRestoreOpenFailed},
		{name: "head without stored version", head: baseHead, stored: nil, condition: ConversationSummaryRestoreHeadWithoutStored},
		{name: "head version invalid", head: ConversationSummaryHead{Identity: identity, Version: 0, Digest: textDigest[:]}, stored: baseStored, document: baseDocument, condition: ConversationSummaryRestoreHeadVersionInvalid},
		{name: "identity mismatch", head: baseHead, stored: baseStored, document: &ConversationSummaryDocument{Identity: ConversationSummaryIdentity{
			ServiceInstanceID: identity.ServiceInstanceID, OwnerID: identity.OwnerID, ActorID: "u7654321",
			ConversationID: identity.ConversationID, ScopeKind: identity.ScopeKind, AuthorizationVersion: 1},
			Version: 2, Text: "summary text"}, condition: ConversationSummaryRestoreIdentityMismatch},
		{name: "version mismatch", head: baseHead, stored: baseStored, document: &ConversationSummaryDocument{Identity: identity, Version: 3, Text: "summary text"}, condition: ConversationSummaryRestoreVersionMismatch},
		{name: "digest mismatch", head: ConversationSummaryHead{Identity: identity, Version: 2, Digest: otherDigest[:]}, stored: baseStored, document: baseDocument, condition: ConversationSummaryRestoreDigestMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failure := CheckConversationSummaryRestore(identity, tc.head, tc.stored, tc.document, tc.openErr)
			if failure == nil {
				t.Fatal("restore check must fail")
			}
			if failure.Condition != tc.condition {
				t.Fatalf("restore condition = %q, want %q (failure: %v)", failure.Condition, tc.condition, failure)
			}
			if failure.Reason == "" {
				t.Fatal("restore failure must carry a reason")
			}
			if tc.stored != nil && failure.StoredBytes != int64(len(tc.stored.Ciphertext)) {
				t.Fatalf("stored bytes = %d, want %d", failure.StoredBytes, len(tc.stored.Ciphertext))
			}
			if len([]rune(failure.Preview)) > ConversationSummaryRestorePreviewMaxRunes {
				t.Fatalf("preview is %d runes, over the 1024 limit", len([]rune(failure.Preview)))
			}
			err := failure.ToError()
			if domain.CodeOf(err) != domain.CodeInvalidState || err.Details["condition"] != tc.condition {
				t.Fatalf("restore domain error = %+v, want invalid_state with condition %q", err, tc.condition)
			}
			if !strings.Contains(err.Error(), tc.condition) {
				t.Fatalf("restore error %q does not name its condition", err.Error())
			}
		})
	}
}

func TestCheckConversationSummaryRestorePreviewIsBounded(t *testing.T) {
	identity := ConversationSummaryIdentity{
		ServiceInstanceID: "00000000-0000-4000-8000-000000000001", OwnerID: "u1234567", ActorID: "u1234567",
		ConversationID: "c1234567", ScopeKind: ConversationSummaryPersonalScope, AuthorizationVersion: 1,
	}
	textDigest := sha256.Sum256([]byte("summary text"))
	head := ConversationSummaryHead{Identity: identity, Version: 2, Digest: textDigest[:]}
	stored := &StoredConversationSummary{Identity: identity, Version: 2, SummarySHA256: textDigest[:], Ciphertext: []byte("sealed-bytes")}
	longText := strings.Repeat("字", 2000)
	document := &ConversationSummaryDocument{Identity: identity, Version: 3, Text: longText}
	failure := CheckConversationSummaryRestore(identity, head, stored, document, nil)
	if failure == nil || failure.Condition != ConversationSummaryRestoreVersionMismatch {
		t.Fatalf("long-text version mismatch must fail by name, got %v", failure)
	}
	if got := len([]rune(failure.Preview)); got != ConversationSummaryRestorePreviewMaxRunes {
		t.Fatalf("preview is %d runes, want exactly 1024", got)
	}
	if failure.RestoredBytes != int64(len(longText)) {
		t.Fatalf("restored bytes = %d, want %d", failure.RestoredBytes, len(longText))
	}
}

func taskAuthorizationForSummary() task.Authorization {
	return task.Authorization{Version: 1}
}
