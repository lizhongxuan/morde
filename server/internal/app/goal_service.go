package app

import (
	"context"
	"encoding/json"
	"errors"

	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

type GoalPage struct {
	Items      []personal.Object
	NextCursor string
}

type goalMutationUseCase interface {
	GetPersonalObject(context.Context, identity.Principal, string, string) (personal.Object, error)
	ApplyPersonalMutation(context.Context, identity.Principal, PersonalMutationCommand) (personal.MutationReceipt, error)
}

type goalListStore interface {
	ListPersonalGoals(context.Context, identity.Principal, PageRequest) (GoalPage, error)
}

type GoalService struct {
	mutations goalMutationUseCase
	store     goalListStore
}

func NewGoalService(mutations goalMutationUseCase, store goalListStore) (*GoalService, error) {
	if mutations == nil || store == nil {
		return nil, errors.New("goal dependencies are required")
	}
	return &GoalService{mutations: mutations, store: store}, nil
}

func (s *GoalService) List(ctx context.Context, principal identity.Principal, page PageRequest) (GoalPage, error) {
	if err := validatePersonalSyncPrincipal(principal); err != nil {
		return GoalPage{}, err
	}
	if err := validatePageRequest(page); err != nil {
		return GoalPage{}, err
	}
	if page.Limit == 0 {
		page.Limit = defaultPageSize
	}
	return s.store.ListPersonalGoals(ctx, principal, page)
}

func (s *GoalService) Get(ctx context.Context, principal identity.Principal, id string) (personal.Object, error) {
	return s.mutations.GetPersonalObject(ctx, principal, personal.ObjectTypeGoal, id)
}

func (s *GoalService) Create(ctx context.Context, principal identity.Principal, id string, body json.RawMessage, mutationID string) (personal.MutationReceipt, error) {
	return s.mutations.ApplyPersonalMutation(ctx, principal, PersonalMutationCommand{
		MutationID: mutationID, ObjectType: personal.ObjectTypeGoal, ObjectID: id,
		BaseRevision: 0, Body: body,
	})
}

func (s *GoalService) Patch(ctx context.Context, principal identity.Principal, id string, baseRevision int64, body json.RawMessage, mutationID string) (personal.MutationReceipt, error) {
	return s.mutations.ApplyPersonalMutation(ctx, principal, PersonalMutationCommand{
		MutationID: mutationID, ObjectType: personal.ObjectTypeGoal, ObjectID: id,
		BaseRevision: baseRevision, Body: body,
	})
}

func (s *GoalService) Delete(ctx context.Context, principal identity.Principal, id string, baseRevision int64, mutationID string) (personal.MutationReceipt, error) {
	return s.mutations.ApplyPersonalMutation(ctx, principal, PersonalMutationCommand{
		MutationID: mutationID, ObjectType: personal.ObjectTypeGoal, ObjectID: id,
		BaseRevision: baseRevision, Delete: true,
	})
}
