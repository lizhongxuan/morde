package app

import (
	"context"
	"encoding/json"

	"morde/server/internal/domain/identity"
)

const (
	RuntimeOperationLedgerCoreNamespace    = "runtime_core"
	RuntimeOperationLedgerBrowserNamespace = "browser"
	RuntimeOperationLedgerPageLimit        = 128
)

type RuntimeOperationLedgerEvent struct {
	Version      int             `json:"version"`
	Sequence     uint64          `json:"sequence"`
	Kind         string          `json:"kind"`
	Data         json.RawMessage `json:"data"`
	PreviousHash string          `json:"previous_hash"`
	Hash         string          `json:"hash"`
}

type RuntimeOperationLedgerPage struct {
	Items   []RuntimeOperationLedgerEvent `json:"items"`
	HasMore bool                          `json:"has_more"`
}

// RuntimeOperationLedgerService is called only from the authenticated
// Host Replica route group. Implementations bind every operation to the
// principal's current Host and runtime generation.
type RuntimeOperationLedgerService interface {
	ListRuntimeOperationEvents(context.Context, identity.Principal, string, uint64, int32) (RuntimeOperationLedgerPage, error)
	AppendRuntimeOperationEvent(context.Context, identity.Principal, string, RuntimeOperationLedgerEvent) (RuntimeOperationLedgerEvent, error)
}
