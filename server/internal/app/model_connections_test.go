package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type modelConnectionStoreStub struct {
	values map[string]ModelConnectionRecord
}

func (s *modelConnectionStoreStub) CreateModelConnection(_ context.Context, value ModelConnectionRecord) (ModelConnectionRecord, error) {
	for _, existing := range s.values {
		if existing.OwnerID == value.OwnerID && strings.EqualFold(existing.Name, value.Name) {
			return ModelConnectionRecord{}, domain.NewError(domain.CodeConflict, "model connection name already exists")
		}
	}
	value.CreatedAt = time.Now().UTC()
	value.UpdatedAt = value.CreatedAt
	s.values[value.ID] = cloneModelConnection(value)
	return cloneModelConnection(value), nil
}

func (s *modelConnectionStoreStub) ListModelConnections(_ context.Context, ownerID string) ([]ModelConnectionRecord, error) {
	var result []ModelConnectionRecord
	for _, value := range s.values {
		if value.OwnerID == "" || value.OwnerID == ownerID {
			result = append(result, cloneModelConnection(value))
		}
	}
	return result, nil
}

func (s *modelConnectionStoreStub) GetModelConnection(_ context.Context, id string) (ModelConnectionRecord, error) {
	value, ok := s.values[id]
	if !ok {
		return ModelConnectionRecord{}, domain.NewError(domain.CodeNotFound, "model connection not found")
	}
	return cloneModelConnection(value), nil
}

func (s *modelConnectionStoreStub) UpdateModelConnection(_ context.Context, value ModelConnectionRecord) (ModelConnectionRecord, error) {
	if _, ok := s.values[value.ID]; !ok {
		return ModelConnectionRecord{}, domain.NewError(domain.CodeNotFound, "model connection not found")
	}
	value.UpdatedAt = time.Now().UTC()
	s.values[value.ID] = cloneModelConnection(value)
	return cloneModelConnection(value), nil
}

func (s *modelConnectionStoreStub) DeleteModelConnection(_ context.Context, id string) error {
	if _, ok := s.values[id]; !ok {
		return domain.NewError(domain.CodeNotFound, "model connection not found")
	}
	delete(s.values, id)
	return nil
}

type modelClientStub struct {
	capabilities ModelCapabilities
	endpoint     ModelEndpoint
	err          error
}

func (c *modelClientStub) Complete(context.Context, ModelRequest) (ModelResponse, error) {
	return ModelResponse{}, errors.New("not used")
}

func (c *modelClientStub) Check(_ context.Context, endpoint ModelEndpoint) (ModelCapabilities, error) {
	c.endpoint = endpoint
	return c.capabilities, c.err
}

func modelConnectionTestService(t *testing.T, store *modelConnectionStoreStub, client *modelClientStub) *ModelConnectionService {
	t.Helper()
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewModelConnectionService(store, sealer, client, []string{"api.openai.com"})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestModelConnectionCreateCheckAndResolveKeepsSecretPrivate(t *testing.T) {
	store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
	client := &modelClientStub{capabilities: ModelCapabilities{Text: true, Tools: true, Vision: true}}
	service := modelConnectionTestService(t, store, client)
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	created, err := service.Create(context.Background(), principal, ModelConnectionCommand{
		Name: "personal", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test", APIKey: "secret-value",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Shared || !created.HasAPIKey || strings.Contains(strings.ToLower(strings.Join([]string{created.Name, created.BaseURL, created.ModelName}, " ")), "secret-value") {
		t.Fatalf("unsafe create response: %+v", created)
	}
	stored := store.values[created.ID]
	if string(stored.APIKeyCipher) == "secret-value" || stored.CheckedAt != nil {
		t.Fatal("API key was stored in plaintext or connection was marked checked before probing")
	}
	configured, err := service.IsConfiguredModel(context.Background(), principal.UserID, created.ID)
	if err != nil || configured {
		t.Fatalf("unverified model configured = %t, err=%v", configured, err)
	}
	capabilities, err := service.Check(context.Background(), principal, created.ID)
	if err != nil || !capabilities.Text || !capabilities.Tools || !capabilities.Vision {
		t.Fatalf("Check() = %+v, %v", capabilities, err)
	}
	if client.endpoint.APIKey != "secret-value" || client.endpoint.ModelID != "gpt-test" {
		t.Fatalf("check endpoint = %+v", client.endpoint)
	}
	configured, err = service.IsConfiguredModel(context.Background(), principal.UserID, created.ID)
	if err != nil || !configured {
		t.Fatalf("checked model configured = %t, err=%v", configured, err)
	}
	resolved, err := service.ResolveEndpoint(context.Background(), principal.UserID, created.ID)
	if err != nil || resolved.APIKey != "secret-value" || resolved.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("ResolveEndpoint() = %+v, %v", resolved, err)
	}
}

func TestCheckedTextToolModelWithoutVisionCanRunNonVisualTasks(t *testing.T) {
	store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
	client := &modelClientStub{capabilities: ModelCapabilities{Text: true, Tools: true, Vision: false}}
	service := modelConnectionTestService(t, store, client)
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	created, err := service.Create(context.Background(), principal, ModelConnectionCommand{
		Name: "text and tools", BaseURL: "https://api.openai.com/v1", ModelName: "text-tool-model", APIKey: "secret-value",
	})
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := service.Check(context.Background(), principal, created.ID)
	if err != nil || !capabilities.Text || !capabilities.Tools || capabilities.Vision {
		t.Fatalf("Check() = %+v, %v", capabilities, err)
	}
	configured, err := service.IsConfiguredModel(context.Background(), principal.UserID, created.ID)
	if err != nil || !configured {
		t.Fatalf("no-vision model configured = %t, err=%v", configured, err)
	}
	endpoint, err := service.ResolveEndpoint(context.Background(), principal.UserID, created.ID)
	if err != nil || endpoint.APIKey != "secret-value" || endpoint.ModelID != "text-tool-model" {
		t.Fatalf("ResolveEndpoint() = %+v, %v", endpoint, err)
	}
}

func TestModelConnectionScopeSeparatesPersonalAndSharedManagement(t *testing.T) {
	store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
	client := &modelClientStub{capabilities: ModelCapabilities{Text: true, Tools: true, Vision: true}}
	service := modelConnectionTestService(t, store, client)
	user := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	admin := identity.Principal{UserID: "u7654321", Role: identity.RoleAdmin, Kind: identity.KindSession}
	if _, err := service.Create(context.Background(), user, ModelConnectionCommand{
		Name: "shared", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test", APIKey: "secret-value", Shared: true,
	}); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("user shared create error = %v", err)
	}
	shared, err := service.Create(context.Background(), admin, ModelConnectionCommand{
		Name: "shared", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test", APIKey: "secret-value", Shared: true,
	})
	if err != nil || !shared.Shared {
		t.Fatalf("admin shared create = %+v, %v", shared, err)
	}
	list, err := service.List(context.Background(), user)
	if err != nil || len(list) != 1 || list[0].ID != shared.ID || list[0].HasAPIKey != true {
		t.Fatalf("shared model visibility = %+v, %v", list, err)
	}
	if _, err := service.Update(context.Background(), user, shared.ID, ModelConnectionCommand{
		Name: "takeover", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test",
	}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("user shared update error = %v", err)
	}
	if _, err := service.ResolveEndpoint(context.Background(), user.UserID, shared.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("unchecked shared connection resolve error = %v", err)
	}
	if err := validateModelPrincipal(identity.Principal{UserID: user.UserID, Role: user.Role, Kind: identity.KindAPIKey}); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("API key was allowed to manage model connections: %v", err)
	}
}

func TestModelConnectionRejectsUnapprovedEndpointsAndResetsChecks(t *testing.T) {
	store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
	client := &modelClientStub{capabilities: ModelCapabilities{Text: true, Tools: true, Vision: true}}
	service := modelConnectionTestService(t, store, client)
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	for _, endpoint := range []string{
		"http://api.openai.com/v1", "https://evil.example/v1", "https://127.0.0.1/v1",
		"https://user:pass@api.openai.com/v1", "https://api.openai.com/v1?token=secret", "https://api.openai.com:8443/v1",
		"https://api.openai.com:/v1", "https://api.openai.com/v1/../private", "https://api.openai.com.",
		"https://api.openai.com/v1/" + strings.Repeat("a", 2050),
	} {
		_, err := service.Create(context.Background(), principal, ModelConnectionCommand{Name: "invalid", BaseURL: endpoint, ModelName: "gpt-test", APIKey: "secret-value"})
		if err == nil {
			t.Errorf("Create accepted endpoint %q", endpoint)
		}
	}
	created, err := service.Create(context.Background(), principal, ModelConnectionCommand{
		Name: "personal", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test", APIKey: "secret-value",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Check(context.Background(), principal, created.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := service.Update(context.Background(), principal, created.ID, ModelConnectionCommand{
		Name: "personal", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-new",
	})
	if err != nil || updated.CheckedAt != nil || updated.Capabilities.Text {
		t.Fatalf("model update retained stale capabilities: %+v, %v", updated, err)
	}
}

func TestModelConnectionRejectsSingleLabelAllowlist(t *testing.T) {
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
	client := &modelClientStub{}
	if _, err := NewModelConnectionService(store, sealer, client, []string{"metadata"}); err == nil {
		t.Fatal("single-label allowlist host was accepted")
	}
}

func TestModelConnectionCheckClassifiesProviderErrorWithoutSecrets(t *testing.T) {
	store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
	client := &modelClientStub{err: &ModelClientError{Kind: ModelErrorAuthentication}}
	service := modelConnectionTestService(t, store, client)
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	created, err := service.Create(context.Background(), principal, ModelConnectionCommand{
		Name: "personal", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test", APIKey: "secret-value",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Check(context.Background(), principal, created.ID)
	var appErr *domain.Error
	if !errors.As(err, &appErr) || appErr.Code != domain.CodeUpstreamError || appErr.Details["reason"] != string(ModelErrorAuthentication) || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("provider error = %#v", err)
	}
}

func cloneModelConnection(value ModelConnectionRecord) ModelConnectionRecord {
	value.APIKeyCipher = append([]byte(nil), value.APIKeyCipher...)
	if value.CheckedAt != nil {
		checked := *value.CheckedAt
		value.CheckedAt = &checked
	}
	return value
}
