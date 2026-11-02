package app

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

type goalMutationStub struct {
	principal     identity.Principal
	objectType    string
	objectID      string
	command       PersonalMutationCommand
	object        personal.Object
	receipt       personal.MutationReceipt
	getCalls      int
	mutationCalls int
	err           error
}

func (s *goalMutationStub) GetPersonalObject(_ context.Context, principal identity.Principal, objectType, objectID string) (personal.Object, error) {
	s.getCalls++
	s.principal, s.objectType, s.objectID = principal, objectType, objectID
	return s.object, s.err
}

func (s *goalMutationStub) ApplyPersonalMutation(_ context.Context, principal identity.Principal, command PersonalMutationCommand) (personal.MutationReceipt, error) {
	s.mutationCalls++
	s.principal, s.command = principal, command
	return s.receipt, s.err
}

type goalListStub struct {
	principal identity.Principal
	page      PageRequest
	result    GoalPage
	calls     int
	err       error
}

func (s *goalListStub) ListPersonalGoals(_ context.Context, principal identity.Principal, page PageRequest) (GoalPage, error) {
	s.calls++
	s.principal, s.page = principal, page
	return s.result, s.err
}

func TestGoalServiceForwardsGoalObjectOperationsThroughPersonalMutations(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	body := json.RawMessage(`{"title":"Learn Go","status":"active","steps":[]}`)
	mutation := &goalMutationStub{receipt: personal.MutationReceipt{ObjectType: personal.ObjectTypeGoal, ObjectID: "g_1", ResultRevision: 1}}
	store := &goalListStub{}
	service, err := NewGoalService(mutation, store)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.Create(context.Background(), principal, "g_1", body, "goal-create-1"); err != nil {
		t.Fatal(err)
	}
	want := PersonalMutationCommand{MutationID: "goal-create-1", ObjectType: personal.ObjectTypeGoal, ObjectID: "g_1", BaseRevision: 0, Body: body}
	if mutation.mutationCalls != 1 || !reflect.DeepEqual(mutation.principal, principal) || !reflect.DeepEqual(mutation.command, want) {
		t.Fatalf("create forwarded principal=%+v command=%+v calls=%d; want %+v", mutation.principal, mutation.command, mutation.mutationCalls, want)
	}

	if _, err := service.Patch(context.Background(), principal, "g_1", 1, body, "goal-patch-1"); err != nil {
		t.Fatal(err)
	}
	if mutation.command.MutationID != "goal-patch-1" || mutation.command.ObjectType != personal.ObjectTypeGoal || mutation.command.ObjectID != "g_1" ||
		mutation.command.BaseRevision != 1 || !reflect.DeepEqual(mutation.command.Body, body) || mutation.command.Delete {
		t.Fatalf("patch command = %+v", mutation.command)
	}

	if _, err := service.Delete(context.Background(), principal, "g_1", 2, "goal-delete-1"); err != nil {
		t.Fatal(err)
	}
	if mutation.command.MutationID != "goal-delete-1" || mutation.command.ObjectType != personal.ObjectTypeGoal || mutation.command.ObjectID != "g_1" ||
		mutation.command.BaseRevision != 2 || !mutation.command.Delete || len(mutation.command.Body) != 0 {
		t.Fatalf("delete command = %+v", mutation.command)
	}

	wantObject := personal.Object{ObjectType: personal.ObjectTypeGoal, ObjectID: "g_1", Revision: 2, Body: body}
	mutation.object = wantObject
	got, err := service.Get(context.Background(), principal, "g_1")
	if err != nil || !reflect.DeepEqual(got, wantObject) || mutation.getCalls != 1 || !reflect.DeepEqual(mutation.principal, principal) ||
		mutation.objectType != personal.ObjectTypeGoal || mutation.objectID != "g_1" {
		t.Fatalf("get = %+v, %v; forwarded principal=%+v type=%q id=%q calls=%d", got, err, mutation.principal, mutation.objectType, mutation.objectID, mutation.getCalls)
	}
}

func TestGoalServiceListDefaultsLimitAndUsesPrincipalOwner(t *testing.T) {
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	store := &goalListStub{result: GoalPage{Items: []personal.Object{}, NextCursor: "next"}}
	service, err := NewGoalService(&goalMutationStub{}, store)
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.List(context.Background(), principal, PageRequest{Cursor: "opaque-cursor"})
	if err != nil || !reflect.DeepEqual(got, store.result) || store.calls != 1 || !reflect.DeepEqual(store.principal, principal) ||
		store.page != (PageRequest{Limit: defaultPageSize, Cursor: "opaque-cursor"}) {
		t.Fatalf("List = %+v, %v; store call=%d principal=%+v page=%+v", got, err, store.calls, store.principal, store.page)
	}
}

func TestGoalServiceRejectsInvalidListPrincipalAndPage(t *testing.T) {
	store := &goalListStub{}
	service, err := NewGoalService(&goalMutationStub{}, store)
	if err != nil {
		t.Fatal(err)
	}
	valid := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	for _, test := range []struct {
		name      string
		principal identity.Principal
		page      PageRequest
	}{
		{name: "api key", principal: identity.Principal{UserID: valid.UserID, Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: valid.TokenID}},
		{name: "invalid owner", principal: identity.Principal{UserID: "bad", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: valid.TokenID}},
		{name: "bad limit", principal: valid, page: PageRequest{Limit: maxPageSize + 1}},
		{name: "long cursor", principal: valid, page: PageRequest{Cursor: string(make([]byte, 1025))}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := service.List(context.Background(), test.principal, test.page); err == nil {
				t.Fatal("invalid list request was accepted")
			}
		})
	}
	if store.calls != 0 {
		t.Fatalf("invalid list requests reached store %d times", store.calls)
	}
}
