package app

import (
	"context"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type HostCopyCommand struct {
	Name                    string
	ExpectedCurrentRevision int64
}

type HostCopyService struct{}

func NewHostCopyService() *HostCopyService {
	return &HostCopyService{}
}

func (s *HostCopyService) Copy(_ context.Context, principal identity.Principal, sourceHostID string,
	command HostCopyCommand, idempotencyKey string) error {
	if s == nil {
		return domain.NewError(domain.CodeWorkspaceUnavailable, "Host copy service is unavailable")
	}
	if principal.Kind != identity.KindSession {
		return domain.NewError(domain.CodeForbidden, "Host copies require a Portal session")
	}
	if err := validatePrincipal(principal); err != nil {
		return err
	}
	command.Name = strings.TrimSpace(command.Name)
	if !domain.ValidHostID(sourceHostID) || command.Name == "" || utf8.RuneCountInString(command.Name) > 100 || !utf8.ValidString(command.Name) ||
		command.ExpectedCurrentRevision < 0 || validateIdempotencyKey(idempotencyKey) != nil {
		return domain.InvalidArgument("host_copy", "Host copy request is invalid")
	}
	return domain.NewError(domain.CodeWorkspaceConflict, "independent Host copies are unavailable because cloud computers share the owner's Agent Workspace")
}
