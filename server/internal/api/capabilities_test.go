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
)

type clientContractFixture struct {
	Capabilities map[string]struct {
		ServiceInstanceID      string   `json:"service_instance_id"`
		APIVersion             string   `json:"api_version"`
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
			TaskID         string `json:"task_id"`
			Role           string `json:"role"`
			Content        string `json:"content"`
			Status         string `json:"status"`
			CreatedAt      string `json:"created_at"`
		} `json:"assistant_message"`
	} `json:"task_snapshot"`
	AssistantMessageStatuses []string          `json:"assistant_message_statuses"`
	TaskEventFrames          map[string]string `json:"task_event_frames"`
}

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
	for _, feature := range []string{"client_login", "available_models", "task_api", "cloud_tasks", "hub", "personal_sync"} {
		if slices.Contains(result.SupportedFeatures, feature) {
			t.Fatalf("unassembled or unimplemented feature %s was advertised", feature)
		}
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
		!slices.Contains(result.SupportedFeatures, "task_api") || slices.Contains(result.SupportedFeatures, "cloud_tasks") {
		t.Fatalf("queued-only task capabilities = %d %s; want task_api without cloud_tasks", response.Code, response.Body)
	}

	deps.ExecutionControl = &executionControlServiceStub{}
	router, err = New(deps, options)
	if err != nil {
		t.Fatal(err)
	}
	response = request(router, http.MethodGet, "/api/capabilities", "", "", "")
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil ||
		!slices.Contains(result.SupportedFeatures, "task_api") || !slices.Contains(result.SupportedFeatures, "cloud_tasks") {
		t.Fatalf("executable task capabilities = %d %s; want task_api and cloud_tasks", response.Code, response.Body)
	}
}

func TestSharedClientFixtureMatchesCapabilitiesErrorsAndTaskCursorFrame(t *testing.T) {
	fixture := loadClientContractFixture(t)
	for _, scenario := range []struct {
		name           string
		taskAPI        bool
		cloudExecution bool
	}{
		{name: "unsupported"},
		{name: "queued_task_api", taskAPI: true},
		{name: "cloud_execution", taskAPI: true, cloudExecution: true},
	} {
		expected, ok := fixture.Capabilities[scenario.name]
		if !ok {
			t.Fatalf("shared contract fixture missing capabilities scenario %q", scenario.name)
		}
		services := newServices()
		deps := services.deps()
		deps.ClientLogin = &clientLoginHTTPStub{}
		deps.ModelConnections = &modelConnectionManagerStub{}
		if scenario.taskAPI {
			deps.Tasks = &taskServiceStub{}
			deps.Conversations = &conversationServiceStub{}
		}
		if scenario.cloudExecution {
			deps.ExecutionControl = &executionControlServiceStub{}
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
		fixture.TaskSnapshot.AssistantMessage.ID == "" || fixture.TaskSnapshot.AssistantMessage.Content == "" {
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
