package app

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type taskCapabilityResolver struct {
	owner, model string
	toolCalls    int
	textCalls    int
	toolError    error
	textError    error
}

func (r *taskCapabilityResolver) ResolveGatewayEndpoint(_ context.Context, owner, model string, tools, _ bool) (ModelGatewayEndpoint, error) {
	r.owner, r.model = owner, model
	if tools {
		r.toolCalls++
		return ModelGatewayEndpoint{}, r.toolError
	}
	r.textCalls++
	return ModelGatewayEndpoint{}, r.textError
}

func (*taskCapabilityResolver) ResolveProbeEndpoint(context.Context, identity.Principal, string) (ModelGatewayEndpoint, error) {
	return ModelGatewayEndpoint{}, nil
}

func (*taskCapabilityResolver) SaveProbeCapabilities(_ context.Context, _ identity.Principal, _ string, _ time.Time, value ModelCapabilities) (ModelCapabilities, error) {
	return value, nil
}

func TestTaskToolCapabilityUsesInitiatorAndOnlyFallsBackForTextModel(t *testing.T) {
	for _, tc := range []struct {
		name      string
		toolError error
		textError error
		wantTools bool
		wantCode  domain.ErrorCode
		textCalls int
	}{
		{name: "tool model", wantTools: true},
		{name: "text model", toolError: domain.NewError(domain.CodeUnsupported, "no tools"), textCalls: 1},
		{name: "missing model", toolError: domain.NewError(domain.CodeNotFound, "missing"), wantCode: domain.CodeNotFound},
		{name: "fallback still unauthorized", toolError: domain.NewError(domain.CodeUnsupported, "no tools"), textError: domain.NewError(domain.CodeNotFound, "missing"), wantCode: domain.CodeNotFound, textCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolver := &taskCapabilityResolver{toolError: tc.toolError, textError: tc.textError}
			service, err := NewModelGatewayService(&modelGatewayStoreTest{}, resolver, &modelGatewayClientTest{})
			if err != nil {
				t.Fatal(err)
			}
			tools, err := service.TaskToolCapability(context.Background(), task.Task{OwnerID: "u1234567", InitiatorID: "u7654321", ModelID: "p1234567"})
			if tools != tc.wantTools || (tc.wantCode == "" && err != nil) || (tc.wantCode != "" && domain.CodeOf(err) != tc.wantCode) || resolver.owner != "u7654321" ||
				resolver.model != "p1234567" || resolver.toolCalls != 1 || resolver.textCalls != tc.textCalls {
				t.Fatalf("tools=%v err=%v resolver=%+v", tools, err, resolver)
			}
		})
	}
}
