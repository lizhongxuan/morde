package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/hub"
	"morde/server/internal/domain/personal"
)

type clientContractFixture struct {
	Capabilities map[string]struct {
		ServiceInstanceID      string   `json:"service_instance_id"`
		APIVersion             string   `json:"api_version"`
		HubVersion             *string  `json:"hub_version"`
		SupportedFeatures      []string `json:"supported_features"`
		DefaultMaxRequestBytes int64    `json:"default_max_request_bytes"`
	} `json:"capabilities"`
	Errors []struct {
		Status int `json:"status"`
		Body   struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"body"`
	} `json:"errors"`
	ErrorCodes   []string `json:"error_codes"`
	TaskSnapshot struct {
		EventSequence    int64 `json:"event_sequence"`
		AssistantMessage struct {
			ID             string `json:"id"`
			ConversationID string `json:"conversation_id"`
			Sequence       int64  `json:"sequence"`
			TaskID         string `json:"task_id"`
			Role           string `json:"role"`
			Content        string `json:"content"`
			Status         string `json:"status"`
			CreatedAt      string `json:"created_at"`
		} `json:"assistant_message"`
	} `json:"task_snapshot"`
	AssistantMessageStatuses []string                   `json:"assistant_message_statuses"`
	TaskEventFrames          map[string]string          `json:"task_event_frames"`
	HubFrames                map[string]json.RawMessage `json:"hub_frames"`
	PersonalData             struct {
		ProfileRead struct {
			ObjectType string                     `json:"object_type"`
			ObjectID   string                     `json:"object_id"`
			Revision   int64                      `json:"revision"`
			Body       map[string]json.RawMessage `json:"body"`
		} `json:"profile_read"`
		ProfilePutRequest struct {
			BaseRevision int64           `json:"base_revision"`
			Body         json.RawMessage `json:"body"`
		} `json:"profile_put_request"`
		ProfileDeleteRequest struct {
			BaseRevision int64 `json:"base_revision"`
		} `json:"profile_delete_request"`
		MutationReceipt struct {
			ObjectType            string `json:"object_type"`
			ObjectID              string `json:"object_id"`
			ResultRevision        int64  `json:"result_revision"`
			AccountChangeSequence int64  `json:"account_change_sequence"`
			Replay                bool   `json:"replay"`
		} `json:"mutation_receipt"`
		SnapshotPage struct {
			Watermark int64 `json:"watermark"`
			Items     []struct {
				ObjectType string          `json:"object_type"`
				ObjectID   string          `json:"object_id"`
				Revision   int64           `json:"revision"`
				Body       json.RawMessage `json:"body"`
			} `json:"items"`
			NextCursor *string `json:"next_cursor"`
		} `json:"snapshot_page"`
		ChangesPage struct {
			Items []struct {
				ChangeSequence int64  `json:"change_sequence"`
				ObjectType     string `json:"object_type"`
				ObjectID       string `json:"object_id"`
				Revision       int64  `json:"revision"`
				Operation      string `json:"operation"`
			} `json:"items"`
			ThroughSequence   int64 `json:"through_sequence"`
			NextAfterSequence int64 `json:"next_after_sequence"`
			HasMore           bool  `json:"has_more"`
		} `json:"changes_page"`
		IdempotencyKey string `json:"idempotency_key"`
	} `json:"personal_data"`
}

type personalDataCapabilityStub struct{ personalDataService }
type personalSyncCapabilityStub struct{ personalSyncService }

func loadClientContractFixture(t *testing.T) clientContractFixture {
	t.Helper()
	path := filepath.Join("..", "..", "openapi", "fixtures", "morde-client-v1.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shared Morde client contract fixture: %v", err)
	}
	var fixture clientContractFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("decode shared Morde client contract fixture: %v", err)
	}
	return fixture
}

func TestCapabilitiesArePublicAndOnlyAdvertiseAssembledServices(t *testing.T) {
	services := newServices()
	deps := services.deps()
	deps.ClientLogin = &clientLoginHTTPStub{}
	deps.PersonalData = &personalDataCapabilityStub{}
	options := validOptions()
	options.ServiceInstanceID = "9f0de095-66ba-49c6-a87b-0c527f4f81a1"
	router, err := New(deps, options)
	if err != nil {
		t.Fatal(err)
	}
	response := request(router, http.MethodGet, "/api/capabilities", "", "", "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("capabilities = %d %s", response.Code, response.Body)
	}
	var result capabilitiesResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ServiceInstanceID != options.ServiceInstanceID || result.APIVersion != "1" || result.DefaultMaxRequestBytes != options.APIMaxBodyBytes {
		t.Fatalf("capability identity or limits = %+v", result)
	}
	if result.HubVersion != nil || result.EventVersion != nil || result.MinimumClientVersion != nil {
		t.Fatalf("unimplemented protocols must not be claimed: %+v", result)
	}
	for _, feature := range []string{"password_login", "ordered_client_login", "client_installations"} {
		if !slices.Contains(result.SupportedFeatures, feature) {
			t.Fatalf("missing %s in %v", feature, result.SupportedFeatures)
		}
	}
	for _, feature := range []string{"client_login", "available_models", "task_api", "cloud_tasks", "task_media_generation", "project_budget_execution", "project_work_items_v2", "hub", "personal_sync", "workspace_sync"} {
		if slices.Contains(result.SupportedFeatures, feature) {
			t.Fatalf("unassembled or unimplemented feature %s was advertised", feature)
		}
	}
}

func TestProjectDerivativeCapabilityRequiresAssembledService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	services := newServices()
	deps := services.deps()
	deps.ProjectConversations = &projectConversationRoutesStub{}
	attachmentService := &projectAttachmentAPIStub{}
	deps.ProjectConversationAttachments = attachmentService
	options := validOptions()
	options.ServiceInstanceID = "9f0de095-66ba-49c6-a87b-0c527f4f81a1"
	router, err := New(deps, options)
	if err != nil {
		t.Fatal(err)
	}
	read := func() capabilitiesResponse {
		response := request(router, http.MethodGet, "/api/capabilities", "", "", "")
		var result capabilitiesResponse
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil {
			t.Fatalf("capability response=%d", response.Code)
		}
		return result
	}
	if slices.Contains(read().SupportedFeatures, "project_attachment_derivatives_v1") {
		t.Fatal("unready derivative service was advertised")
	}
	attachmentService.derivativesReady = true
	if !slices.Contains(read().SupportedFeatures, "project_attachment_derivatives_v1") {
		t.Fatal("assembled derivative service was not advertised")
	}
}

func TestCapabilitiesSeparateTaskAPIFromCloudExecution(t *testing.T) {
	services := newServices()
	deps := services.deps()
	deps.Tasks = &taskServiceStub{}
	deps.Conversations = &conversationServiceStub{}
	options := validOptions()
	options.ServiceInstanceID = "9f0de095-66ba-49c6-a87b-0c527f4f81a1"
	router, err := New(deps, options)
	if err != nil {
		t.Fatal(err)
	}
	response := request(router, http.MethodGet, "/api/capabilities", "", "", "")
	var result capabilitiesResponse
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil ||
		!slices.Contains(result.SupportedFeatures, "task_api") || slices.Contains(result.SupportedFeatures, "cloud_tasks") ||
		slices.Contains(result.SupportedFeatures, "project_work_items_v2") {
		t.Fatalf("queued-only task capabilities = %d %s; want task_api without cloud_tasks", response.Code, response.Body)
	}

	deps.ExecutionControl = &executionControlServiceStub{}
	deps.ProjectBudgetExecutionReady = true
	router, err = New(deps, options)
	if err != nil {
		t.Fatal(err)
	}
	response = request(router, http.MethodGet, "/api/capabilities", "", "", "")
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil ||
		!slices.Contains(result.SupportedFeatures, "task_api") || !slices.Contains(result.SupportedFeatures, "cloud_tasks") ||
		slices.Contains(result.SupportedFeatures, "project_budget_execution") {
		t.Fatalf("executable task capabilities = %d %s; want task_api and cloud_tasks without the incomplete project stack", response.Code, response.Body)
	}
}

func TestCapabilitiesAdvertiseProjectWorkItemsV2WhenMounted(t *testing.T) {
	services := newServices()
	deps := services.deps()
	deps.ProjectWorkItems = &projectWorkItemRoutesStub{}
	options := validOptions()
	options.ServiceInstanceID = "9f0de095-66ba-49c6-a87b-0c527f4f81a1"
	router, err := New(deps, options)
	if err != nil {
		t.Fatal(err)
	}
	response := request(router, http.MethodGet, "/api/capabilities", "", "", "")
	var result capabilitiesResponse
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.APIVersion != "1" ||
		!slices.Contains(result.SupportedFeatures, "project_work_items_v2") {
		t.Fatalf("project work item capability = %d %s, want v2 capability without a version bump", response.Code, response.Body)
	}
}

func TestTaskMediaCapabilityRequiresReadyGenerationService(t *testing.T) {
	services := newServices()
	deps := services.deps()
	deps.Tasks = &taskServiceStub{}
	deps.Conversations = &conversationServiceStub{}
	deps.ExecutionControl = &executionControlServiceStub{}
	deps.WorkspaceSync = &workspaceSyncServiceStub{}
	deps.ModelConnections = &modelConnectionManagerStub{}
	deps.ModelGateway = &modelGatewayHTTPTest{}
	media := &taskMediaPreviewStub{}
	deps.TaskMedia = media
	options := validOptions()
	options.ServiceInstanceID = "9f0de095-66ba-49c6-a87b-0c527f4f81a1"

	for _, ready := range []bool{false, true} {
		media.generationReady = ready
		router, err := New(deps, options)
		if err != nil {
			t.Fatal(err)
		}
		response := request(router, http.MethodGet, "/api/capabilities", "", "", "")
		var result capabilitiesResponse
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil {
			t.Fatalf("media capabilities response = %d %s", response.Code, response.Body)
		}
		if slices.Contains(result.SupportedFeatures, "task_media_generation") != ready {
			t.Fatalf("media generation readiness %t produced features %v", ready, result.SupportedFeatures)
		}
	}
}

func TestSharedClientFixtureMatchesCapabilitiesErrorsAndTaskCursorFrame(t *testing.T) {
	fixture := loadClientContractFixture(t)
	for _, scenario := range []struct {
		name                   string
		taskAPI                bool
		cloudExecution         bool
		projectBudgetExecution bool
		hub                    bool
		personalSync           bool
	}{
		{name: "unsupported"},
		{name: "queued_task_api", taskAPI: true},
		{name: "cloud_execution", taskAPI: true, cloudExecution: true},
		{name: "project_budget_execution", taskAPI: true, cloudExecution: true, projectBudgetExecution: true},
		{name: "hub", hub: true},
		{name: "personal_sync", personalSync: true},
	} {
		expected, ok := fixture.Capabilities[scenario.name]
		if !ok {
			t.Fatalf("shared contract fixture missing capabilities scenario %q", scenario.name)
		}
		services := newServices()
		deps := services.deps()
		deps.ClientLogin = &clientLoginHTTPStub{}
		deps.ModelConnections = &modelConnectionManagerStub{}
		if scenario.hub {
			deps.Hub = newHubSessionStub()
		}
		if scenario.personalSync {
			deps.PersonalData = &personalDataCapabilityStub{}
			deps.PersonalSync = &personalSyncCapabilityStub{}
		}
		if scenario.taskAPI {
			deps.Tasks = &taskServiceStub{}
			deps.Conversations = &conversationServiceStub{}
		}
		if scenario.cloudExecution {
			deps.ExecutionControl = &executionControlServiceStub{}
		}
		if scenario.projectBudgetExecution {
			deps.ProjectBudgetExecutionReady = true
			deps.Projects = &projectRoutesStub{}
			deps.ProjectContent = &projectContentRoutesStub{}
			deps.ProjectConversations = &projectConversationRoutesStub{}
			deps.ProjectWorkItems = &projectWorkItemRoutesStub{}
			deps.ProjectBudget = &projectBudgetRoutesServiceStub{}
			deps.ModelConnections = &modelConnectionManagerStub{}
			deps.ModelGateway = &modelGatewayHTTPTest{}
		}
		options := validOptions()
		options.ServiceInstanceID = expected.ServiceInstanceID
		router, err := New(deps, options)
		if err != nil {
			t.Fatal(err)
		}
		response := request(router, http.MethodGet, "/api/capabilities", "", "", "")
		var actual capabilitiesResponse
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &actual) != nil {
			t.Fatalf("fixture capabilities %s response = %d %s", scenario.name, response.Code, response.Body)
		}
		if actual.APIVersion != expected.APIVersion || actual.DefaultMaxRequestBytes != expected.DefaultMaxRequestBytes ||
			(actual.HubVersion == nil) != (expected.HubVersion == nil) ||
			(actual.HubVersion != nil && *actual.HubVersion != *expected.HubVersion) ||
			!slices.Equal(actual.SupportedFeatures, expected.SupportedFeatures) {
			t.Fatalf("fixture capabilities %s = %+v; expected %+v", scenario.name, actual, expected)
		}
	}

	openAPI, err := os.ReadFile(filepath.Join("..", "..", "openapi", "portal.yaml"))
	if err != nil {
		t.Fatalf("read Portal OpenAPI schema: %v", err)
	}
	const errorSchemaMarker = "    ErrorResponse:\n"
	start := strings.Index(string(openAPI), errorSchemaMarker)
	if start < 0 {
		t.Fatal("Portal OpenAPI ErrorResponse schema is missing")
	}
	section := string(openAPI[start:])
	match := regexp.MustCompile(`enum: \[([^\]]+)\]`).FindStringSubmatch(section)
	if len(match) != 2 {
		t.Fatal("Portal OpenAPI ErrorResponse code enum is missing")
	}
	var schemaCodes []string
	for _, code := range strings.Split(match[1], ",") {
		schemaCodes = append(schemaCodes, strings.TrimSpace(code))
	}
	if !slices.Equal(schemaCodes, fixture.ErrorCodes) {
		t.Fatalf("shared client error codes = %v; OpenAPI = %v", fixture.ErrorCodes, schemaCodes)
	}
	if fixture.TaskSnapshot.AssistantMessage.Status == "" ||
		!slices.Contains(fixture.AssistantMessageStatuses, fixture.TaskSnapshot.AssistantMessage.Status) ||
		fixture.TaskSnapshot.AssistantMessage.Role != string(conversation.RoleAssistant) ||
		fixture.TaskSnapshot.AssistantMessage.ID == "" || fixture.TaskSnapshot.AssistantMessage.Sequence < 1 ||
		fixture.TaskSnapshot.AssistantMessage.Content == "" {
		t.Fatalf("shared assistant message snapshot fixture is incomplete: %+v", fixture.TaskSnapshot.AssistantMessage)
	}
	assistantSchemaStart := strings.Index(string(openAPI), "    TaskAssistantMessage:\n")
	taskSchemaStart := strings.Index(string(openAPI), "    Task:\n")
	if assistantSchemaStart < 0 || taskSchemaStart <= assistantSchemaStart {
		t.Fatal("Portal OpenAPI TaskAssistantMessage schema is missing")
	}
	assistantSection := string(openAPI[assistantSchemaStart:taskSchemaStart])
	if !strings.Contains(assistantSection, "enum: [streaming, completed, interrupted]") {
		t.Fatal("Portal OpenAPI TaskAssistantMessage status enum differs from the shared fixture")
	}
	if !strings.Contains(assistantSection, "required: [id, conversation_id, sequence, task_id, role, content, status, created_at]") {
		t.Fatal("Portal OpenAPI TaskAssistantMessage must include the conversation sequence")
	}
	if !slices.Equal(fixture.AssistantMessageStatuses, []string{"streaming", "completed", "interrupted"}) {
		t.Fatalf("shared assistant message statuses = %v", fixture.AssistantMessageStatuses)
	}
	for _, value := range fixture.Errors {
		code := domain.ErrorCode(value.Body.Error.Code)
		router := gin.New()
		router.GET("/error", func(c *gin.Context) { writeError(c, domain.NewError(code, "fixture message is not trusted")) })
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/error", nil))
		var actual struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if response.Code != value.Status || json.Unmarshal(response.Body.Bytes(), &actual) != nil ||
			actual.Error.Code != value.Body.Error.Code || actual.Error.Message != value.Body.Error.Message {
			t.Errorf("fixture error %s = %d %+v; want %d %s", code, response.Code, actual.Error, value.Status, value.Body.Error.Message)
		}
	}
	var snapshotFrame strings.Builder
	if err := writeTaskSnapshotRequired(&snapshotFrame, fixture.TaskSnapshot.EventSequence); err != nil {
		t.Fatal(err)
	}
	if got, want := snapshotFrame.String(), fixture.TaskEventFrames["snapshot_required"]; got != want {
		t.Fatalf("snapshot recovery frame = %q; want %q", got, want)
	}
}

func TestSharedHubFramesMatchProtocolValidators(t *testing.T) {
	fixture := loadClientContractFixture(t)
	for name, raw := range fixture.HubFrames {
		frame, err := hub.DecodeFrame(raw)
		if err != nil {
			t.Fatalf("decode Hub fixture %s: %v", name, err)
		}
		switch name {
		case "hello":
			err = hub.ValidateHello(frame)
		case "ping", "devices", "cancel":
		case "call_info", "call_open", "call_notify":
			err = hub.ValidateCall(frame)
		case "result_info", "result_open", "result_notify", "result_denied":
			err = hub.ValidateResult(frame)
			if err == nil {
				action := map[string]string{"result_info": "info", "result_open": "open", "result_notify": "notify", "result_denied": "info"}[name]
				err = hub.ValidateActionResult(action, frame)
			}
		default:
			t.Fatalf("unknown Hub fixture frame %q", name)
		}
		if err != nil {
			t.Fatalf("validate Hub fixture %s: %v", name, err)
		}
	}
	if len(fixture.HubFrames) != 11 {
		t.Fatalf("Hub fixture frame count = %d, want 11", len(fixture.HubFrames))
	}
}

func TestSharedPersonalDataFixtureMatchesProfileAndSyncContract(t *testing.T) {
	fixture := loadClientContractFixture(t)
	value := fixture.PersonalData
	if value.ProfileRead.ObjectType != personal.ObjectTypeProfile || value.ProfileRead.ObjectID != personal.ProfileObjectID ||
		value.ProfileRead.Revision < 1 || value.IdempotencyKey == "" ||
		value.MutationReceipt.ObjectType != personal.ObjectTypeProfile || value.MutationReceipt.ObjectID != personal.ProfileObjectID ||
		value.MutationReceipt.ResultRevision < 1 || value.MutationReceipt.AccountChangeSequence < 1 {
		t.Fatalf("shared personal data fixture is incomplete: %+v", value)
	}
	if err := personal.ValidateMutation(personal.Mutation{
		OwnerID: testUserID, MutationID: value.IdempotencyKey, ObjectType: personal.ObjectTypeProfile,
		ObjectID: personal.ProfileObjectID, BaseRevision: value.ProfilePutRequest.BaseRevision,
		Body: value.ProfilePutRequest.Body,
	}); err != nil {
		t.Fatalf("shared profile PUT fixture violates the domain contract: %v", err)
	}
	if value.ProfileDeleteRequest.BaseRevision < value.ProfilePutRequest.BaseRevision {
		t.Fatalf("shared profile delete revision is invalid: %+v", value.ProfileDeleteRequest)
	}
	if value.SnapshotPage.Watermark < 0 || len(value.SnapshotPage.Items) == 0 || value.SnapshotPage.NextCursor == nil ||
		len(value.SnapshotPage.Items) > 100 {
		t.Fatalf("shared personal snapshot fixture is incomplete: %+v", value.SnapshotPage)
	}
	for _, item := range value.SnapshotPage.Items {
		if !personal.IsPersonalSyncObjectType(item.ObjectType) || personal.ValidateObjectIdentifier(item.ObjectType, item.ObjectID) != nil ||
			item.Revision < 1 || personal.ValidateObjectBody(item.ObjectType, item.ObjectID, item.Body) != nil {
			t.Fatalf("shared personal snapshot item is invalid: %+v", item)
		}
	}
	if len(value.ChangesPage.Items) == 0 || value.ChangesPage.ThroughSequence < value.SnapshotPage.Watermark ||
		value.ChangesPage.NextAfterSequence < value.SnapshotPage.Watermark || value.ChangesPage.HasMore {
		t.Fatalf("shared personal change fixture is incomplete: %+v", value.ChangesPage)
	}
	previousSequence := value.SnapshotPage.Watermark
	for _, change := range value.ChangesPage.Items {
		if change.ChangeSequence <= previousSequence || change.ChangeSequence > value.ChangesPage.ThroughSequence ||
			!personal.IsPersonalSyncObjectType(change.ObjectType) || personal.ValidateObjectIdentifier(change.ObjectType, change.ObjectID) != nil ||
			change.Revision < 1 || (change.Operation != "upsert" && change.Operation != "delete") {
			t.Fatalf("shared personal change item is invalid: %+v", change)
		}
		previousSequence = change.ChangeSequence
	}
	if value.ChangesPage.NextAfterSequence != previousSequence {
		t.Fatalf("shared personal change cursor = %d, want last returned sequence %d", value.ChangesPage.NextAfterSequence, previousSequence)
	}
}

func TestCapabilitiesFailClosedWithoutValidInstanceIdentity(t *testing.T) {
	for _, id := range []string{"", "cloudhost", "00000000-0000-0000-0000-000000000000"} {
		t.Run(id, func(t *testing.T) {
			options := validOptions()
			options.ServiceInstanceID = id
			router, err := New(newServices().deps(), options)
			if err != nil {
				t.Fatal(err)
			}
			response := request(router, http.MethodGet, "/api/capabilities", "", "", "")
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("invalid service identity returned %d %s", response.Code, response.Body)
			}
		})
	}
}

func TestConversationMessageOpenAPIUsesAsyncAcceptanceContract(t *testing.T) {
	openAPI, err := os.ReadFile(filepath.Join("..", "..", "openapi", "portal.yaml"))
	if err != nil {
		t.Fatalf("read Portal OpenAPI schema: %v", err)
	}
	contract := string(openAPI)
	routeStart := strings.Index(contract, "  /api/conversations/{id}/messages:\n")
	if routeStart < 0 {
		t.Fatal("Portal OpenAPI personal conversation messages route is missing")
	}
	routeEnd := strings.Index(contract[routeStart+1:], "  /api/conversations/{id}/messages/{message_id}/retry-processing:\n")
	if routeEnd < 0 {
		t.Fatal("Portal OpenAPI personal conversation messages route is not bounded")
	}
	route := contract[routeStart : routeStart+1+routeEnd]
	for _, requiredRule := range []string{
		"operationId: acceptConversationMessage\n",
		"#/components/schemas/AcceptConversationMessageRequest",
		"#/components/schemas/AcceptedConversationMessageResponse",
		"Task selection and execution happen asynchronously after acceptance",
	} {
		if !strings.Contains(route, requiredRule) {
			t.Fatalf("Portal OpenAPI message acceptance route is missing %q:\n%s", requiredRule, route)
		}
	}
	if strings.Contains(route, "appendConversationMessage") || strings.Contains(route, "AppendConversationMessageResponse") {
		t.Fatal("Portal OpenAPI personal route retains the old direct Task contract")
	}
	start := strings.Index(contract, "    AcceptConversationMessageRequest:\n")
	end := strings.Index(contract[start+1:], "\n    AcceptedConversationMessageResponse:\n")
	if start < 0 || end < 0 {
		t.Fatal("Portal OpenAPI message acceptance request schema is missing or unbounded")
	}
	section := contract[start : start+1+end]
	for _, requiredRule := range []string{
		"required: [client_message_id, content]",
		"previous_client_message_id:",
		"reference_task_id:",
		"client_attachment_ids:",
	} {
		if !strings.Contains(section, requiredRule) {
			t.Fatalf("Portal OpenAPI message acceptance request schema is missing %q:\n%s", requiredRule, section)
		}
	}
	if strings.Contains(section, "mode:") || strings.Contains(section, "input_revision:") || strings.Contains(section, "\n        attachment_ids:") {
		t.Fatalf("Portal OpenAPI message acceptance request contains direct Task fields:\n%s", section)
	}
}
