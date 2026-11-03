package app

import (
	"context"
	"encoding/json"
	"errors"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

type PersonalMutationCommand struct {
	MutationID   string
	ObjectType   string
	ObjectID     string
	BaseRevision int64
	Body         json.RawMessage
	Delete       bool
}

type PersonalMutationStore interface {
	ApplyPersonalMutation(context.Context, identity.Principal, personal.Mutation) (personal.MutationReceipt, error)
	GetPersonalObject(context.Context, identity.Principal, string, string) (personal.Object, error)
}

type PersonalMutationService struct {
	store PersonalMutationStore
}

func NewPersonalMutationService(store PersonalMutationStore) (*PersonalMutationService, error) {
	if store == nil {
		return nil, errors.New("personal mutation store is required")
	}
	return &PersonalMutationService{store: store}, nil
}

func (s *PersonalMutationService) ApplyPersonalMutation(ctx context.Context, principal identity.Principal, command PersonalMutationCommand) (personal.MutationReceipt, error) {
	mutation, err := s.PreparePersonalMutation(principal, command)
	if err != nil {
		return personal.MutationReceipt{}, err
	}
	return s.store.ApplyPersonalMutation(ctx, principal, mutation)
}

// PreparePersonalMutation applies the same identity, content, and idempotency
// checks as ApplyPersonalMutation before a caller commits the mutation with
// another durable operation in its own transaction.
func (s *PersonalMutationService) PreparePersonalMutation(principal identity.Principal, command PersonalMutationCommand) (personal.Mutation, error) {
	if principal.Kind != identity.KindSession && principal.Kind != identity.KindClient {
		return personal.Mutation{}, domain.NewError(domain.CodeForbidden, "portal session required")
	}
	if err := validatePrincipal(principal); err != nil {
		return personal.Mutation{}, err
	}
	mutation := personal.Mutation{
		OwnerID: principal.UserID, MutationID: command.MutationID, ObjectType: command.ObjectType,
		ObjectID: command.ObjectID, BaseRevision: command.BaseRevision,
		Body: append(json.RawMessage(nil), command.Body...), Delete: command.Delete,
	}
	if err := personal.ValidateMutation(mutation); err != nil {
		return personal.Mutation{}, err
	}
	requestHash, err := personal.HashMutationContent(mutation)
	if err != nil {
		return personal.Mutation{}, err
	}
	mutation.RequestHash = requestHash
	return mutation, nil
}

func (s *PersonalMutationService) GetPersonalObject(ctx context.Context, principal identity.Principal, objectType, objectID string) (personal.Object, error) {
	if principal.Kind != identity.KindSession && principal.Kind != identity.KindClient {
		return personal.Object{}, domain.NewError(domain.CodeForbidden, "portal session required")
	}
	if err := validatePrincipal(principal); err != nil {
		return personal.Object{}, err
	}
	if err := personal.ValidateObjectIdentifier(objectType, objectID); err != nil {
		return personal.Object{}, err
	}
	return s.store.GetPersonalObject(ctx, principal, objectType, objectID)
}
