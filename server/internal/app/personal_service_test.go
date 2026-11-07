package app

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

type personalMutationStoreStub struct {
	calls         int
	readCalls     int
	principal     identity.Principal
	mutation      personal.Mutation
	receipt       personal.MutationReceipt
	readPrincipal identity.Principal
	objectType    string
	objectID      string
	object        personal.Object
	objects       map[string]personal.Object
	readErr       error
	err           error
}

func (s *personalMutationStoreStub) ApplyPersonalMutation(_ context.Context, principal identity.Principal, mutation personal.Mutation) (personal.MutationReceipt, error) {
	s.calls++
	s.principal, s.mutation = principal, mutation
	return s.receipt, s.err
}

func (s *personalMutationStoreStub) GetPersonalObject(_ context.Context, principal identity.Principal, objectType, objectID string) (personal.Object, error) {
	s.readCalls++
	s.readPrincipal, s.objectType, s.objectID = principal, objectType, objectID
	if s.objects != nil {
		object, ok := s.objects[objectType+"/"+objectID]
		if !ok {
			return personal.Object{}, domain.NewError(domain.CodeNotFound, "personal object not found")
		}
		return object, s.readErr
	}
	return s.object, s.readErr
}

func TestPersonalMutationServiceDerivesOwnerAndReturnsReceipt(t *testing.T) {
	store := &personalMutationStoreStub{receipt: personal.MutationReceipt{
		ObjectType: personal.ObjectTypeProfile, ObjectID: personal.ProfileObjectID, ResultRevision: 7,
		AccountChangeSequence: 22, Replay: true,
	}}
	service, err := NewPersonalMutationService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: "t1234567", ClientInstallationID: "i1234567",
	}
	command := PersonalMutationCommand{
		MutationID: "mutation-123", ObjectType: personal.ObjectTypeProfile, ObjectID: personal.ProfileObjectID,
		BaseRevision: 6, Body: []byte(`{"name":"Muse","avatar":"dragon"}`),
	}
	receipt, err := service.ApplyPersonalMutation(context.Background(), principal, command)
	if err != nil {
		t.Fatal(err)
	}
	if store.calls != 1 || !reflect.DeepEqual(store.principal, principal) || store.mutation.OwnerID != principal.UserID ||
		store.mutation.MutationID != command.MutationID || store.mutation.ObjectType != command.ObjectType ||
		store.mutation.ObjectID != command.ObjectID || store.mutation.BaseRevision != command.BaseRevision ||
		string(store.mutation.Body) != string(command.Body) || len(store.mutation.RequestHash) != 32 {
		t.Fatalf("store received principal=%+v mutation=%+v", store.principal, store.mutation)
	}
	if receipt != store.receipt {
		t.Fatalf("receipt = %+v, want %+v", receipt, store.receipt)
	}
}

func TestPersonalMutationServiceAcceptsSessionAndRejectsInvalidPrincipals(t *testing.T) {
	command := PersonalMutationCommand{
		MutationID: "mutation-123", ObjectType: personal.ObjectTypeProfile, ObjectID: personal.ProfileObjectID,
		Body: []byte(`{"name":"Muse","avatar":"dragon"}`),
	}
	store := &personalMutationStoreStub{}
	service, err := NewPersonalMutationService(store)
	if err != nil {
		t.Fatal(err)
	}
	session := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	if _, err := service.ApplyPersonalMutation(context.Background(), session, command); err != nil {
		t.Fatalf("valid session rejected: %v", err)
	}
	for _, principal := range []identity.Principal{
		{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey},
		{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindDevice},
		{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindHostCookie, HostScope: "h1234567"},
		{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, HostScope: "h1234567"},
		{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient},
		{UserID: "invalid", Role: identity.RoleUser, Kind: identity.KindSession},
	} {
		if _, err := service.ApplyPersonalMutation(context.Background(), principal, command); err == nil {
			t.Errorf("invalid principal was accepted: %+v", principal)
		}
	}
	if store.calls != 1 {
		t.Fatalf("store calls = %d, want only the valid session call", store.calls)
	}
}

func TestPersonalMutationServiceValidatesBeforeStore(t *testing.T) {
	store := &personalMutationStoreStub{}
	service, err := NewPersonalMutationService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	for _, test := range []struct {
		command PersonalMutationCommand
		code    domain.ErrorCode
	}{
		{PersonalMutationCommand{MutationID: "short", ObjectType: personal.ObjectTypeProfile, ObjectID: personal.ProfileObjectID, Body: []byte(`{"name":"Muse","avatar":"dragon"}`)}, domain.CodeInvalidArgument},
		{PersonalMutationCommand{MutationID: "mutation-123", ObjectType: personal.ObjectTypeProfile, ObjectID: personal.ProfileObjectID, Body: []byte(`[]`)}, domain.CodeInvalidArgument},
	} {
		if _, err := service.ApplyPersonalMutation(context.Background(), principal, test.command); domain.CodeOf(err) != test.code {
			t.Errorf("invalid command error = %v, want %q", err, test.code)
		}
	}
	if store.calls != 0 {
		t.Fatalf("invalid command reached store %d times", store.calls)
	}
}

func TestPersonalMutationServiceAcceptsGoalObject(t *testing.T) {
	store := &personalMutationStoreStub{}
	service, err := NewPersonalMutationService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	command := PersonalMutationCommand{
		MutationID: "goal-create-1", ObjectType: personal.ObjectTypeGoal, ObjectID: "g_1",
		Body: []byte(`{"title":"Learn Go","status":"active","steps":[]}`),
	}
	if _, err := service.ApplyPersonalMutation(context.Background(), principal, command); err != nil {
		t.Fatalf("Goal mutation rejected: %v", err)
	}
	if store.calls != 1 || store.mutation.OwnerID != principal.UserID || store.mutation.ObjectType != personal.ObjectTypeGoal || store.mutation.ObjectID != "g_1" {
		t.Fatalf("store received %d calls and mutation %+v", store.calls, store.mutation)
	}
}

func TestPersonalMutationServiceAcceptsMemoryObject(t *testing.T) {
	store := &personalMutationStoreStub{}
	service, err := NewPersonalMutationService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	command := PersonalMutationCommand{
		MutationID: "memory-write-1", ObjectType: personal.ObjectTypeMemory, ObjectID: "global",
		Body: []byte(`{"content":"A user-selected memory."}`),
	}
	if _, err := service.ApplyPersonalMutation(context.Background(), principal, command); err != nil {
		t.Fatalf("memory mutation rejected: %v", err)
	}
	if store.calls != 1 || store.mutation.ObjectType != personal.ObjectTypeMemory || store.mutation.ObjectID != "global" {
		t.Fatalf("store received %d calls and mutation %+v", store.calls, store.mutation)
	}
}

func TestPersonalMutationReceiptDoesNotContainBody(t *testing.T) {
	encoded, err := json.Marshal(personal.MutationReceipt{
		ObjectType: personal.ObjectTypeProfile, ObjectID: personal.ProfileObjectID, ResultRevision: 7, AccountChangeSequence: 22,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" || !json.Valid(encoded) {
		t.Fatalf("invalid receipt JSON: %s", encoded)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["Body"]; exists {
		t.Fatalf("receipt exposes mutation body: %s", encoded)
	}
	if _, exists := fields["body"]; exists {
		t.Fatalf("receipt exposes mutation body: %s", encoded)
	}
}

func TestPersonalServiceGetPersonalObjectForwardsProfileAndPrincipal(t *testing.T) {
	want := personal.Object{
		ObjectType: personal.ObjectTypeProfile, ObjectID: personal.ProfileObjectID,
		Revision: 7, Body: json.RawMessage(`{"name":"Muse","avatar":"dragon"}`),
	}
	store := &personalMutationStoreStub{object: want}
	service, err := NewPersonalMutationService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	got, err := service.GetPersonalObject(context.Background(), principal, personal.ObjectTypeProfile, personal.ProfileObjectID)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("GetPersonalObject() = %+v, %v; want %+v", got, err, want)
	}
	if store.readCalls != 1 || !reflect.DeepEqual(store.readPrincipal, principal) ||
		store.objectType != personal.ObjectTypeProfile || store.objectID != personal.ProfileObjectID {
		t.Fatalf("store read call = calls %d, principal %+v, type %q, id %q", store.readCalls, store.readPrincipal, store.objectType, store.objectID)
	}
}

func TestPersonalServiceGetPersonalObjectAcceptsMemoryObject(t *testing.T) {
	store := &personalMutationStoreStub{}
	service, err := NewPersonalMutationService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	if _, err := service.GetPersonalObject(context.Background(), principal, personal.ObjectTypeMemory, "global"); err != nil {
		t.Fatalf("memory read rejected: %v", err)
	}
	if store.readCalls != 1 || store.objectType != personal.ObjectTypeMemory || store.objectID != "global" {
		t.Fatalf("store read = %d calls for %s/%s", store.readCalls, store.objectType, store.objectID)
	}
}

func TestPersonalServiceGetPersonalObjectAcceptsGoal(t *testing.T) {
	store := &personalMutationStoreStub{}
	service, err := NewPersonalMutationService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	if _, err := service.GetPersonalObject(context.Background(), principal, personal.ObjectTypeGoal, "g_1"); err != nil {
		t.Fatalf("Goal read rejected: %v", err)
	}
	if store.readCalls != 1 || store.objectType != personal.ObjectTypeGoal || store.objectID != "g_1" {
		t.Fatalf("store read = %d calls for %s/%s", store.readCalls, store.objectType, store.objectID)
	}
}

func TestPersonalServiceGetPersonalObjectRejectsUnsupportedOrInvalidInput(t *testing.T) {
	store := &personalMutationStoreStub{}
	service, err := NewPersonalMutationService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	for _, test := range []struct {
		objectType string
		objectID   string
		wantCode   domain.ErrorCode
	}{
		{objectType: "unknown", objectID: personal.ProfileObjectID, wantCode: domain.CodeUnsupported},
		{objectType: personal.ObjectTypeProfile, objectID: "other", wantCode: domain.CodeInvalidArgument},
	} {
		if _, err := service.GetPersonalObject(context.Background(), principal, test.objectType, test.objectID); domain.CodeOf(err) != test.wantCode {
			t.Errorf("GetPersonalObject(%q, %q) error = %v, want %q", test.objectType, test.objectID, err, test.wantCode)
		}
	}
	for _, denied := range []identity.Principal{
		{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey},
		{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindDevice},
		{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient},
		{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, HostScope: "h1234567"},
	} {
		if _, err := service.GetPersonalObject(context.Background(), denied, personal.ObjectTypeProfile, personal.ProfileObjectID); err == nil {
			t.Errorf("GetPersonalObject accepted principal %+v", denied)
		}
	}
	if store.readCalls != 0 {
		t.Fatalf("invalid read reached store %d times", store.readCalls)
	}
}
