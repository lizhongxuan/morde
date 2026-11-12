package postgres

import (
	"context"
	"strings"
	"testing"
)

func TestPersonalModelLedgerMigrationPreservesHistoricalAttribution(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 1, 1000, 2048)
	provider, closeProvider, err := newMigrationProvider(store.pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 134); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_connections(id,owner_id,name,base_url,model_name,api_key_cipher)
VALUES ('p1234567',$1,'personal','https://model.example/v1','test-model',decode(repeat('01',29),'hex')),
('p7654321',NULL,'platform','https://model.example/v1','test-model',decode(repeat('01',29),'hex')),
('p3333333',$1,'recreated','https://model.example/v1','test-model',decode(repeat('01',29),'hex'))`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE model_connections SET created_at=clock_timestamp()+interval '1 day' WHERE id='p3333333'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_call_ledger(owner_id,source,call_id,request_digest,model_id,provider_model,
reserved_tokens,state,input_tokens,output_tokens,settled_at) VALUES
($1,'chat','personal-call-01',decode(repeat('01',32),'hex'),'p1234567','test-model',100,'settled',4,5,now()),
($1,'chat','platform-call-01',decode(repeat('02',32),'hex'),'p7654321','test-model',100,'settled',6,7,now()),
($1,'chat','deleted-model-01',decode(repeat('03',32),'hex'),'p0000000','test-model',100,'settled',8,9,now()),
($1,'chat','recreated-model-01',decode(repeat('04',32),'hex'),'p3333333','test-model',100,'settled',1,2,now())`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 135); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id       string
		personal bool
	}{{"personal-call-01", true}, {"platform-call-01", false}, {"deleted-model-01", false}, {"recreated-model-01", false}} {
		var personal bool
		var input, output int64
		if err := store.pool.QueryRow(ctx, `SELECT personal_model,input_tokens,output_tokens FROM model_call_ledger WHERE owner_id=$1 AND call_id=$2`, owner.ID, test.id).
			Scan(&personal, &input, &output); err != nil || personal != test.personal || input+output == 0 {
			t.Fatalf("historical call %s: personal=%v usage=%d/%d err=%v", test.id, personal, input, output, err)
		}
	}
	if _, err := provider.DownTo(ctx, 134); err == nil || !strings.Contains(err.Error(), "cannot discard personal model ledger attribution") {
		t.Fatalf("rollback with personal usage = %v", err)
	}
}
