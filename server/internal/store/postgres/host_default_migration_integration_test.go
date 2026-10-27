package postgres

import (
	"context"
	"testing"

	"morde/server/internal/domain"
)

func TestHostDefaultsMigrationPreservesExplicitAdminQuotaAndExistingHost(t *testing.T) {
	_, pool := newTemporaryStore(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 47); err != nil {
		t.Fatalf("return to schema before host defaults: %v", err)
	}
	ids := make([]string, 3)
	for index := range ids {
		ids[index], err = domain.NewUserID()
		if err != nil {
			t.Fatal(err)
		}
		quota := 3
		if index == 2 {
			quota = 5
		}
		if _, err := pool.Exec(ctx, `INSERT INTO users
(id, name, password_hash, role, status, quota_hosts, quota_cpu_milli, quota_mem_mib)
VALUES ($1, $2, 'hash', 'user', 'approved', $3, 20000, 40000)`, ids[index], "migration-host-user-"+ids[index], quota); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO audit_logs (actor_kind, action, target, detail)
VALUES ('system', 'user.update', $1, '{"quota":{"hosts":3}}'::jsonb)`, ids[1]); err != nil {
		t.Fatal(err)
	}
	hostID, err := domain.NewHostID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, insertHostSQL,
		hostID, ids[0], "existing", "std-2c4g", "container", "stopped", "",
		int32(1000), int32(2048), int32(1000), int32(2048), []byte("sealed-key"),
		nil, nil, []byte("sealed-runtime"), nil, int64(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 48); err != nil {
		t.Fatalf("apply host defaults migration: %v", err)
	}
	for index, wantQuota := range []int{1, 3, 5} {
		var quota, cpu, mem int
		if err := pool.QueryRow(ctx, `SELECT quota_hosts, default_host_cpu_milli, default_host_mem_mib
FROM users WHERE id=$1`, ids[index]).Scan(&quota, &cpu, &mem); err != nil {
			t.Fatal(err)
		}
		if quota != wantQuota || cpu != 1000 || mem != 2048 {
			t.Fatalf("migrated user %d: quota=%d cpu=%d mem=%d", index, quota, cpu, mem)
		}
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM hosts WHERE id=$1`, hostID).Scan(&state); err != nil || state != "stopped" {
		t.Fatalf("existing host after migration: state=%q err=%v", state, err)
	}
}
