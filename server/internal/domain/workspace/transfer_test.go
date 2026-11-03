package workspace

import (
	"testing"
	"time"
)

func TestOwnershipTransferValidatesFrozenAndTerminalStates(t *testing.T) {
	created := time.Now().UTC()
	base := OwnershipTransfer{
		ID: "r1234567", WorkspaceID: "w1234567", FromOwnerID: "u1234567", ToOwnerID: "u7654321",
		State: OwnershipTransferPending, ExpectedRevision: 0, Version: 1, CreatedAt: created, UpdatedAt: created,
	}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, state := range []OwnershipTransferState{OwnershipTransferAccepting, OwnershipTransferUnknown, OwnershipTransferAccepted} {
		value := base
		value.State, value.OperationID = state, "o1234567"
		if state == OwnershipTransferAccepted {
			value.AcceptedAt = &created
		}
		if err := value.Validate(); err != nil {
			t.Errorf("%s transfer validation: %v", state, err)
		}
	}
	value := OwnershipTransfer{ID: "r1234567", WorkspaceID: "w1234567", FromOwnerID: "u1234567", ToOwnerID: "u7654321",
		State: OwnershipTransferUnknown, ExpectedRevision: 0, Version: 1, CreatedAt: created, UpdatedAt: created}
	if value.Validate() == nil {
		t.Fatal("unknown transfer without an operation validated")
	}
}
