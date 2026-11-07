package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func TestProjectConversationDerivativesWaitReplayAndIsolation(t *testing.T) {
	f := newProjectAttachmentFixture(t)
	ctx := context.Background()
	scope := f.receive(t, "derivative-message-001", []string{"derivative-slot-001"})[0]
	original := []byte("synthetic pdf original")
	metadata := syntheticProjectAttachmentMetadata(original)
	metadata.Name, metadata.MediaType, metadata.DerivativeMode = "synthetic.pdf", "application/pdf", "pdf"
	initialized, err := f.service.Initialize(ctx, f.member, scope, metadata)
	if err != nil || initialized.DerivativeState != "awaiting" || initialized.DerivativeMode != "pdf" {
		t.Fatalf("initialize derivative mode: %+v err=%v", initialized, err)
	}
	changedMode := metadata
	changedMode.DerivativeMode = "none"
	if _, err := f.service.Initialize(ctx, f.member, scope, changedMode); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed mode accepted: %v", err)
	}
	digest := sha256.Sum256(original)
	if _, err := f.service.PutChunk(ctx, f.member, scope, 0, hex.EncodeToString(digest[:]), bytes.NewReader(original), int64(len(original))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Commit(ctx, f.member, scope); err != nil {
		t.Fatal(err)
	}
	if _, found, err := f.store.ClaimConversationRoute(ctx, "derivative-router", time.Minute); err != nil || found {
		t.Fatalf("pending derivatives did not hold their message: found=%t err=%v", found, err)
	}
	unrelated := f.receive(t, "derivative-unrelated-001", nil)
	if len(unrelated) != 0 {
		t.Fatal("unexpected unrelated slots")
	}
	claim, found, err := f.store.ClaimConversationRoute(ctx, "derivative-router", time.Minute)
	if err != nil || !found || claim.Message.ID == scope.MessageID {
		t.Fatalf("unrelated message blocked: found=%t err=%v", found, err)
	}
	text := []byte("synthetic extracted PDF text")
	textDigest := sha256.Sum256(text)
	set := app.ProjectConversationDerivatives{Outcome: "completed", Files: []app.ProjectConversationDerivativeFile{{
		Kind: "extracted_text", Page: 0, Name: "synthetic-text.txt", MediaType: "text/plain",
		SHA256: hex.EncodeToString(textDigest[:]), Content: text,
	}}}
	if _, err := f.service.PutDerivatives(ctx, f.owner, scope, set); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("another project member modified sender's derivatives: %v", err)
	}
	wrongInstance := scope
	wrongInstance.ServiceInstanceID = "22222222-2222-4222-8222-222222222222"
	if _, err := f.service.PutDerivatives(ctx, f.member, wrongInstance, set); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("wrong service instance accepted: %v", err)
	}
	const workers = 4
	results := make(chan app.ProjectConversationAttachmentStatus, workers)
	errors := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := f.service.PutDerivatives(ctx, f.member, scope, set)
			results <- result
			errors <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent derivative upload: %v", err)
		}
	}
	writes := 0
	for result := range results {
		if result.DerivativeState != "completed" || result.AttachmentID != initialized.AttachmentID {
			t.Fatalf("invalid receipt: %+v", result)
		}
		if !result.Replay {
			writes++
		}
	}
	if writes != 1 {
		t.Fatalf("expected one committed set, got %d", writes)
	}
	conflict := app.ProjectConversationDerivatives{Outcome: "skipped", Files: []app.ProjectConversationDerivativeFile{}}
	if _, err := f.service.PutDerivatives(ctx, f.member, scope, conflict); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed derivative outcome accepted: %v", err)
	}
	claim, found, err = f.store.ClaimConversationRoute(ctx, "derivative-router", time.Minute)
	if err != nil || !found || claim.Message.ID != scope.MessageID {
		t.Fatalf("completed set not routed: found=%t err=%v", found, err)
	}
	if replay, err := f.service.PutDerivatives(ctx, f.member, scope, set); err != nil || !replay.Replay {
		t.Fatalf("lost-response retry after routing: %+v err=%v", replay, err)
	}
	skipScope := f.receive(t, "derivative-message-skipped-001", []string{"derivative-slot-skipped-001"})[0]
	if _, err := f.service.Initialize(ctx, f.member, skipScope, metadata); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.PutChunk(ctx, f.member, skipScope, 0, hex.EncodeToString(digest[:]), bytes.NewReader(original), int64(len(original))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Commit(ctx, f.member, skipScope); err != nil {
		t.Fatal(err)
	}
	skipped, err := f.service.PutDerivatives(ctx, f.member, skipScope, conflict)
	if err != nil || skipped.DerivativeState != "skipped" {
		t.Fatalf("explicit skip: %+v err=%v", skipped, err)
	}
	if replay, err := f.service.PutDerivatives(ctx, f.member, skipScope, conflict); err != nil || !replay.Replay {
		t.Fatalf("skip replay: %+v err=%v", replay, err)
	}
	claim, found, err = f.store.ClaimConversationRoute(ctx, "derivative-router", time.Minute)
	if err != nil || !found || claim.Message.ID != skipScope.MessageID {
		t.Fatalf("skipped set not routed: found=%t err=%v", found, err)
	}
}
