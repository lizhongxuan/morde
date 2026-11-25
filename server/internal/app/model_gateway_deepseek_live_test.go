package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/model"
)

type liveConversationRouteStore struct {
	liveModelCallStore
	calls       int
	settlements []routeSettlement
}

type routeSettlement struct {
	decision app.ConversationRouteDecision
	usage    app.ModelUsage
	unknown  bool
}

func (*liveConversationRouteStore) ResolveConversationRouteModel(context.Context, app.ConversationRouteClaim) (string, error) {
	return "p1234567", nil
}

func (*liveConversationRouteStore) PrepareConversationRouteModelCall(context.Context, app.ModelCallReservation,
	*app.ProjectModelReservation) (*app.ConversationRouteDecision, error) {
	return nil, nil
}

func (store *liveConversationRouteStore) BeginConversationRouteModelCall(context.Context, app.ModelCallReservation) error {
	store.calls++
	return nil
}

func (store *liveConversationRouteStore) SettleConversationRouteModelCall(_ context.Context, _ app.ModelCallReservation,
	decision *app.ConversationRouteDecision, usage app.ModelUsage, unknown bool) error {
	settled := routeSettlement{usage: usage, unknown: unknown}
	if decision != nil {
		settled.decision = *decision
	}
	store.settlements = append(store.settlements, settled)
	return nil
}

func TestDeepSeekFlashDialogueThroughMordeModelGateway(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("MORDE_TEST_DEEPSEEK_KEY"))
	if key == "" {
		t.Skip("MORDE_TEST_DEEPSEEK_KEY is unset; real DeepSeek dialogue tests were not run")
	}
	client, err := model.NewResponsesClient([]string{"api.deepseek.com"}, os.Getenv("MORDE_TEST_MODEL_PROXY"))
	if err != nil {
		t.Fatal(err)
	}
	store := &liveModelCallStore{}
	resolver := &liveModelResolver{endpoint: app.ModelEndpoint{
		BaseURL: "https://api.deepseek.com", APIKey: key, ModelID: "deepseek-flash",
	}}
	gateway, err := app.NewModelGatewayService(store, resolver, client)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: "t1234567", ClientInstallationID: "i1234567",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	nextCall := 0
	complete := func(t *testing.T, request app.ModelRequest) app.ModelResponse {
		t.Helper()
		nextCall++
		response, err := gateway.Complete(ctx, app.ModelCall{
			Source: app.ModelCallClient, Principal: principal, ModelID: "p1234567",
			CallID:  fmt.Sprintf("deepseek-live-%02d", nextCall),
			Request: request,
		})
		if err != nil {
			var gatewayErr *domain.Error
			if errors.As(err, &gatewayErr) {
				t.Fatalf("Morde model-gateway call %d failed: code=%s details=%v", nextCall, gatewayErr.Code, gatewayErr.Details)
			}
			t.Fatalf("Morde model-gateway call %d failed: %v", nextCall, err)
		}
		if !response.Usage.Known || response.Usage.InputTokens < 1 || response.Usage.OutputTokens < 1 {
			t.Fatalf("Morde model-gateway call %d returned invalid usage: %#v", nextCall, response.Usage)
		}
		return response
	}

	t.Run("Chinese factual question", func(t *testing.T) {
		response := complete(t, app.ModelRequest{
			Input:           []app.ModelInput{{Type: "message", Role: "user", Text: "请只回答数字：1+1等于几？"}},
			MaxOutputTokens: 32, DisableThinking: true,
		})
		if !strings.Contains(response.Text, "2") {
			t.Fatalf("answer did not contain 2: %q", response.Text)
		}
	})

	t.Run("format instruction", func(t *testing.T) {
		response := complete(t, app.ModelRequest{
			Input:           []app.ModelInput{{Type: "message", Role: "user", Text: "用中文给出恰好三条降低会议时长的建议，每条单独一行并以数字开头。"}},
			MaxOutputTokens: 96, DisableThinking: true,
		})
		if strings.TrimSpace(response.Text) == "" || !strings.Contains(response.Text, "1") || !strings.Contains(response.Text, "2") || !strings.Contains(response.Text, "3") {
			t.Fatalf("formatted answer did not contain the requested three numbered items: %q", response.Text)
		}
	})

	t.Run("route acknowledgement follows expected effort", func(t *testing.T) {
		routeStore := &liveConversationRouteStore{}
		routeGateway, err := app.NewModelGatewayService(routeStore, resolver, client)
		if err != nil {
			t.Fatal(err)
		}
		cases := []struct {
			name        string
			content     string
			acknowledge bool
		}{
			{name: "read one project file", content: "查看当前项目根目录的 README，找出运行命令并回复我，不要修改文件。"},
			{name: "audit and repair deployment", content: "全面核对当前仓库的部署配置与服务代码，修复不一致，运行相关测试，并编写可重复运行的检查脚本。", acknowledge: true},
			{name: "run one short command", content: "在当前项目执行 git status，只回复工作树是否干净。"},
			{name: "prepare a deployment script", content: "在当前仓库编写可重复运行的部署脚本，包含配置检查、构建、数据库迁移、启动和端到端验证，并实际运行脚本验证。", acknowledge: true},
		}
		for index, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				messageID := []string{"m1234567", "m2345678", "m3456789", "m4567890"}[index]
				claim := app.ConversationRouteClaim{Message: conversation.Message{ID: messageID, ConversationID: "c1234567",
					OwnerID: principal.UserID, ActorID: principal.UserID, Role: conversation.RoleUser, Sequence: int64(index + 1),
					Content: test.content}, ServiceInstanceID: "00000000-0000-4000-8000-000000000001", RouteAttempt: 1,
					LeaseOwner: "deepseek-route-test", LeaseVersion: 1}
				value := app.ConversationRouteContext{ActorID: principal.UserID, ModelID: "p1234567", RouteAttempt: 1, Content: test.content}
				decision, err := routeGateway.ClassifyConversationRoute(ctx, claim, value)
				if err != nil {
					t.Fatal(err)
				}
				if decision.Action != app.ConversationRouteNew || decision.InvalidOutput || decision.Acknowledge != test.acknowledge {
					t.Fatalf("route decision did not match expected effort: action=%q acknowledge=%t invalid=%t output=%q",
						decision.Action, decision.Acknowledge, decision.InvalidOutput, decision.ModelOutput)
				}
			})
		}
		if routeStore.calls != len(cases) || len(routeStore.settlements) != len(cases) {
			t.Fatalf("route model calls were not durably settled: calls=%d settlements=%d", routeStore.calls, len(routeStore.settlements))
		}
		for index, settlement := range routeStore.settlements {
			if settlement.unknown || !settlement.usage.Known || settlement.decision.InvalidOutput {
				t.Fatalf("route call %d did not settle a valid model decision: %#v", index+1, settlement)
			}
		}
	})

	t.Run("multi-turn context", func(t *testing.T) {
		first := complete(t, app.ModelRequest{
			Input:           []app.ModelInput{{Type: "message", Role: "user", Text: "请记住暗号是星河-2048，只回复收到。"}},
			MaxOutputTokens: 32, DisableThinking: true,
		})
		second := complete(t, app.ModelRequest{
			Input: []app.ModelInput{
				{Type: "message", Role: "user", Text: "请记住暗号是星河-2048，只回复收到。"},
				{Type: "message", Role: "assistant", Text: first.Text},
				{Type: "message", Role: "user", Text: "暗号是什么？只回复暗号。"},
			},
			MaxOutputTokens: 32, DisableThinking: true,
		})
		if !strings.Contains(second.Text, "星河-2048") {
			t.Fatalf("second turn did not use the earlier conversation context: %q", second.Text)
		}
	})

	t.Run("long input usage and history follow-up routing", func(t *testing.T) {
		var archive strings.Builder
		for index := 0; index < 800; index++ {
			marker := ""
			value := (index*37 + 11) % 997
			checksum := (value * (index + 1000)) % 1000003
			switch index {
			case 0:
				marker, value, checksum = "ALPHA-7KQ2", 11, 11000
			case 400:
				marker, value, checksum = "MID-4P8N", 842, 167600
			case 799:
				marker, value, checksum = "OMEGA-Q9W3", 913, 799913
			}
			fmt.Fprintf(&archive, "Record %04d: value=%d; checksum=%d; source=archive_batch_%02d", index, value, checksum, index%97)
			if marker != "" {
				fmt.Fprintf(&archive, "; marker=%s", marker)
			}
			archive.WriteByte('\n')
		}
		archiveText := archive.String()
		type recordAnswer struct {
			Marker   string `json:"marker"`
			Value    int    `json:"value"`
			Checksum int    `json:"checksum"`
		}
		type archiveAnswer struct {
			Count   int                     `json:"count"`
			Records map[string]recordAnswer `json:"records"`
		}
		parseAnswer := func(t *testing.T, response app.ModelResponse) archiveAnswer {
			t.Helper()
			var answer archiveAnswer
			if err := json.Unmarshal([]byte(response.Text), &answer); err != nil {
				t.Fatalf("long-input response was not the requested JSON: %q: %v", response.Text, err)
			}
			return answer
		}
		first := complete(t, app.ModelRequest{
			Instructions:    "只依据下面完整的合成档案回答：总记录数，以及记录 0000、0400、0799 各自的 marker、value 和 checksum。只输出 JSON 对象，字段为 count 和 records。不要猜测。",
			Input:           []app.ModelInput{{Type: "message", Role: "user", Text: archiveText}},
			MaxOutputTokens: 256, DisableThinking: true, JSONMode: true,
		})
		if first.Usage.InputTokens <= 8192 {
			t.Fatalf("provider reported only %d input tokens for %d synthetic bytes; want more than 8192",
				first.Usage.InputTokens, len(archiveText))
		}
		t.Logf("DeepSeek Flash via Morde gateway: input_tokens=%d synthetic_bytes=%d", first.Usage.InputTokens, len(archiveText))
		answer := parseAnswer(t, first)
		if answer.Count != 800 || answer.Records["0000"] != (recordAnswer{Marker: "ALPHA-7KQ2", Value: 11, Checksum: 11000}) ||
			answer.Records["0400"] != (recordAnswer{Marker: "MID-4P8N", Value: 842, Checksum: 167600}) ||
			answer.Records["0799"] != (recordAnswer{Marker: "OMEGA-Q9W3", Value: 913, Checksum: 799913}) {
			t.Fatalf("long-input answer did not preserve the exact count and anchors: %+v", answer)
		}

		window := 65536
		longHistoryResolver := &liveModelResolver{endpoint: resolver.endpoint, contextWindowTokens: &window}
		routeStore := &liveConversationRouteStore{}
		routeGateway, err := app.NewModelGatewayService(routeStore, longHistoryResolver, client)
		if err != nil {
			t.Fatal(err)
		}
		followup := "刚才档案里记录 0400 的 marker、value 和 checksum 是什么？"
		claim := app.ConversationRouteClaim{Message: conversation.Message{ID: "m5678901", ConversationID: "c1234567",
			OwnerID: principal.UserID, ActorID: principal.UserID, Role: conversation.RoleUser, Sequence: 2,
			Content: followup}, ServiceInstanceID: "00000000-0000-4000-8000-000000000001", RouteAttempt: 1,
			LeaseOwner: "deepseek-long-history-test", LeaseVersion: 1}
		decision, err := routeGateway.ClassifyConversationRoute(ctx, claim, app.ConversationRouteContext{
			ActorID: principal.UserID, ModelID: "p1234567", RouteAttempt: 1, Content: followup,
			HistoryIncomplete: true,
			History:           []app.ConversationRouteHistory{{MessageID: "m4567890", Sequence: 1, Role: conversation.RoleUser, Content: archiveText}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if decision.Action != app.ConversationRouteNew || decision.ExplicitNew || decision.Reply != "" {
			t.Fatalf("truncated-history follow-up did not route to authorized Task search: action=%q explicit_new=%t invalid=%t reply=%q output=%q",
				decision.Action, decision.ExplicitNew, decision.InvalidOutput, decision.Reply, decision.ModelOutput)
		}
		if routeStore.calls != 1 || len(routeStore.settlements) != 1 || routeStore.settlements[0].unknown ||
			!routeStore.settlements[0].usage.Known || routeStore.settlements[0].decision.InvalidOutput {
			t.Fatalf("long-history route usage was not durably settled: calls=%d settlements=%+v", routeStore.calls, routeStore.settlements)
		}
		selfContained := "请只回答数字：1+1等于几？"
		directClaim := app.ConversationRouteClaim{Message: conversation.Message{ID: "m6789012", ConversationID: "c1234567",
			OwnerID: principal.UserID, ActorID: principal.UserID, Role: conversation.RoleUser, Sequence: 3,
			Content: selfContained}, ServiceInstanceID: "00000000-0000-4000-8000-000000000001", RouteAttempt: 1,
			LeaseOwner: "deepseek-long-history-test", LeaseVersion: 1}
		directDecision, err := routeGateway.ClassifyConversationRoute(ctx, directClaim, app.ConversationRouteContext{
			ActorID: principal.UserID, ModelID: "p1234567", RouteAttempt: 1, Content: selfContained,
			HistoryIncomplete: true,
			History:           []app.ConversationRouteHistory{{MessageID: "m4567890", Sequence: 1, Role: conversation.RoleUser, Content: archiveText}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if directDecision.Action != app.ConversationRouteReply || strings.TrimSpace(directDecision.Reply) == "" || directDecision.InvalidOutput {
			t.Fatalf("self-contained question was not answered directly with incomplete history: action=%q invalid=%t reply=%q output=%q",
				directDecision.Action, directDecision.InvalidOutput, directDecision.Reply, directDecision.ModelOutput)
		}

		second := complete(t, app.ModelRequest{
			Instructions: "只依据对话中完整的合成档案回答记录 0400 的 marker、value 和 checksum。用一行中文，包含 marker、value、checksum，不输出 JSON，不要猜测。",
			Input: []app.ModelInput{
				{Type: "message", Role: "user", Text: archiveText},
				{Type: "message", Role: "assistant", Text: first.Text},
				{Type: "message", Role: "user", Text: followup},
			},
			MaxOutputTokens: 128, DisableThinking: true,
		})
		if second.Usage.InputTokens <= 8192 {
			t.Fatalf("provider reported only %d input tokens for the full-history follow-up; want more than 8192", second.Usage.InputTokens)
		}
		if !strings.Contains(second.Text, "MID-4P8N") || !strings.Contains(second.Text, "842") || !strings.Contains(second.Text, "167600") {
			t.Fatalf("full-history follow-up did not recover record 0400: %q", second.Text)
		}
		t.Logf("DeepSeek Flash full-history follow-up via Morde gateway: input_tokens=%d", second.Usage.InputTokens)
	})

	t.Run("streamed reply", func(t *testing.T) {
		var streamed strings.Builder
		response := complete(t, app.ModelRequest{
			Input:           []app.ModelInput{{Type: "message", Role: "user", Text: "请只输出 STREAM_OK。"}},
			MaxOutputTokens: 32, DisableThinking: true,
			OnTextDelta: func(delta string) error { streamed.WriteString(delta); return nil },
		})
		if !strings.Contains(response.Text, "STREAM_OK") || !strings.Contains(streamed.String(), "STREAM_OK") {
			t.Fatalf("streamed reply missing expected text: final=%q streamed=%q", response.Text, streamed.String())
		}
	})

	t.Run("tool call and result continuation", func(t *testing.T) {
		tool := app.ModelTool{Name: "lookup", Description: "Look up a synthetic test value.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"key":{"type":"string"}},"required":["key"],"additionalProperties":false}`)}
		request := app.ModelRequest{
			Instructions: "Call the lookup function with key set to M-42. Do not answer before calling it.",
			Input:        []app.ModelInput{{Type: "message", Role: "user", Text: "Look up the test value."}},
			Tools:        []app.ModelTool{tool}, RequiredTool: tool.Name,
			MaxOutputTokens: 64, DisableThinking: true,
		}
		first := complete(t, request)
		if len(first.ToolCalls) != 1 || first.ToolCalls[0].Name != tool.Name {
			t.Fatalf("model did not return the requested tool call: %#v", first.ToolCalls)
		}
		call := first.ToolCalls[0]
		request.RequiredTool = ""
		request.Input = append(request.Input,
			app.ModelInput{Type: "function_call", CallID: call.CallID, Name: call.Name, Arguments: call.Arguments},
			app.ModelInput{Type: "function_call_output", CallID: call.CallID, Output: "Synthetic lookup result: DELTA-73."},
		)
		second := complete(t, request)
		if !strings.Contains(second.Text, "DELTA-73") {
			t.Fatalf("model did not incorporate the tool result: %q", second.Text)
		}
	})

	if len(store.reservations) != nextCall || len(store.settlements) != nextCall {
		t.Fatalf("usage ledger calls: calls=%d reservations=%d settlements=%d", nextCall, len(store.reservations), len(store.settlements))
	}
}
