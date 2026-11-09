//go:build !linux

package runtimeactions

import (
	"context"
	"encoding/json"

	"morde-runtime/contract"
	"morde-runtime/runtimecore"
)

// ShellHandler is unavailable outside Linux because this implementation relies
// on cgroup v2 process domains and CLONE_INTO_CGROUP.
type ShellHandler struct{}

func NewShellHandler(ShellOptions) (*ShellHandler, error) {
	return nil, ErrProcessDomainUnavailable
}

func GemCredentialIDs() (uint32, uint32, error) {
	return 0, 0, ErrProcessDomainUnavailable
}

func NewApplicationProcessDomain(ApplicationProcessDomainOptions) (ApplicationProcessDomain, error) {
	return nil, ErrProcessDomainUnavailable
}

func (*ShellHandler) Capability() string { return shellCapability }

func (*ShellHandler) Action() string { return shellAction }

func (*ShellHandler) Execute(context.Context, contract.ExecuteRequest) (json.RawMessage, error) {
	return nil, ErrProcessDomainUnavailable
}

func (*ShellHandler) Stop(context.Context, contract.Operation) (runtimecore.StopResult, error) {
	return runtimecore.StopResult{}, ErrProcessDomainUnavailable
}

var _ runtimecore.ActionHandler = (*ShellHandler)(nil)
