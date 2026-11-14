package postgres

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

func TestPostgresPersonalMemoryHistoryCapturesFirstEditAndPurgesBodiesOnDelete(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 8192)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	firstBody := json.RawMessage(`{"content":"baseline memory"}`)
	created, err := store.ApplyPersonalMutation(ctx, principal, personalContentMutationForTest(t, owner.ID, 1, personal.Object{
		ObjectType: personal.ObjectTypeMemory, ObjectID: "global", Body: firstBody,
	}, 0))
	if err != nil || created.ResultRevision != 1 {
		t.Fatalf("create memory = %+v, %v", created, err)
	}
	secondBody := json.RawMessage(`{"content":"updated memory"}`)
	updated, err := store.ApplyPersonalMutation(ctx, principal, personalContentMutationForTest(t, owner.ID, 2, personal.Object{
		ObjectType: personal.ObjectTypeMemory, ObjectID: "global", Body: secondBody,
	}, 1))
	if err != nil || updated.ResultRevision != 2 {
		t.Fatalf("first memory edit = %+v, %v", updated, err)
	}

	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	firstPage, err := store.GetPersonalMemoryHistoryBatch(ctx, principal, "global", nil, 1)
	if err != nil || firstPage.ServiceInstanceID != instanceID || !firstPage.HasMore || len(firstPage.Items) != 1 ||
		firstPage.Items[0].Revision != 2 || !personalJSONEqual(firstPage.Items[0].Body, secondBody) ||
		firstPage.Items[0].SourceStatus != personal.MemoryHistorySourceAvailable || len(firstPage.Items[0].Sources) != 1 ||
		firstPage.Items[0].Sources[0].Kind != "mutation_receipt" || firstPage.Items[0].Sources[0].ID != "content-write-2" {
		t.Fatalf("first history page = %+v, %v", firstPage, err)
	}
	olderPage, err := store.GetPersonalMemoryHistoryBatch(ctx, principal, "global", &personal.MemoryHistoryCursor{
		Version: personal.MemoryHistoryCursorVersion, OwnerID: owner.ID, ServiceInstanceID: instanceID,
		ObjectType: personal.ObjectTypeMemory, ObjectID: "global", AfterRevision: 2,
	}, 10)
	if err != nil || olderPage.HasMore || len(olderPage.Items) != 1 || olderPage.Items[0].Revision != 1 ||
		!personalJSONEqual(olderPage.Items[0].Body, firstBody) || !olderPage.Items[0].Restorable ||
		olderPage.Items[0].SourceStatus != personal.MemoryHistorySourceAvailable || len(olderPage.Items[0].Sources) != 1 ||
		olderPage.Items[0].Sources[0].ID != "content-write-1" || olderPage.Items[0].ChangedAt.IsZero() {
		t.Fatalf("older history page = %+v, %v", olderPage, err)
	}
	var usedBytes, expectedBytes int64
	if err := pool.QueryRow(ctx, `SELECT used_bytes FROM personal_data_quotas WHERE owner_id=$1`, owner.ID).Scan(&usedBytes); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT octet_length($1::jsonb::text)+octet_length($2::jsonb::text)`, string(firstBody), string(secondBody)).Scan(&expectedBytes); err != nil {
		t.Fatal(err)
	}
	if usedBytes != expectedBytes {
		t.Fatalf("quota after first edit = %d, want current plus saved baseline %d", usedBytes, expectedBytes)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM personal_mutation_receipts WHERE owner_id=$1 AND mutation_id='content-write-1'`, owner.ID); err != nil {
		t.Fatal(err)
	}
	withheldPage, err := store.GetPersonalMemoryHistoryBatch(ctx, principal, "global", &personal.MemoryHistoryCursor{
		Version: personal.MemoryHistoryCursorVersion, OwnerID: owner.ID, ServiceInstanceID: instanceID,
		ObjectType: personal.ObjectTypeMemory, ObjectID: "global", AfterRevision: 2,
	}, 10)
	if err != nil || len(withheldPage.Items) != 1 || withheldPage.Items[0].Revision != 1 || len(withheldPage.Items[0].Body) != 0 ||
		withheldPage.Items[0].Restorable || withheldPage.Items[0].SourceStatus != personal.MemoryHistorySourceUnavailable || len(withheldPage.Items[0].Sources) != 0 {
		t.Fatalf("history with no longer authorized source was not withheld: %+v, %v", withheldPage, err)
	}

	deletion := personal.Mutation{
		OwnerID: owner.ID, MutationID: "content-delete-003", ObjectType: personal.ObjectTypeMemory,
		ObjectID: "global", BaseRevision: 2, Delete: true,
	}
	deletion.RequestHash, err = personal.HashMutationContent(deletion)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := store.ApplyPersonalMutation(ctx, principal, deletion)
	if err != nil || deleted.ResultRevision != 3 {
		t.Fatalf("delete memory = %+v, %v", deleted, err)
	}
	var remainingHistoryBodies int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM personal_memory_history
WHERE owner_id=$1 AND object_id='global' AND body IS NOT NULL`, owner.ID).Scan(&remainingHistoryBodies); err != nil || remainingHistoryBodies != 0 {
		t.Fatalf("deleted memory history retained %d bodies, err=%v", remainingHistoryBodies, err)
	}
	if err := pool.QueryRow(ctx, `SELECT used_bytes FROM personal_data_quotas WHERE owner_id=$1`, owner.ID).Scan(&usedBytes); err != nil || usedBytes != 0 {
		t.Fatalf("quota after memory delete = %d, want 0, err=%v", usedBytes, err)
	}
	if _, err := store.GetPersonalMemoryHistoryBatch(ctx, principal, "global", nil, 10); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("deleted memory history error = %v, want not_found", err)
	}
	otherOwner := createTestUser(t, store, 3, 4000, 8192)
	otherPrincipal := createPersonalReadSessionPrincipal(t, store, otherOwner.ID)
	if _, err := store.GetPersonalMemoryHistoryBatch(ctx, otherPrincipal, "global", nil, 10); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("inaccessible memory history error = %v, want not_found", err)
	}
}

func TestPostgresPersonalMemoryHistoryDoesNotInventLegacyBodies(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 8192)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	legacyBody := `{"content":"legacy current"}`
	if _, err := pool.Exec(ctx, `INSERT INTO account_change_counters(service_instance_id,owner_id,last_sequence)
VALUES ($1::uuid,$2,4) ON CONFLICT (service_instance_id,owner_id) DO UPDATE SET last_sequence=4`, instanceID, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_object_versions(service_instance_id,owner_id,object_type,object_id,revision,is_deleted)
VALUES ($1::uuid,$2,'memory','global',4,false)`, instanceID, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO personal_objects(service_instance_id,owner_id,object_type,object_id,body)
VALUES ($1::uuid,$2,'memory','global',$3::jsonb)`, instanceID, owner.ID, legacyBody); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_change_log
(service_instance_id,owner_id,change_sequence,object_type,object_id,revision,operation)
SELECT $1::uuid,$2,revision,'memory','global',revision,'upsert' FROM generate_series(1,4) AS seq(revision)`, instanceID, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE personal_data_quotas
SET used_bytes=octet_length($2::jsonb::text) WHERE service_instance_id=$1::uuid AND owner_id=$3`, instanceID, legacyBody, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyPersonalMutation(ctx, principal, personalContentMutationForTest(t, owner.ID, 40, personal.Object{
		ObjectType: personal.ObjectTypeMemory, ObjectID: "global", Body: json.RawMessage(`{"content":"new body"}`),
	}, 4)); err != nil {
		t.Fatal(err)
	}
	page, err := store.GetPersonalMemoryHistoryBatch(ctx, principal, "global", nil, 10)
	if err != nil || len(page.Items) != 5 {
		t.Fatalf("legacy memory history = %+v, %v; want five revision metadata entries", page, err)
	}
	for index, revision := range page.Items {
		if revision.Revision != int64(5-index) {
			t.Fatalf("legacy history revision %d = %d, want %d", index, revision.Revision, 5-index)
		}
		if index == 0 {
			if !personalJSONEqual(revision.Body, []byte(`{"content":"new body"}`)) || !revision.Restorable {
				t.Fatalf("new revision body = %s, restorable=%t", revision.Body, revision.Restorable)
			}
			continue
		}
		if index == 1 {
			if !personalJSONEqual(revision.Body, []byte(legacyBody)) || !revision.Restorable {
				t.Fatalf("first-edit baseline = %s, restorable=%t", revision.Body, revision.Restorable)
			}
			continue
		}
		if len(revision.Body) != 0 || revision.Restorable || revision.SourceStatus != personal.MemoryHistorySourceUnrecorded {
			t.Fatalf("legacy revision %d was fabricated: %+v", revision.Revision, revision)
		}
	}
}

func TestPostgresPersonalMemoryHistoryQuotaIsAtomicAndConcurrentCASHasOneWinner(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 8192)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	firstBody := json.RawMessage(`{"content":"starting point"}`)
	if _, err := store.ApplyPersonalMutation(ctx, principal, personalContentMutationForTest(t, owner.ID, 10, personal.Object{
		ObjectType: personal.ObjectTypeMemory, ObjectID: "global", Body: firstBody,
	}, 0)); err != nil {
		t.Fatal(err)
	}
	var usedBytes int64
	if err := pool.QueryRow(ctx, `SELECT used_bytes FROM personal_data_quotas WHERE owner_id=$1`, owner.ID).Scan(&usedBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE personal_data_quotas SET quota_bytes=used_bytes WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	tooLarge := json.RawMessage(`{"content":"this replacement makes the new current body exceed the quota while the original body is retained as history"}`)
	if _, err := store.ApplyPersonalMutation(ctx, principal, personalContentMutationForTest(t, owner.ID, 11, personal.Object{
		ObjectType: personal.ObjectTypeMemory, ObjectID: "global", Body: tooLarge,
	}, 1)); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("quota-bound update error = %v, want quota_exceeded", err)
	}
	var currentRevision int64
	var currentBody string
	var historyRows, changeRows int64
	if err := pool.QueryRow(ctx, `SELECT v.revision,p.body::text,
  (SELECT count(*) FROM personal_memory_history h WHERE h.owner_id=$1 AND h.object_id='global'),
  (SELECT count(*) FROM account_change_log l WHERE l.owner_id=$1 AND l.object_type='memory' AND l.object_id='global')
FROM account_object_versions v JOIN personal_objects p USING(service_instance_id,owner_id,object_type,object_id)
WHERE v.owner_id=$1 AND v.object_type='memory' AND v.object_id='global'`, owner.ID).Scan(&currentRevision, &currentBody, &historyRows, &changeRows); err != nil {
		t.Fatal(err)
	}
	if currentRevision != 1 || currentBody != `{"content": "starting point"}` || historyRows != 0 || changeRows != 1 {
		t.Fatalf("quota rejection left revision=%d body=%q history=%d changes=%d", currentRevision, currentBody, historyRows, changeRows)
	}

	// Raise the quota before two distinct requests race on the same CAS base.
	if _, err := pool.Exec(ctx, `UPDATE personal_data_quotas SET quota_bytes=8192 WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var callers sync.WaitGroup
	mutations := []personal.Mutation{
		personalContentMutationForTest(t, owner.ID, 20, personal.Object{
			ObjectType: personal.ObjectTypeMemory, ObjectID: "global", Body: json.RawMessage(`{"content":"winner one"}`),
		}, 1),
		personalContentMutationForTest(t, owner.ID, 21, personal.Object{
			ObjectType: personal.ObjectTypeMemory, ObjectID: "global", Body: json.RawMessage(`{"content":"winner two"}`),
		}, 1),
	}
	for _, mutation := range mutations {
		callers.Add(1)
		go func(value personal.Mutation) {
			defer callers.Done()
			<-start
			_, err := store.ApplyPersonalMutation(ctx, principal, value)
			results <- err
		}(mutation)
	}
	close(start)
	callers.Wait()
	first, second := <-results, <-results
	if (first == nil) == (second == nil) {
		t.Fatalf("concurrent memory CAS errors = %v, %v; want exactly one winner", first, second)
	}
	var conflicts, rows int64
	for _, err := range []error{first, second} {
		if domain.CodeOf(err) == domain.CodeConflict {
			conflicts++
		} else if err != nil {
			t.Fatalf("concurrent memory mutation error = %v, want conflict", err)
		}
	}
	if conflicts != 1 {
		t.Fatalf("concurrent conflict count = %d, want 1", conflicts)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM personal_memory_history WHERE owner_id=$1 AND object_id='global' AND object_revision=1 AND body IS NOT NULL`, owner.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("concurrent edits retained %d baseline rows, err=%v", rows, err)
	}
}

func TestPostgresPersonalMemoryHistoryCursorAndRevokedPrincipal(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 8192)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	if _, err := store.ApplyPersonalMutation(ctx, principal, personalContentMutationForTest(t, owner.ID, 30, personal.Object{
		ObjectType: personal.ObjectTypeMemory, ObjectID: "global", Body: json.RawMessage(`{"content":"one"}`),
	}, 0)); err != nil {
		t.Fatal(err)
	}
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wrongCursor := &personal.MemoryHistoryCursor{
		Version: personal.MemoryHistoryCursorVersion, OwnerID: owner.ID, ServiceInstanceID: instanceID,
		ObjectType: personal.ObjectTypeMemory, ObjectID: "user", AfterRevision: 1,
	}
	if _, err := store.GetPersonalMemoryHistoryBatch(ctx, principal, "global", wrongCursor, 10); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("cross-object history cursor error = %v, want invalid_argument", err)
	}
	installationID := createTestClientInstallation(t, pool, owner.ID, nil)
	clientToken := clientTestToken(t, owner.ID, installationID, identity.KindClient, nil)
	if _, err := store.CreateToken(ctx, clientToken); err != nil {
		t.Fatal(err)
	}
	clientPrincipal := identity.Principal{
		UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: clientToken.ID, ClientInstallationID: installationID,
	}
	clientPage, err := store.GetPersonalMemoryHistoryBatch(ctx, clientPrincipal, "global", nil, 10)
	if err != nil || len(clientPage.Items) != 1 || clientPage.Items[0].Revision != 1 {
		t.Fatalf("client memory history = %+v, %v", clientPage, err)
	}
	if err := store.RevokeToken(ctx, principal.TokenID, principal.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetPersonalMemoryHistoryBatch(ctx, principal, "global", nil, 10); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked history credential error = %v, want unauthenticated", err)
	}
}
