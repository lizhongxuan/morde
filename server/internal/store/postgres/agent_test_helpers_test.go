package postgres

import (
	"context"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
)

type testConfiguredModelCatalog struct{}

func (testConfiguredModelCatalog) IsConfiguredModel(context.Context, string, string) (bool, error) {
	return true, nil
}

func enableTaskExecutionForTest(t testing.TB, store *Store, principal identity.Principal) {
	t.Helper()
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	control, err := app.NewExecutionControlService(store, audit)
	if err != nil {
		t.Fatal(err)
	}
	current, err := control.Status(context.Background(), principal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.Enable(context.Background(), principal, current.Epoch, "test-execution-enable-0001"); err != nil {
		t.Fatal(err)
	}
}
