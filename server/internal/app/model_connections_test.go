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
	values         map[string]ModelConnectionRecord
	defaultModelID string
	defaultVersion int64
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

func (s *modelConnectionStoreStub) GetAdminModelDefault(context.Context) (AdminModelDefaultRecord, error) {
	return AdminModelDefaultRecord{ModelID: s.defaultModelID, Version: s.defaultVersion, UpdatedAt: time.Now().UTC()}, nil
}

func (s *modelConnectionStoreStub) SetAdminModelDefault(_ context.Context, _ identity.Principal, expectedVersion int64, modelID, _ string) (AdminModelDefaultRecord, error) {
	if expectedVersion != s.defaultVersion {
		return AdminModelDefaultRecord{}, domain.NewError(domain.CodeConflict, "default model version changed")
	}
	if modelID != "" {
		record, ok := s.values[modelID]
		if !ok || record.OwnerID != "" {
			return AdminModelDefaultRecord{}, domain.NewError(domain.CodeNotFound, "model connection not found")
		}
		if record.CheckedAt == nil || !record.Capabilities.Text {
			return AdminModelDefaultRecord{}, domain.NewError(domain.CodeUnsupported, "model is not an available default")
		}
	}
	s.defaultModelID = modelID
	s.defaultVersion++
	return AdminModelDefaultRecord{ModelID: s.defaultModelID, Version: s.defaultVersion, UpdatedAt: time.Now().UTC()}, nil
}

type modelClientStub struct {
	capabilities  ModelCapabilities
	endpoint      ModelEndpoint
	err           error
	completeCalls int
}

func (c *modelClientStub) Complete(_ context.Context, request ModelRequest) (ModelResponse, error) {
	c.completeCalls++
	c.endpoint = request.Endpoint
	if c.err != nil {
		return ModelResponse{}, c.err
	}
	if request.RequiredTool != "morde_capability_probe" || !c.capabilities.Text || !c.capabilities.Tools || !c.capabilities.Vision {
		return ModelResponse{}, errors.New("probe is unavailable")
	}
	return ModelResponse{Text: "red", ToolCalls: []ModelToolCall{{CallID: "probe-1", Name: request.RequiredTool, Arguments: `{"color":"red"}`}},
		Usage: ModelUsage{Known: true, InputTokens: 3, OutputTokens: 2}}, nil
}

func (c *modelClientStub) Check(_ context.Context, endpoint ModelEndpoint) (ModelCapabilities, error) {
	c.endpoint = endpoint
	return c.capabilities, c.err
}

func modelConnectionTestService(t *testing.T, store *modelConnectionStoreStub, client ModelClient) *ModelConnectionService {
	t.Helper()
	if store.values == nil {
		store.values = make(map[string]ModelConnectionRecord)
	}
	if store.defaultModelID == "" {
		checkedAt := time.Now().UTC()
		store.defaultModelID = "pzzzzzzz"
		store.defaultVersion = 1
		store.values[store.defaultModelID] = ModelConnectionRecord{
			ID: store.defaultModelID, Name: "configured default", BaseURL: "https://api.openai.com/v1", ModelName: "default-model",
			APIKeyCipher: []byte("test-ciphertext"), Capabilities: ModelCapabilities{Text: true, Tools: true},
			CheckedAt: &checkedAt, CreatedAt: checkedAt, UpdatedAt: checkedAt,
		}
	}
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

func checkModelForTest(t *testing.T, service *ModelConnectionService, client ModelClient, principal identity.Principal, id string) (ModelCapabilities, error) {
	t.Helper()
	gateway, err := NewModelGatewayService(&modelGatewayStoreTest{}, service, client)
	if err != nil {
		t.Fatal(err)
	}
	return gateway.CheckModel(context.Background(), principal, id, "probe-key-0001")
}

type modelMediaCapabilityClientStub struct {
	*modelClientStub
	report          ModelMediaGenerationCapabilityReport
	checked         string
	mediaCheckCalls int
	mediaCheckErr   error
}

func (c *modelMediaCapabilityClientStub) CheckMediaGenerationCapability(_ context.Context, endpoint ModelEndpoint, capability string) (ModelMediaGenerationCapabilityReport, error) {
	c.endpoint = endpoint
	c.checked = capability
	c.mediaCheckCalls++
	return c.report, c.mediaCheckErr
}

type modelConnectionStoreDropMediaStub struct {
	*modelConnectionStoreStub
}

func (s *modelConnectionStoreDropMediaStub) UpdateModelConnection(ctx context.Context, value ModelConnectionRecord) (ModelConnectionRecord, error) {
	value.Capabilities.MediaGeneration = ModelMediaGenerationCapabilities{}
	return s.modelConnectionStoreStub.UpdateModelConnection(ctx, value)
}

func TestModelMediaCapabilityCheckIsExplicitAndPersistsThroughModelConnection(t *testing.T) {
	store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
	client := &modelMediaCapabilityClientStub{
		modelClientStub: &modelClientStub{capabilities: ModelCapabilities{Text: true, Tools: true, Vision: true}},
		report: ModelMediaGenerationCapabilityReport{Supported: true, ExactTokenReservation: true, ActualTokenUsage: true,
			IdempotentOperation: true, DurableOperationRecovery: true, ConfirmedCancellation: true},
	}
	models := modelConnectionTestService(t, store, client)
	admin := identity.Principal{UserID: "u7654321", Role: identity.RoleAdmin, Kind: identity.KindSession, TokenID: "t1234567"}
	created, err := models.Create(context.Background(), admin, ModelConnectionCommand{
		Name: "media", BaseURL: "https://api.openai.com/v1", ModelName: "media-test", APIKey: "secret-value", Shared: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := NewModelGatewayService(&modelGatewayStoreTest{}, models, client)
	if err != nil {
		t.Fatal(err)
	}
	for i, probe := range []struct{ capability, key string }{
		{capability: "image_generation", key: "probe-media-0001"},
		{capability: "video_generation", key: "probe-media-0002"},
		{capability: "speech_generation", key: "probe-media-0003"},
	} {
		capabilities, err := gateway.CheckModelCapability(context.Background(), admin, created.ID, probe.key, probe.capability)
		if err != nil || client.checked != probe.capability || client.mediaCheckCalls != i+1 || client.completeCalls != 0 {
			t.Fatalf("explicit media probe %q = %+v, checked=%q calls=%d/%d err=%v", probe.capability, capabilities, client.checked, client.mediaCheckCalls, client.completeCalls, err)
		}
		resolved, err := models.ResolveMediaGenerationEndpoint(context.Background(), admin.UserID, created.ID, probe.capability)
		if err != nil || resolved.Endpoint.APIKey != "secret-value" || resolved.Endpoint.ModelID != "media-test" {
			t.Fatalf("ResolveMediaGenerationEndpoint(%q) = %+v, %v", probe.capability, resolved, err)
		}
	}

	capabilities, err := gateway.CheckModelCapability(context.Background(), admin, created.ID, "probe-text-0001", "all")
	if err != nil || !capabilities.Text || !capabilities.Tools || !capabilities.Vision || !capabilities.MediaGeneration.ImageGeneration ||
		!capabilities.MediaGeneration.VideoGeneration || !capabilities.MediaGeneration.SpeechGeneration || client.mediaCheckCalls != 3 || client.completeCalls != 1 {
		t.Fatalf("all probe altered media state or probed media: %+v, capability calls=%d, complete calls=%d, err=%v", capabilities, client.mediaCheckCalls, client.completeCalls, err)
	}
}

func TestModelMediaCapabilityCheckFailsClosedWithoutProviderAndDurableStorage(t *testing.T) {
	admin := identity.Principal{UserID: "u7654321", Role: identity.RoleAdmin, Kind: identity.KindSession, TokenID: "t1234567"}
	fullReport := ModelMediaGenerationCapabilityReport{Supported: true, ExactTokenReservation: true, ActualTokenUsage: true,
		IdempotentOperation: true, DurableOperationRecovery: true, ConfirmedCancellation: true}
	for _, test := range []struct {
		name      string
		report    ModelMediaGenerationCapabilityReport
		dropMedia bool
	}{
		{name: "provider has no exact reservation", report: ModelMediaGenerationCapabilityReport{Supported: true, ActualTokenUsage: true, IdempotentOperation: true, DurableOperationRecovery: true, ConfirmedCancellation: true}},
		{name: "provider cannot reconcile unknown operations", report: ModelMediaGenerationCapabilityReport{Supported: true, ExactTokenReservation: true, ActualTokenUsage: true, IdempotentOperation: true, ConfirmedCancellation: true}},
		{name: "storage drops media capability", report: fullReport, dropMedia: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := &modelClientStub{}
			client := &modelMediaCapabilityClientStub{modelClientStub: base, report: test.report}
			baseStore := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
			defaultCheckedAt := time.Now().UTC()
			baseStore.defaultModelID, baseStore.defaultVersion = "pzzzzzzz", 1
			baseStore.values[baseStore.defaultModelID] = ModelConnectionRecord{
				ID: baseStore.defaultModelID, Name: "default", BaseURL: "https://api.openai.com/v1", ModelName: "default-model",
				APIKeyCipher: []byte("test-ciphertext"), Capabilities: ModelCapabilities{Text: true, Tools: true},
				CheckedAt: &defaultCheckedAt,
			}
			var store ModelConnectionStore = baseStore
			if test.dropMedia {
				store = &modelConnectionStoreDropMediaStub{modelConnectionStoreStub: baseStore}
			}
			sealer, err := NewSealer(make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			models, err := NewModelConnectionService(store, sealer, client, []string{"api.openai.com"})
			if err != nil {
				t.Fatal(err)
			}
			created, err := models.Create(context.Background(), admin, ModelConnectionCommand{
				Name: "media", BaseURL: "https://api.openai.com/v1", ModelName: "media-test", APIKey: "secret-value", Shared: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			gateway, err := NewModelGatewayService(&modelGatewayStoreTest{}, models, client)
			if err != nil {
				t.Fatal(err)
			}
			_, err = gateway.CheckModelCapability(context.Background(), admin, created.ID, "probe-media-0001", "image_generation")
			if domain.CodeOf(err) != domain.CodeUnsupported || client.completeCalls != 0 {
				t.Fatalf("media probe = %v, regular provider calls=%d", err, client.completeCalls)
			}
			if test.dropMedia {
				if client.mediaCheckCalls != 1 {
					t.Fatalf("media provider check calls = %d", client.mediaCheckCalls)
				}
				if _, err := models.ResolveMediaGenerationEndpoint(context.Background(), admin.UserID, created.ID, "image_generation"); domain.CodeOf(err) != domain.CodeNotFound {
					t.Fatalf("connection whose store dropped media capability resolved: %v", err)
				}
			} else if client.mediaCheckCalls != 1 || baseStore.values[created.ID].CheckedAt != nil {
				t.Fatalf("incomplete provider was marked checked: calls=%d record=%+v", client.mediaCheckCalls, baseStore.values[created.ID])
			}
		})
	}
	base := &modelClientStub{}
	store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
	models := modelConnectionTestService(t, store, base)
	created, err := models.Create(context.Background(), admin, ModelConnectionCommand{
		Name: "legacy", BaseURL: "https://api.openai.com/v1", ModelName: "text-test", APIKey: "secret-value", Shared: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := NewModelGatewayService(&modelGatewayStoreTest{}, models, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.CheckModelCapability(context.Background(), admin, created.ID, "probe-media-0002", "speech_generation"); domain.CodeOf(err) != domain.CodeUnsupported || base.completeCalls != 0 {
		t.Fatalf("provider without metadata-only media contract did not fail closed: calls=%d err=%v", base.completeCalls, err)
	}
}

func TestModelConnectionCreateCheckAndResolveKeepsSecretPrivate(t *testing.T) {
	store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
	client := &modelClientStub{capabilities: ModelCapabilities{Text: true, Tools: true, Vision: true}}
	service := modelConnectionTestService(t, store, client)
	principal := identity.Principal{UserID: "u7654321", Role: identity.RoleAdmin, Kind: identity.KindSession, TokenID: "t1234567"}
	created, err := service.Create(context.Background(), principal, ModelConnectionCommand{
		Name: "platform", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test", APIKey: "secret-value", Shared: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created.Shared || !created.HasAPIKey || strings.Contains(strings.ToLower(strings.Join([]string{created.Name, created.BaseURL, created.ModelName}, " ")), "secret-value") {
		t.Fatalf("unsafe create response: %+v", created)
	}
	stored := store.values[created.ID]
	if string(stored.APIKeyCipher) == "secret-value" || stored.CheckedAt != nil {
		t.Fatal("API key was stored in plaintext or connection was marked checked before probing")
	}
	configured, err := service.IsConfiguredModel(context.Background(), principal.UserID, created.ID)
	if err != nil || !configured {
		t.Fatalf("saved but unchecked model configured = %t, err=%v", configured, err)
	}
	capabilities, err := checkModelForTest(t, service, client, principal, created.ID)
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

func TestUserModelConnectionIsPrivateEncryptedAndRunsCloudTasks(t *testing.T) {
	store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
	client := &modelClientStub{capabilities: ModelCapabilities{Text: true, Tools: true, Vision: true}}
	service := modelConnectionTestService(t, store, client)
	store.defaultModelID = ""
	owner := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: "t1234567", ClientInstallationID: "i1234567"}
	created, err := service.Create(context.Background(), owner, ModelConnectionCommand{
		Name: "my model", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test", APIKey: "secret-value",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Shared || !created.HasAPIKey || store.values[created.ID].OwnerID != owner.UserID ||
		string(store.values[created.ID].APIKeyCipher) == "secret-value" {
		t.Fatalf("user model was shared, plaintext, or not owner-bound: view=%+v stored=%+v", created, store.values[created.ID])
	}
	if _, err := service.Create(context.Background(), owner, ModelConnectionCommand{
		Name: "shared attempt", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test", APIKey: "secret-value", Shared: true,
	}); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("user shared model creation = %v", err)
	}

	items, err := service.List(context.Background(), owner)
	if err != nil || len(items) != 2 || !items[0].Shared || items[1].ID != created.ID || items[1].Shared {
		t.Fatalf("owner list = %+v, %v", items, err)
	}
	other := identity.Principal{UserID: "u7654321", Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: "t7654321", ClientInstallationID: "i7654321"}
	if _, err := service.Get(context.Background(), other, created.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("other account read private model = %v", err)
	}
	if err := service.Delete(context.Background(), other, created.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("other account deleted private model = %v", err)
	}

	capabilities, err := checkModelForTest(t, service, client, owner, created.ID)
	if err != nil || !capabilities.Text || !capabilities.Tools {
		t.Fatalf("client model probe = %+v, %v", capabilities, err)
	}
	configured, err := service.IsConfiguredModel(context.Background(), owner.UserID, created.ID)
	if err != nil || !configured {
		t.Fatalf("owner configured model = %t, %v", configured, err)
	}
	if configured, err := service.IsConfiguredModel(context.Background(), other.UserID, created.ID); err != nil || configured {
		t.Fatalf("other account configured model = %t, %v", configured, err)
	}
	endpoint, err := service.ResolveGatewayEndpoint(context.Background(), owner.UserID, created.ID, true, false)
	if err != nil || endpoint.Endpoint.APIKey != "secret-value" || endpoint.Endpoint.ModelID != "gpt-test" {
		t.Fatalf("owner gateway endpoint = %+v, %v", endpoint, err)
	}
	if _, err := service.ResolveGatewayEndpoint(context.Background(), other.UserID, created.ID, true, false); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("other account resolved private model = %v", err)
	}

	updated, err := service.Update(context.Background(), owner, created.ID, ModelConnectionCommand{
		Name: "renamed", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test",
	})
	if err != nil || updated.Name != "renamed" || updated.Shared {
		t.Fatalf("owner update = %+v, %v", updated, err)
	}
	if err := service.Delete(context.Background(), owner, created.ID); err != nil {
		t.Fatalf("owner delete = %v", err)
	}
}

func TestAdminModelDefaultIsVersionedAndOptional(t *testing.T) {
	store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
	service := modelConnectionTestService(t, store, &modelClientStub{})
	defaults, err := NewAdminModelDefaultService(store)
	if err != nil {
		t.Fatal(err)
	}
	admin := identity.Principal{UserID: "u7654321", Role: identity.RoleAdmin, Kind: identity.KindSession, TokenID: "t7654321"}
	user := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: "t1234567", ClientInstallationID: "i1234567"}
	initial, err := defaults.Get(context.Background(), admin)
	if err != nil || !initial.Configured || initial.ModelID != store.defaultModelID || initial.Version != store.defaultVersion {
		t.Fatalf("admin default get = %+v, %v", initial, err)
	}
	if _, err := defaults.Get(context.Background(), user); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("client read admin default = %v", err)
	}
	availability, err := defaults.Available(context.Background())
	if err != nil || !availability.Configured || availability.ModelID != initial.ModelID {
		t.Fatalf("available model state = %+v, %v", availability, err)
	}
	cleared, err := defaults.Update(context.Background(), admin, initial.Version, "", "192.0.2.1")
	if err != nil || cleared.Configured || cleared.ModelID != "" || cleared.Version != initial.Version+1 {
		t.Fatalf("clear default = %+v, %v", cleared, err)
	}
	availability, err = defaults.Available(context.Background())
	if err != nil || availability.Configured || availability.ModelID != "" {
		t.Fatalf("cleared availability = %+v, %v", availability, err)
	}
	configured, err := service.IsConfiguredModel(context.Background(), user.UserID, initial.ModelID)
	if err != nil || !configured {
		t.Fatalf("checked shared model unavailable after clearing the default: configured=%t err=%v", configured, err)
	}
	if _, err := defaults.Update(context.Background(), admin, initial.Version, initial.ModelID, "192.0.2.1"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale default update = %v", err)
	}
}

func TestAdminDefaultAllowsCheckedTextOnlyModel(t *testing.T) {
	store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
	models := modelConnectionTestService(t, store, &modelClientStub{})
	defaults, err := NewAdminModelDefaultService(store)
	if err != nil {
		t.Fatal(err)
	}
	admin := identity.Principal{UserID: "u7654321", Role: identity.RoleAdmin, Kind: identity.KindSession, TokenID: "t7654321"}
	shared, err := models.Create(context.Background(), admin, ModelConnectionCommand{
		Name: "text only", BaseURL: "https://api.openai.com/v1", ModelName: "text-model", APIKey: "test-key", Shared: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := defaults.Update(context.Background(), admin, store.defaultVersion, shared.ID, "192.0.2.1"); domain.CodeOf(err) != domain.CodeUnsupported {
		t.Fatalf("unchecked model accepted as default: %v", err)
	}
	checked := time.Now().UTC()
	record := store.values[shared.ID]
	record.Capabilities, record.CheckedAt = ModelCapabilities{Text: true}, &checked
	store.values[shared.ID] = record
	updated, err := defaults.Update(context.Background(), admin, store.defaultVersion, shared.ID, "192.0.2.1")
	if err != nil || !updated.Configured || updated.ModelID != shared.ID {
		t.Fatalf("text-only model default = %+v, %v", updated, err)
	}
	availability, err := defaults.Available(context.Background())
	if err != nil || !availability.Configured || availability.ModelID != shared.ID {
		t.Fatalf("text-only default availability = %+v, %v", availability, err)
	}
}

func TestCloudModelGatewayDispatchesSelectedPersonalModelWithoutAdminDefault(t *testing.T) {
	store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
	client := &modelGatewayClientTest{response: ModelResponse{Text: "hello", Usage: ModelUsage{Known: true, InputTokens: 3, OutputTokens: 2}}}
	service := modelConnectionTestService(t, store, client)
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: "t1234567", ClientInstallationID: "i1234567"}
	created, err := service.Create(context.Background(), principal, ModelConnectionCommand{
		Name: "candidate", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test", APIKey: "secret-value",
	})
	if err != nil {
		t.Fatal(err)
	}
	checkedAt := time.Now().UTC()
	record := store.values[created.ID]
	record.Capabilities, record.CheckedAt = ModelCapabilities{Text: true, Tools: true}, &checkedAt
	store.values[created.ID] = record
	store.defaultModelID, store.defaultVersion = "", 0

	configured, err := service.IsConfiguredModel(context.Background(), principal.UserID, created.ID)
	if err != nil || !configured {
		t.Fatalf("model without platform default configured=%t, err=%v", configured, err)
	}
	ledger := &modelGatewayStoreTest{}
	gateway, err := NewModelGatewayService(ledger, service, client)
	if err != nil {
		t.Fatal(err)
	}
	response, err := gateway.Complete(context.Background(), ModelCall{
		Source: ModelCallClient, Principal: principal, ModelID: created.ID, CallID: "no-default-model-01",
		Request: ModelRequest{Input: []ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 10},
	})
	if err != nil || response.Text != "hello" || ledger.reservation.CallID != "no-default-model-01" || client.calls != 1 || client.request.Endpoint.APIKey != "secret-value" {
		t.Fatalf("model without platform default failed to dispatch: reservation=%+v calls=%d err=%v", ledger.reservation, client.calls, err)
	}
}

func TestPlatformConnectionUsesMeteredModelGateway(t *testing.T) {
	store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
	service := modelConnectionTestService(t, store, &modelClientStub{})
	admin := identity.Principal{UserID: "u7654321", Role: identity.RoleAdmin, Kind: identity.KindSession, TokenID: "t1234567"}
	created, err := service.Create(context.Background(), admin, ModelConnectionCommand{
		Name: "platform", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test", APIKey: "platform-secret", Shared: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkedAt := time.Now().UTC()
	record := store.values[created.ID]
	record.Capabilities, record.CheckedAt = ModelCapabilities{Text: true, Tools: true}, &checkedAt
	store.values[created.ID] = record

	ledger := &modelGatewayStoreTest{}
	client := &modelGatewayClientTest{response: ModelResponse{Text: "answer", Usage: ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}}}
	gateway, err := NewModelGatewayService(ledger, service, client)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient, TokenID: "t1234567", ClientInstallationID: "i1234567"}
	response, err := gateway.Complete(context.Background(), ModelCall{
		Source: ModelCallClient, Principal: principal, ModelID: created.ID, CallID: "platform-call-01",
		Request: ModelRequest{Input: []ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 10},
	})
	if err != nil || response.Text != "answer" || client.request.Endpoint.APIKey != "platform-secret" ||
		!ledger.dispatch || ledger.reservation.Source != ModelCallClient || ledger.settlement.Unknown {
		t.Fatalf("platform gateway response=%#v request=%#v reservation=%#v settlement=%#v err=%v", response, client.request, ledger.reservation, ledger.settlement, err)
	}
}

func TestCheckedTextToolModelWithoutVisionCanRunNonVisualTasks(t *testing.T) {
	store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
	client := &modelClientStub{capabilities: ModelCapabilities{Text: true, Tools: true, Vision: false}}
	service := modelConnectionTestService(t, store, client)
	principal := identity.Principal{UserID: "u7654321", Role: identity.RoleAdmin, Kind: identity.KindSession, TokenID: "t1234567"}
	created, err := service.Create(context.Background(), principal, ModelConnectionCommand{
		Name: "text and tools", BaseURL: "https://api.openai.com/v1", ModelName: "text-tool-model", APIKey: "secret-value", Shared: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkedAt := time.Now().UTC()
	stored := store.values[created.ID]
	stored.Capabilities, stored.CheckedAt = client.capabilities, &checkedAt
	store.values[created.ID] = stored
	configured, err := service.IsConfiguredModel(context.Background(), principal.UserID, created.ID)
	if err != nil || !configured {
		t.Fatalf("no-vision model configured = %t, err=%v", configured, err)
	}
	endpoint, err := service.ResolveEndpoint(context.Background(), principal.UserID, created.ID)
	if err != nil || endpoint.APIKey != "secret-value" || endpoint.ModelID != "text-tool-model" {
		t.Fatalf("ResolveEndpoint() = %+v, %v", endpoint, err)
	}
}

func TestConfiguredTextOnlyModelSeparatesConfigurationFromCapability(t *testing.T) {
	store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
	client := &modelGatewayClientTest{response: ModelResponse{Text: "hello", StopReason: "stop", Usage: ModelUsage{Known: true, InputTokens: 3, OutputTokens: 2}}}
	service := modelConnectionTestService(t, store, client)
	owner := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: "t1234567", ClientInstallationID: "i1234567"}
	created, err := service.Create(context.Background(), owner, ModelConnectionCommand{
		Name: "text only", BaseURL: "https://api.openai.com/v1", ModelName: "text-model", APIKey: "test-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if configured, err := service.IsConfiguredModel(context.Background(), owner.UserID, created.ID); err != nil || !configured {
		t.Fatalf("saved but unchecked model configured = %t, %v", configured, err)
	}
	if configured, err := service.IsConfiguredModel(context.Background(), "u7654321", created.ID); err != nil || configured {
		t.Fatalf("other account can use private model = %t, %v", configured, err)
	}
	if _, err := service.ResolveGatewayEndpoint(context.Background(), owner.UserID, created.ID, false, false); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("unchecked model resolved = %v", err)
	}

	checked := time.Now().UTC()
	record := store.values[created.ID]
	record.CheckedAt = &checked
	record.Capabilities = ModelCapabilities{Text: true}
	store.values[created.ID] = record
	if endpoint, err := service.ResolveEndpoint(context.Background(), owner.UserID, created.ID); err != nil || endpoint.ModelID != "text-model" {
		t.Fatalf("text-only model endpoint = %+v, %v", endpoint, err)
	}
	if _, err := service.ResolveGatewayEndpoint(context.Background(), owner.UserID, created.ID, true, false); domain.CodeOf(err) != domain.CodeUnsupported {
		t.Fatalf("tool request to text-only model = %v", err)
	}
	if _, err := service.ResolveGatewayEndpoint(context.Background(), owner.UserID, created.ID, false, true); domain.CodeOf(err) != domain.CodeUnsupported {
		t.Fatalf("image request to text-only model = %v", err)
	}

	ledger := &modelGatewayStoreTest{}
	gateway, err := NewModelGatewayService(ledger, service, client)
	if err != nil {
		t.Fatal(err)
	}
	response, err := gateway.Complete(context.Background(), ModelCall{
		Source: ModelCallClient, Principal: owner, ModelID: created.ID, CallID: "text-model-test-001",
		Request: ModelRequest{Input: []ModelInput{{Type: "message", Role: "user", Text: "hello"}}, MaxOutputTokens: 10},
	})
	if err != nil || response.Text != "hello" || client.calls != 1 || ledger.reservation.NeedsTools {
		t.Fatalf("text-only gateway call: response=%+v calls=%d tools=%t err=%v", response, client.calls, ledger.reservation.NeedsTools, err)
	}
	record.APIKeyCipher = nil
	store.values[created.ID] = record
	if configured, err := service.IsConfiguredModel(context.Background(), owner.UserID, created.ID); err != nil || configured {
		t.Fatalf("model with no saved credential configured = %t, %v", configured, err)
	}
}

func TestModelConfigurationStatusUsesOnlyAuthorizedConnections(t *testing.T) {
	store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
	client := &modelClientStub{err: errors.New("provider unavailable")}
	service := modelConnectionTestService(t, store, client)
	delete(store.values, store.defaultModelID)
	owner := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	other := identity.Principal{UserID: "u7654321", Role: identity.RoleAdmin, Kind: identity.KindSession, TokenID: "t7654321"}
	personal, err := service.Create(context.Background(), owner, ModelConnectionCommand{
		Name: "private", BaseURL: "https://api.openai.com/v1", ModelName: "personal-model", APIKey: "test-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkModelForTest(t, service, client, owner, personal.ID); err == nil {
		t.Fatal("expected the provider check to fail")
	}
	ownerViews, err := service.List(context.Background(), owner)
	if err != nil || ModelConnectionsConfigurationStatus(ownerViews) != ModelConfigurationConfigured {
		t.Fatalf("owner configuration after failed probe = %+v, %v", ownerViews, err)
	}
	otherViews, err := service.List(context.Background(), other)
	if err != nil || ModelConnectionsConfigurationStatus(otherViews) != ModelConfigurationNotConfigured {
		t.Fatalf("other account sees private configuration = %+v, %v", otherViews, err)
	}
	if _, err := service.Create(context.Background(), other, ModelConnectionCommand{
		Name: "shared", BaseURL: "https://api.openai.com/v1", ModelName: "shared-model", APIKey: "test-key", Shared: true,
	}); err != nil {
		t.Fatal(err)
	}
	otherViews, err = service.List(context.Background(), other)
	if err != nil || ModelConnectionsConfigurationStatus(otherViews) != ModelConfigurationConfigured {
		t.Fatalf("shared configuration unavailable = %+v, %v", otherViews, err)
	}
}

func TestModelConnectionScopeSeparatesPersonalAndSharedManagement(t *testing.T) {
	store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
	client := &modelClientStub{capabilities: ModelCapabilities{Text: true, Tools: true, Vision: true}}
	service := modelConnectionTestService(t, store, client)
	user := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	admin := identity.Principal{UserID: "u7654321", Role: identity.RoleAdmin, Kind: identity.KindSession, TokenID: "t7654321"}
	if _, err := service.Create(context.Background(), user, ModelConnectionCommand{
		Name: "shared", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test", APIKey: "secret-value", Shared: true,
	}); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("user shared create error = %v", err)
	}
	personal, err := service.Create(context.Background(), user, ModelConnectionCommand{
		Name: "personal", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test", APIKey: "secret-value",
	})
	if err != nil || personal.Shared || store.values[personal.ID].OwnerID != user.UserID {
		t.Fatalf("user personal create = %+v, stored=%+v, err=%v", personal, store.values[personal.ID], err)
	}
	adminPersonal, err := service.Create(context.Background(), admin, ModelConnectionCommand{
		Name: "personal", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test", APIKey: "secret-value",
	})
	if err != nil || adminPersonal.Shared || store.values[adminPersonal.ID].OwnerID != admin.UserID {
		t.Fatalf("admin personal create = %+v, err=%v", adminPersonal, err)
	}
	shared, err := service.Create(context.Background(), admin, ModelConnectionCommand{
		Name: "shared", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test", APIKey: "secret-value", Shared: true,
	})
	if err != nil || !shared.Shared {
		t.Fatalf("admin shared create = %+v, %v", shared, err)
	}
	list, err := service.List(context.Background(), user)
	if err != nil || len(list) != 3 || !list[0].Shared || !list[1].Shared || list[2].ID != personal.ID || list[2].Shared || !list[0].HasAPIKey {
		t.Fatalf("shared model visibility = %+v, %v", list, err)
	}
	if _, err := service.Update(context.Background(), user, shared.ID, ModelConnectionCommand{
		Name: "takeover", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test",
	}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("user shared update error = %v", err)
	}
	if _, err := service.Update(context.Background(), admin, shared.ID, ModelConnectionCommand{
		Name: "shared", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test",
	}); err != nil {
		t.Fatalf("admin shared update error = %v", err)
	}
	if _, err := service.ResolveEndpoint(context.Background(), user.UserID, shared.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("unchecked shared connection resolve error = %v", err)
	}
	if err := validateModelAccountPrincipal(identity.Principal{UserID: user.UserID, Role: user.Role, Kind: identity.KindAPIKey}); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("API key was allowed to manage model connections: %v", err)
	}
}

func TestLegacyPersonalModelConnectionsRemainPrivate(t *testing.T) {
	checkedAt := time.Now().UTC()
	legacy := ModelConnectionRecord{
		ID: "p1234567", OwnerID: "u1234567", Name: "legacy", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-legacy",
		APIKeyCipher: []byte("opaque-existing-ciphertext"), Capabilities: ModelCapabilities{Text: true, Tools: true}, CheckedAt: &checkedAt,
		CreatedAt: checkedAt, UpdatedAt: checkedAt,
	}
	store := &modelConnectionStoreStub{values: map[string]ModelConnectionRecord{legacy.ID: legacy}}
	client := &modelClientStub{}
	service := modelConnectionTestService(t, store, client)
	user := identity.Principal{UserID: legacy.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}

	if items, err := service.List(context.Background(), user); err != nil || len(items) != 2 || !items[0].Shared || items[1].ID != legacy.ID || items[1].Shared {
		t.Fatalf("legacy personal list = %+v, %v", items, err)
	}
	other := identity.Principal{UserID: "u7654321", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t7654321"}
	if _, err := service.Get(context.Background(), other, legacy.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("other account read legacy personal model = %v", err)
	}
	if _, err := service.ResolveProbeEndpoint(context.Background(), other, legacy.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("other account probed legacy personal model = %v", err)
	}
}

func TestModelConnectionRejectsUnapprovedEndpointsAndResetsChecks(t *testing.T) {
	store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
	client := &modelClientStub{capabilities: ModelCapabilities{Text: true, Tools: true, Vision: true}}
	service := modelConnectionTestService(t, store, client)
	principal := identity.Principal{UserID: "u7654321", Role: identity.RoleAdmin, Kind: identity.KindSession, TokenID: "t1234567"}
	for _, endpoint := range []string{
		"http://api.openai.com/v1", "https://evil.example/v1", "https://127.0.0.1/v1",
		"https://user:pass@api.openai.com/v1", "https://api.openai.com/v1?token=secret", "https://api.openai.com:8443/v1",
		"https://api.openai.com:/v1", "https://api.openai.com/v1/../private", "https://api.openai.com.",
		"https://api.openai.com/v1/" + strings.Repeat("a", 2050),
	} {
		_, err := service.Create(context.Background(), principal, ModelConnectionCommand{Name: "invalid", BaseURL: endpoint, ModelName: "gpt-test", APIKey: "secret-value", Shared: true})
		if err == nil {
			t.Errorf("Create accepted endpoint %q", endpoint)
		}
	}
	created, err := service.Create(context.Background(), principal, ModelConnectionCommand{
		Name: "platform", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test", APIKey: "secret-value", Shared: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkModelForTest(t, service, client, principal, created.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := service.Update(context.Background(), principal, created.ID, ModelConnectionCommand{
		Name: "platform", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-new",
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
	client := &modelClientStub{err: &ModelClientError{Kind: ModelErrorInvalidRequest, Stage: "provider_status", ProviderCode: "unsupported_parameter"}}
	service := modelConnectionTestService(t, store, client)
	principal := identity.Principal{UserID: "u7654321", Role: identity.RoleAdmin, Kind: identity.KindSession, TokenID: "t1234567"}
	created, err := service.Create(context.Background(), principal, ModelConnectionCommand{
		Name: "platform", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-test", APIKey: "secret-value", Shared: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = checkModelForTest(t, service, client, principal, created.ID)
	var appErr *domain.Error
	if !errors.As(err, &appErr) || appErr.Code != domain.CodeUpstreamError || appErr.Details["reason"] != string(ModelErrorInvalidRequest) ||
		appErr.Details["stage"] != "provider_status" || appErr.Details["provider_code"] != "unsupported_parameter" || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("provider error = %#v", err)
	}
}

func TestModelConnectionDirectCheckIsDisabledWithoutLedger(t *testing.T) {
	store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
	client := &modelClientStub{}
	service := modelConnectionTestService(t, store, client)
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	if _, err := service.Check(context.Background(), principal, "p1234567"); domain.CodeOf(err) != domain.CodeUnsupported || client.endpoint.APIKey != "" {
		t.Fatalf("unmetered model check was allowed: %v", err)
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
