package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

func TestPostgresRuntimeOperationLedgerPersistsHashChainAndIdempotentReplay(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	hostValue := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, nil)
	principal := identity.Principal{
		UserID: owner.ID, Kind: identity.KindHostReplica, HostScope: hostValue.ID, HostGeneration: hostValue.RuntimeGeneration,
	}
	first := testRuntimeLedgerEvent(1, "runtime_state", `{"host":"`+hostValue.ID+`"}`, "")
	acknowledged, err := store.AppendRuntimeOperationEvent(ctx, principal, app.RuntimeOperationLedgerCoreNamespace, first)
	if err != nil || !sameRuntimeOperationEvent(acknowledged, first) {
		t.Fatalf("append first operation ledger event = %#v, err=%v", acknowledged, err)
	}
	replayed, err := store.AppendRuntimeOperationEvent(ctx, principal, app.RuntimeOperationLedgerCoreNamespace, first)
	if err != nil || !sameRuntimeOperationEvent(replayed, first) {
		t.Fatalf("replay committed operation ledger event = %#v, err=%v", replayed, err)
	}

	fork := testRuntimeLedgerEvent(1, "runtime_state", `{"host":"other"}`, "")
	if _, err := store.AppendRuntimeOperationEvent(ctx, principal, app.RuntimeOperationLedgerCoreNamespace, fork); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("replay with different event data error=%v, want conflict", err)
	}
	wrongPrevious := testRuntimeLedgerEvent(2, "operation", `{"status":"accepted"}`, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if _, err := store.AppendRuntimeOperationEvent(ctx, principal, app.RuntimeOperationLedgerCoreNamespace, wrongPrevious); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("append with incorrect previous hash error=%v, want conflict", err)
	}
	second := testRuntimeLedgerEvent(2, "operation", `{"status":"accepted"}`, first.Hash)
	if _, err := store.AppendRuntimeOperationEvent(ctx, principal, app.RuntimeOperationLedgerCoreNamespace, second); err != nil {
		t.Fatalf("append second operation ledger event: %v", err)
	}

	page, err := store.ListRuntimeOperationEvents(ctx, principal, app.RuntimeOperationLedgerCoreNamespace, 0, 1)
	if err != nil || len(page.Items) != 1 || !page.HasMore || !sameRuntimeOperationEvent(page.Items[0], first) {
		t.Fatalf("first operation ledger page = %#v, err=%v", page, err)
	}
	page, err = store.ListRuntimeOperationEvents(ctx, principal, app.RuntimeOperationLedgerCoreNamespace, 1, 1)
	if err != nil || len(page.Items) != 1 || page.HasMore || !sameRuntimeOperationEvent(page.Items[0], second) {
		t.Fatalf("second operation ledger page = %#v, err=%v", page, err)
	}
	page, err = store.ListRuntimeOperationEvents(ctx, principal, app.RuntimeOperationLedgerBrowserNamespace, 0, 1)
	if err != nil || len(page.Items) != 0 || page.HasMore {
		t.Fatalf("browser namespace leaked core events: %#v, err=%v", page, err)
	}
	browserEvent := testRuntimeLedgerEvent(1, "browser.operation", `{"operation_id":"o1234567"}`, "")
	if _, err := store.AppendRuntimeOperationEvent(ctx, principal, app.RuntimeOperationLedgerBrowserNamespace, browserEvent); err != nil {
		t.Fatalf("append isolated browser event: %v", err)
	}
	page, err = store.ListRuntimeOperationEvents(ctx, principal, app.RuntimeOperationLedgerBrowserNamespace, 0, 1)
	if err != nil || len(page.Items) != 1 || !sameRuntimeOperationEvent(page.Items[0], browserEvent) {
		t.Fatalf("browser operation ledger page = %#v, err=%v", page, err)
	}

	stale := principal
	stale.HostGeneration++
	if _, err := store.ListRuntimeOperationEvents(ctx, stale, app.RuntimeOperationLedgerCoreNamespace, 0, 1); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("stale generation list error=%v, want unauthenticated", err)
	}
}

func testRuntimeLedgerEvent(sequence uint64, kind, data, previousHash string) app.RuntimeOperationLedgerEvent {
	input := runtimeOperationHashInput{
		Version: 1, Sequence: sequence, Kind: kind, Data: json.RawMessage(data), PreviousHash: previousHash,
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(encoded)
	return app.RuntimeOperationLedgerEvent{
		Version: 1, Sequence: sequence, Kind: kind, Data: json.RawMessage(data),
		PreviousHash: previousHash, Hash: hex.EncodeToString(digest[:]),
	}
}
