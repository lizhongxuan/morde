package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
)

func TestMainConversationEnsureSelectionSwitchAndReplay(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 1, 1000, 2048)

	state, err := store.GetMainConversation(ctx, owner.ID)
	if err != nil || state.Status != conversation.MainConversationUninitialized || state.Version != 0 || state.ConversationID != "" {
		t.Fatalf("initial main conversation = %+v, err=%v", state, err)
	}
	ensured, replayed, ensureRecord := ensureMainConversationForTest(t, store, owner.ID, "main-ensure-001")
	if replayed || ensured.Status != conversation.MainConversationReady || ensured.Version != 1 {
		t.Fatalf("ensure new account = %+v, replay=%t", ensured, replayed)
	}
	if replay, replayed, err := store.EnsureMainConversation(ctx, conversation.Conversation{ID: mustID(t, domain.NewConversationID), OwnerID: owner.ID}, ensureRecord); err != nil || !replayed || replay != ensured {
		t.Fatalf("ensure retry = %+v, replay=%t, err=%v; want %+v", replay, replayed, err, ensured)
	}

	second := createMainConversationForTest(t, store, owner.ID)
	setRecord := setMainConversationRecord(owner.ID, second.ID, 1, "main-switch-001")
	changed, replayed, err := store.SetMainConversation(ctx, owner.ID, second.ID, 1, setRecord)
	if err != nil || replayed || changed.Status != conversation.MainConversationReady || changed.ConversationID != second.ID || changed.Version != 2 {
		t.Fatalf("switch main conversation = %+v, replay=%t", changed, replayed)
	}
	if _, _, err := setMainConversationForTest(t, store, owner.ID, ensured.ConversationID, 1, "main-switch-stale-01"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale expected version should conflict: %v", err)
	}
	if replay, replayed, err := store.SetMainConversation(ctx, owner.ID, ensured.ConversationID, 1, setRecord); err != nil || !replayed || replay != changed {
		t.Fatalf("switch retry = %+v, replay=%t, err=%v; want original %+v", replay, replayed, err, changed)
	}
	if _, _, err := setMainConversationForTest(t, store, owner.ID, ensured.ConversationID, 2, "main-switch-001"); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("same key with changed request should conflict: %v", err)
	}
	other := createTestUser(t, store, 1, 1000, 2048)
	otherConversation := createMainConversationForTest(t, store, other.ID)
	if _, _, err := setMainConversationForTest(t, store, owner.ID, otherConversation.ID, 2, "main-switch-cross-01"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-account conversation selection should be hidden: %v", err)
	}
	state, err = store.GetMainConversation(ctx, owner.ID)
	if err != nil || state != changed {
		t.Fatalf("current main conversation = %+v, err=%v; want %+v", state, err, changed)
	}
	var tasks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE owner_id=$1`, owner.ID).Scan(&tasks); err != nil || tasks != 0 {
		t.Fatalf("main conversation operations created %d tasks, err=%v", tasks, err)
	}
}

func TestMainConversationEnsureSelectionAndConcurrentInitialization(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()

	owner := createTestUser(t, store, 1, 1000, 2048)
	createMainConversationForTest(t, store, owner.ID)
	second := createMainConversationForTest(t, store, owner.ID)
	state, err := store.GetMainConversation(ctx, owner.ID)
	if err != nil || state.Status != conversation.MainConversationSelectionRequired {
		t.Fatalf("multiple history candidates = %+v, err=%v", state, err)
	}
	selected, replayed, _ := ensureMainConversationForTest(t, store, owner.ID, "main-select-001")
	if replayed || selected.Status != conversation.MainConversationSelectionRequired || selected.ConversationID != "" || selected.Version != 0 {
		t.Fatalf("ensure with ambiguous candidates = %+v, replay=%t", selected, replayed)
	}
	selected, replayed, _ = setMainConversationForTest(t, store, owner.ID, second.ID, 0, "main-select-002")
	if replayed || selected.Status != conversation.MainConversationReady || selected.ConversationID != second.ID || selected.Version != 1 {
		t.Fatalf("explicitly selected candidate = %+v, replay=%t", selected, replayed)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversations WHERE owner_id=$1 AND archived_at IS NULL`, owner.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("selection altered history: candidate count=%d, err=%v", count, err)
	}
	concurrentOwner := createTestUser(t, store, 1, 1000, 2048)
	values := []conversation.Conversation{
		{ID: mustID(t, domain.NewConversationID), OwnerID: concurrentOwner.ID},
		{ID: mustID(t, domain.NewConversationID), OwnerID: concurrentOwner.ID},
	}
	results := make(chan struct {
		value    conversation.MainConversation
		replayed bool
		err      error
	}, 2)
	var wait sync.WaitGroup
	for index, value := range values {
		wait.Add(1)
		go func(index int, value conversation.Conversation) {
			defer wait.Done()
			key := fmt.Sprintf("main-race-%03d", index)
			record := ensureMainConversationRecord(concurrentOwner.ID, key, value.ID)
			main, replayed, err := store.EnsureMainConversation(ctx, value, record)
			results <- struct {
				value    conversation.MainConversation
				replayed bool
				err      error
			}{main, replayed, err}
		}(index, value)
	}
	wait.Wait()
	close(results)
	var winner conversation.MainConversation
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent ensure: %v", result.err)
		}
		if winner.Status == "" {
			winner = result.value
		} else if result.value != winner {
			t.Fatalf("concurrent ensure produced different main pointers: %+v and %+v", winner, result.value)
		}
	}
	if winner.Status != conversation.MainConversationReady || winner.Version != 1 {
		t.Fatalf("concurrent main pointer = %+v", winner)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversations WHERE owner_id=$1 AND archived_at IS NULL`, concurrentOwner.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("concurrent ensure created %d conversations, err=%v; want one", count, err)
	}
}

func TestMainConversationConcurrentSwitchUsesVersionCAS(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 1, 1000, 2048)
	current, _, _ := ensureMainConversationForTest(t, store, owner.ID, "main-cas-init-001")
	targets := []string{createMainConversationForTest(t, store, owner.ID).ID, createMainConversationForTest(t, store, owner.ID).ID}
	results := make(chan error, len(targets))
	var wait sync.WaitGroup
	for index, target := range targets {
		wait.Add(1)
		go func(index int, target string) {
			defer wait.Done()
			key := fmt.Sprintf("main-cas-%03d", index)
			_, _, err := store.SetMainConversation(ctx, owner.ID, target, current.Version, setMainConversationRecord(owner.ID, target, current.Version, key))
			results <- err
		}(index, target)
	}
	wait.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if domain.CodeOf(err) == domain.CodeConflict {
			conflicts++
		} else {
			t.Fatalf("concurrent switch error = %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("concurrent CAS wins/conflicts = %d/%d, want 1/1", wins, conflicts)
	}
	state, err := store.GetMainConversation(ctx, owner.ID)
	if err != nil || state.Status != conversation.MainConversationReady || state.Version != current.Version+1 {
		t.Fatalf("concurrent switch result = %+v, err=%v", state, err)
	}
}

func ensureMainConversationForTest(t *testing.T, store *Store, ownerID, key string) (conversation.MainConversation, bool, app.IdempotencyRecord) {
	t.Helper()
	value := conversation.Conversation{ID: mustID(t, domain.NewConversationID), OwnerID: ownerID}
	record := ensureMainConversationRecord(ownerID, key, value.ID)
	result, replayed, err := store.EnsureMainConversation(context.Background(), value, record)
	if err != nil {
		t.Fatal(err)
	}
	return result, replayed, record
}

func setMainConversationForTest(t *testing.T, store *Store, ownerID, conversationID string, expectedVersion int64, key string) (conversation.MainConversation, bool, error) {
	t.Helper()
	record := setMainConversationRecord(ownerID, conversationID, expectedVersion, key)
	return store.SetMainConversation(context.Background(), ownerID, conversationID, expectedVersion, record)
}

func ensureMainConversationRecord(ownerID, key, newConversationID string) app.IdempotencyRecord {
	hash := sha256.Sum256([]byte("{}"))
	return app.IdempotencyRecord{OwnerID: ownerID, Operation: "main_conversation.ensure", Key: key, RequestHash: hash[:], ResourceID: newConversationID}
}

func setMainConversationRecord(ownerID, conversationID string, expectedVersion int64, key string) app.IdempotencyRecord {
	request, _ := json.Marshal(struct {
		ConversationID  string `json:"conversation_id"`
		ExpectedVersion int64  `json:"expected_version"`
	}{ConversationID: conversationID, ExpectedVersion: expectedVersion})
	hash := sha256.Sum256(request)
	return app.IdempotencyRecord{OwnerID: ownerID, Operation: "main_conversation.set", Key: key, RequestHash: hash[:], ResourceID: conversationID}
}

func createMainConversationForTest(t *testing.T, store *Store, ownerID string) conversation.Conversation {
	t.Helper()
	id := mustID(t, domain.NewConversationID)
	hash := sha256.Sum256([]byte(id))
	record := app.IdempotencyRecord{OwnerID: ownerID, Operation: "conversation.create", Key: "conversation-create-" + id, RequestHash: hash[:], ResourceID: id}
	value, _, err := store.CreateConversation(context.Background(), conversation.Conversation{ID: id, OwnerID: ownerID}, record)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
