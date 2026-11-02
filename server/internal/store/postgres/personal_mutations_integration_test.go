package postgres

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/personal"
)

func TestPostgresPersonalMutationCASIdempotencyAndTombstone(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	owner := createTestUser(t, store, 3, 4000, 8192)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	initial := personalMutationForTest(t, owner.ID, "profile-create-001", 0, `{"name":"Muse","avatar":"dragon"}`, false)

	start := make(chan struct{})
	type outcome struct {
		receipt personal.MutationReceipt
		err     error
	}
	results := make(chan outcome, 2)
	var callers sync.WaitGroup
	for range 2 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			<-start
			receipt, err := store.ApplyPersonalMutation(ctx, principal, initial)
			results <- outcome{receipt: receipt, err: err}
		}()
	}
	close(start)
	callers.Wait()
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.receipt.ResultRevision != 1 || second.receipt.ResultRevision != 1 ||
		first.receipt.AccountChangeSequence != 1 || second.receipt.AccountChangeSequence != 1 || first.receipt.Replay == second.receipt.Replay {
		t.Fatalf("concurrent idempotent mutations = (%+v, %v), (%+v, %v)", first.receipt, first.err, second.receipt, second.err)
	}

	update := personalMutationForTest(t, owner.ID, "profile-update-001", 1, `{"name":"Muse 2","avatar":"dragon"}`, false)
	updated, err := store.ApplyPersonalMutation(ctx, principal, update)
	if err != nil || updated.ResultRevision != 2 || updated.AccountChangeSequence != 2 {
		t.Fatalf("profile update = %+v, %v", updated, err)
	}
	changedReplay := personalMutationForTest(t, owner.ID, "profile-create-001", 1, `{"name":"different","avatar":"dragon"}`, false)
	if _, err := store.ApplyPersonalMutation(ctx, principal, changedReplay); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("reusing a mutation ID with different content error = %v, want idempotency_conflict", err)
	}
	stale := personalMutationForTest(t, owner.ID, "profile-stale-001", 1, `{"name":"stale","avatar":"dragon"}`, false)
	if _, err := store.ApplyPersonalMutation(ctx, principal, stale); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale profile update error = %v, want conflict", err)
	}

	deletion := personalMutationForTest(t, owner.ID, "profile-delete-001", 2, "", true)
	deleted, err := store.ApplyPersonalMutation(ctx, principal, deletion)
	if err != nil || deleted.ResultRevision != 3 || deleted.AccountChangeSequence != 3 {
		t.Fatalf("profile deletion = %+v, %v", deleted, err)
	}
	if _, err := store.GetPersonalObject(ctx, principal, personal.ObjectTypeProfile, personal.ProfileObjectID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("deleted profile read error = %v, want not_found", err)
	}
	resurrection := personalMutationForTest(t, owner.ID, "profile-recreate-001", 3, `{"name":"again","avatar":"dragon"}`, false)
	if _, err := store.ApplyPersonalMutation(ctx, principal, resurrection); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("profile resurrection error = %v, want conflict", err)
	}
	var currentRevision int64
	var deletedFlag bool
	if err := store.pool.QueryRow(ctx, `SELECT revision, is_deleted FROM account_object_versions
WHERE owner_id=$1 AND object_type=$2 AND object_id=$3`, owner.ID, personal.ObjectTypeProfile, personal.ProfileObjectID).
		Scan(&currentRevision, &deletedFlag); err != nil || currentRevision != 3 || !deletedFlag {
		t.Fatalf("profile tombstone = revision %d deleted %t err %v", currentRevision, deletedFlag, err)
	}
	replayedAfterDelete, err := store.ApplyPersonalMutation(ctx, principal, initial)
	if err != nil || !replayedAfterDelete.Replay || replayedAfterDelete.ResultRevision != 1 || replayedAfterDelete.AccountChangeSequence != 1 {
		t.Fatalf("create replay after delete = %+v, %v", replayedAfterDelete, err)
	}
	if _, err := store.GetPersonalObject(ctx, principal, personal.ObjectTypeProfile, personal.ProfileObjectID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("create replay restored deleted profile: %v", err)
	}
	var lastSequence, changeCount int64
	if err := store.pool.QueryRow(ctx, `SELECT last_sequence FROM account_change_counters WHERE owner_id=$1`, owner.ID).
		Scan(&lastSequence); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM account_change_log WHERE owner_id=$1`, owner.ID).
		Scan(&changeCount); err != nil || lastSequence != 3 || changeCount != 3 {
		t.Fatalf("replay after delete changed tombstone history: last_sequence=%d changes=%d err=%v", lastSequence, changeCount, err)
	}

	if err := store.RevokeToken(ctx, principal.TokenID, principal.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyPersonalMutation(ctx, principal, initial); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("replay after credential revocation error = %v, want unauthenticated", err)
	}
}

func TestPostgresPersonalMutationRollsBackBusinessWritesWhenReceiptFails(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 8192)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	if _, err := store.pool.Exec(ctx, `CREATE FUNCTION reject_personal_receipt() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced receipt failure'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `CREATE TRIGGER reject_personal_receipt BEFORE INSERT ON personal_mutation_receipts
FOR EACH ROW EXECUTE FUNCTION reject_personal_receipt()`); err != nil {
		t.Fatal(err)
	}
	mutation := personalMutationForTest(t, owner.ID, "profile-rollback-001", 0, `{"name":"Muse","avatar":"dragon"}`, false)
	if _, err := store.ApplyPersonalMutation(ctx, principal, mutation); err == nil {
		t.Fatal("mutation succeeded despite rejected receipt insert")
	}
	var objects, versions, changes, receipts int64
	var sequence int64
	err := store.pool.QueryRow(ctx, `SELECT
  (SELECT count(*) FROM personal_objects WHERE owner_id=$1),
  (SELECT count(*) FROM account_object_versions WHERE owner_id=$1 AND object_type='profile'),
  (SELECT count(*) FROM account_change_log WHERE owner_id=$1),
  (SELECT count(*) FROM personal_mutation_receipts WHERE owner_id=$1),
	  COALESCE((SELECT last_sequence FROM account_change_counters WHERE owner_id=$1), 0)`, owner.ID).
		Scan(&objects, &versions, &changes, &receipts, &sequence)
	if err != nil || objects != 0 || versions != 0 || changes != 0 || receipts != 0 || sequence != 0 {
		t.Fatalf("failed mutation left objects=%d versions=%d changes=%d receipts=%d sequence=%d err=%v",
			objects, versions, changes, receipts, sequence, err)
	}
}

func TestPostgresPersonalMutationQuotaRejectsGrowthButAllowsDelete(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 8192)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	initial := personalMutationForTest(t, owner.ID, "profile-quota-create", 0, `{"name":"Muse","avatar":"dragon"}`, false)
	if _, err := store.ApplyPersonalMutation(ctx, principal, initial); err != nil {
		t.Fatal(err)
	}
	var usedBytes int64
	if err := store.pool.QueryRow(ctx, `SELECT used_bytes FROM personal_data_quotas WHERE owner_id=$1`, owner.ID).Scan(&usedBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE personal_data_quotas SET quota_bytes=used_bytes WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	growth := personalMutationForTest(t, owner.ID, "profile-quota-grow", 1, `{"name":"Muse","avatar":"dragon","style":"flat"}`, false)
	if _, err := store.ApplyPersonalMutation(ctx, principal, growth); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("over-quota object update error = %v, want quota_exceeded", err)
	}
	var body []byte
	var revision int64
	if err := store.pool.QueryRow(ctx, `SELECT p.body::text,v.revision FROM personal_objects p
JOIN account_object_versions v USING (service_instance_id,owner_id,object_type,object_id)
WHERE p.owner_id=$1 AND p.object_type=$2 AND p.object_id=$3`, owner.ID, personal.ObjectTypeProfile, personal.ProfileObjectID).
		Scan(&body, &revision); err != nil || revision != 1 || strings.Contains(string(body), "style") {
		t.Fatalf("rejected growth changed saved object body=%s revision=%d err=%v", body, revision, err)
	}
	var remainingUsed int64
	if err := store.pool.QueryRow(ctx, `SELECT used_bytes FROM personal_data_quotas WHERE owner_id=$1`, owner.ID).Scan(&remainingUsed); err != nil || remainingUsed != usedBytes {
		t.Fatalf("rejected growth usage = %d, want %d, err=%v", remainingUsed, usedBytes, err)
	}
	deletion := personalMutationForTest(t, owner.ID, "profile-quota-delete", 1, "", true)
	if _, err := store.ApplyPersonalMutation(ctx, principal, deletion); err != nil {
		t.Fatalf("delete at quota limit: %v", err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT used_bytes FROM personal_data_quotas WHERE owner_id=$1`, owner.ID).Scan(&remainingUsed); err != nil || remainingUsed != 0 {
		t.Fatalf("usage after delete = %d, want 0, err=%v", remainingUsed, err)
	}
}

func TestPostgresGoalUsesPersonalQuotaVersionAndOwnerLedger(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 8192)
	other := createTestUser(t, store, 3, 4000, 8192)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	otherPrincipal := createPersonalReadSessionPrincipal(t, store, other.ID)

	foreignMaterial := personalObjectMutationForTest(t, other.ID, "library-foreign-1", personal.ObjectTypeLibraryItem, "library_private", 0,
		`{"name":"Private","kind":"artifact"}`, false)
	if _, err := store.ApplyPersonalMutation(ctx, otherPrincipal, foreignMaterial); err != nil {
		t.Fatal(err)
	}
	foreignGoal := personalObjectMutationForTest(t, owner.ID, "goal-foreign-1", personal.ObjectTypeGoal, "g_owner_1", 0,
		`{"title":"Learn Go","status":"active","steps":[],"related_material_ids":["library_private"]}`, false)
	if _, err := store.ApplyPersonalMutation(ctx, principal, foreignGoal); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("goal referencing another account's material error = %v, want invalid_argument", err)
	}

	material := personalObjectMutationForTest(t, owner.ID, "library-owner-1", personal.ObjectTypeLibraryItem, "library_own", 0,
		`{"name":"My notes","kind":"artifact"}`, false)
	if _, err := store.ApplyPersonalMutation(ctx, principal, material); err != nil {
		t.Fatal(err)
	}
	goalBody := `{"title":"Learn Go","status":"active","steps":[{"id":"step_1","title":"Read the tour","status":"in_progress"}],"related_material_ids":["library_own"]}`
	create := personalObjectMutationForTest(t, owner.ID, "goal-create-001", personal.ObjectTypeGoal, "g_owner_1", 0, goalBody, false)
	created, err := store.ApplyPersonalMutation(ctx, principal, create)
	if err != nil || created.ResultRevision != 1 || created.AccountChangeSequence != 2 {
		t.Fatalf("create goal = %+v, %v", created, err)
	}
	if _, err := store.GetPersonalObject(ctx, otherPrincipal, personal.ObjectTypeGoal, "g_owner_1"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("other account read goal error = %v, want not_found", err)
	}
	if _, err := store.ApplyPersonalMutation(ctx, principal, personalObjectMutationForTest(t, owner.ID, "goal-stale-001", personal.ObjectTypeGoal,
		"g_owner_1", 0, goalBody, false)); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale goal update error = %v, want conflict", err)
	}

	var used int64
	if err := store.pool.QueryRow(ctx, `SELECT used_bytes FROM personal_data_quotas WHERE owner_id=$1`, owner.ID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE personal_data_quotas SET quota_bytes=used_bytes WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	oversizedGoal := `{"title":"Learn Go","description":"` + strings.Repeat("x", 1024) + `","status":"active","steps":[{"id":"step_1","title":"Read the tour","status":"in_progress"}],"related_material_ids":["library_own"]}`
	if _, err := store.ApplyPersonalMutation(ctx, principal, personalObjectMutationForTest(t, owner.ID, "goal-grow-001", personal.ObjectTypeGoal,
		"g_owner_1", 1, oversizedGoal, false)); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("over-quota goal growth error = %v, want quota_exceeded", err)
	}
	var revision int64
	var savedBody []byte
	if err := store.pool.QueryRow(ctx, `SELECT v.revision,p.body::text FROM personal_objects p
JOIN account_object_versions v USING (service_instance_id,owner_id,object_type,object_id)
WHERE p.owner_id=$1 AND p.object_type=$2 AND p.object_id=$3`, owner.ID, personal.ObjectTypeGoal, "g_owner_1").Scan(&revision, &savedBody); err != nil || revision != 1 || strings.Contains(string(savedBody), "description") {
		t.Fatalf("rejected goal growth saved body=%s revision=%d err=%v", savedBody, revision, err)
	}
	var unchangedUsed int64
	if err := store.pool.QueryRow(ctx, `SELECT used_bytes FROM personal_data_quotas WHERE owner_id=$1`, owner.ID).Scan(&unchangedUsed); err != nil || unchangedUsed != used {
		t.Fatalf("quota usage after rejected growth = %d, want %d, err=%v", unchangedUsed, used, err)
	}

	shrink := `{"title":"Learn Go","status":"active","steps":[{"id":"step_1","title":"Read the tour","status":"done"}],"related_material_ids":["library_own"]}`
	updated, err := store.ApplyPersonalMutation(ctx, principal, personalObjectMutationForTest(t, owner.ID, "goal-shrink-001", personal.ObjectTypeGoal,
		"g_owner_1", 1, shrink, false))
	if err != nil || updated.ResultRevision != 2 || updated.AccountChangeSequence != 3 {
		t.Fatalf("shrink goal = %+v, %v", updated, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT used_bytes FROM personal_data_quotas WHERE owner_id=$1`, owner.ID).Scan(&unchangedUsed); err != nil || unchangedUsed >= used {
		t.Fatalf("quota usage after goal shrink = %d, want below %d, err=%v", unchangedUsed, used, err)
	}
	deleted, err := store.ApplyPersonalMutation(ctx, principal, personalObjectMutationForTest(t, owner.ID, "goal-delete-001", personal.ObjectTypeGoal,
		"g_owner_1", 2, "", true))
	if err != nil || deleted.ResultRevision != 3 || deleted.AccountChangeSequence != 4 {
		t.Fatalf("delete goal = %+v, %v", deleted, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT used_bytes FROM personal_data_quotas WHERE owner_id=$1`, owner.ID).Scan(&unchangedUsed); err != nil || unchangedUsed == 0 {
		t.Fatalf("quota usage after goal delete = %d, want retained library item usage, err=%v", unchangedUsed, err)
	}
	if _, err := store.GetPersonalObject(ctx, principal, personal.ObjectTypeGoal, "g_owner_1"); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("deleted goal read error = %v, want not_found", err)
	}
}

func personalMutationForTest(t *testing.T, ownerID, mutationID string, revision int64, body string, deleted bool) personal.Mutation {
	t.Helper()
	mutation := personal.Mutation{
		OwnerID: ownerID, MutationID: mutationID, ObjectType: personal.ObjectTypeProfile,
		ObjectID: personal.ProfileObjectID, BaseRevision: revision, Delete: deleted,
	}
	if body != "" {
		mutation.Body = []byte(body)
	}
	hash, err := personal.HashMutationContent(mutation)
	if err != nil {
		t.Fatalf("hash personal mutation: %v", err)
	}
	mutation.RequestHash = hash
	return mutation
}

func personalObjectMutationForTest(t *testing.T, ownerID, mutationID, objectType, objectID string, revision int64, body string, deleted bool) personal.Mutation {
	t.Helper()
	mutation := personal.Mutation{
		OwnerID: ownerID, MutationID: mutationID, ObjectType: objectType, ObjectID: objectID,
		BaseRevision: revision, Delete: deleted,
	}
	if body != "" {
		mutation.Body = []byte(body)
	}
	hash, err := personal.HashMutationContent(mutation)
	if err != nil {
		t.Fatalf("hash personal mutation: %v", err)
	}
	mutation.RequestHash = hash
	return mutation
}
