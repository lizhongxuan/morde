package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"
)

const (
	personalMemoryPCBrowserAcceptanceEnv = "MORDE_PERSONAL_MEMORY_PC_ACCEPTANCE"
	personalMemoryPCSourceID             = "m_abcdef01"
	personalMemoryPCOriginalBody         = `{"content":"M08-ORCHID-8472：我希望先看详细分析。","category":"preference"}`
	personalMemoryPCCurrentText          = "M08-ORCHID-8472：我更喜欢先看三行摘要，再看可展开细节。"
	personalMemoryPCRevisedText          = "M08-ORCHID-8472：我现在更喜欢先看结论，再看证据。"
	personalMemoryPCSearchQuery          = "ORCHID-8472"
)

// TestPersonalMemoryPCBrowserAcceptance exercises the Personal Data UI in a
// real morde-pc process against a disposable PostgreSQL database and a
// loopback-only API. The broker context and history-search assertions below
// are separate synthetic server evidence: they do not invoke an LLM or claim
// that a live DSH model loop has been exercised.
func TestPersonalMemoryPCBrowserAcceptance(t *testing.T) {
	if os.Getenv(personalMemoryPCBrowserAcceptanceEnv) != "1" {
		t.Skip("set MORDE_PERSONAL_MEMORY_PC_ACCEPTANCE=1 to run the isolated morde-pc loopback acceptance")
	}
	gin.SetMode(gin.ReleaseMode)
	store, credentials, clients, owner := clientLoginFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatalf("enable the isolated owner's synthetic DSH Task execution gate: %v", err)
	}

	login, err := credentials.Login(ctx, owner.Name, "client-password-123", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifySession(ctx, login.Credential)
	if err != nil {
		t.Fatal(err)
	}
	personalData, err := app.NewPersonalMutationService(store)
	if err != nil {
		t.Fatal(err)
	}
	personalSync, err := app.NewPersonalSyncService(store, bytes.Repeat([]byte{0x4d}, 32))
	if err != nil {
		t.Fatal(err)
	}
	quota, err := app.NewPersonalDataQuotaService(store)
	if err != nil {
		t.Fatal(err)
	}
	quotaUser, err := app.NewPersonalDataQuotaUserService(store)
	if err != nil {
		t.Fatal(err)
	}
	attachments, err := app.NewPersonalAttachmentService(store, quota)
	if err != nil {
		t.Fatal(err)
	}
	capture, err := app.NewRecordCaptureService(store)
	if err != nil {
		t.Fatal(err)
	}
	goals, err := app.NewGoalService(personalData, store)
	if err != nil {
		t.Fatal(err)
	}
	runtimeExecutor, err := app.NewRuntimeToolExecutor(store, credentials, personalMemoryPCFenceReader{}, &app.RuntimeControl{}, &app.WorkspaceSyncService{})
	if err != nil {
		t.Fatalf("build synthetic runtime with the isolated CredentialService authorizer: %v", err)
	}
	broker, err := app.NewCloudDSHToolBrokerWithPersonalData(store, runtimeExecutor, nil, nil,
		capture, personalData, personalSync, goals, attachments, nil)
	if err != nil {
		t.Fatal(err)
	}

	users, err := app.NewUserService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	conversations, err := app.NewConversationService(store, credentials)
	if err != nil {
		t.Fatal(err)
	}
	sourceConversation, _, err := conversations.Create(ctx, principal, app.CreateConversationCommand{
		Title: "M08 删除后搜索验收来源", IdempotencyKey: "m08-pc-source-conversation-01",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Keep an earlier personal-conversation message that contains the exact
	// content the memory will hold when it is deleted. Seed revision 1 through
	// this running Task so history records the real conversation-message source.
	sourceSubmission := newTestTaskSubmission(t, owner.ID, "", "m08-pc-source-task-01", personalMemoryPCRevisedText)
	sourceSubmission.Task.ConversationID, sourceSubmission.Message.ConversationID = sourceConversation.ID, sourceConversation.ID
	sourceSubmission.Task.ExecutionLocation = "server"
	sourceToken, err := store.GetTokenByID(ctx, principal.TokenID)
	if err != nil || sourceToken.OwnerID != owner.ID || sourceToken.Kind != identity.KindSession || sourceToken.RevokedAt != nil {
		t.Fatalf("load the owner session token for the source Task: token_id=%s owner=%s kind=%s revoked=%t err=%v",
			sourceToken.ID, sourceToken.OwnerID, sourceToken.Kind, sourceToken.RevokedAt != nil, err)
	}
	sourceSubmission.Task.Authorization = task.Authorization{
		Kind: string(identity.KindSession), TokenID: sourceToken.ID, Version: 1, ExpiresAt: &sourceToken.ExpiresAt,
		Scopes: []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)},
	}
	sourceCreated, err := store.SubmitTask(ctx, sourceSubmission)
	if err != nil {
		t.Fatalf("seed a searchable conversation containing the source text: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE conversation_messages SET delivery_status='applied' WHERE id=$1`, sourceCreated.Message.ID); err != nil {
		t.Fatalf("mark the synthetic source message as applied: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE conversation_message_routes SET state='routed' WHERE message_id=$1`, sourceCreated.Message.ID); err != nil {
		t.Fatalf("mark the synthetic source message as delivered: %v", err)
	}
	if _, err := personalData.ApplyPersonalMutation(ctx, principal, app.PersonalMutationCommand{
		MutationID: "m08-enable-memory-0001", ObjectType: personal.ObjectTypeSkillSetting, ObjectID: "builtin_memory",
		Body: json.RawMessage(`{"enabled":true}`),
	}); err != nil {
		t.Fatalf("enable personal memory for the synthetic source Task: %v", err)
	}
	sourceTask, found, err := store.ClaimNextTask(ctx, "m08-pc-memory-source", 25*time.Second)
	if err != nil || !found || sourceTask.ID != sourceCreated.Task.ID {
		t.Fatalf("claim source conversation Task: claimed=%t id=%s err=%v", found, sourceTask.ID, err)
	}
	sourceTask.AgentSessionID = "session-m08-memory-source"
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET agent_session_id=$2 WHERE id=$1`, sourceTask.ID, sourceTask.AgentSessionID); err != nil {
		t.Fatalf("bind the source Task to its synthetic DSH session: %v", err)
	}
	if _, err := store.CommitTaskProgress(ctx, personalMemoryPCMutationProgress(t, principal, personalData, sourceTask,
		personalMemoryPCSourceID, "m08_seed_memory", 0, json.RawMessage(personalMemoryPCOriginalBody))); err != nil {
		t.Fatalf("seed revision 1 memory from the source conversation Task: %v", err)
	}
	personalMemoryPCFinishSyntheticTask(t, ctx, store, sourceTask.ID)
	initialMemory, err := store.GetPersonalObject(ctx, principal, personal.ObjectTypeMemory, personalMemoryPCSourceID)
	if err != nil || initialMemory.Revision != 1 {
		t.Fatalf("initial source memory = %+v, err=%v", initialMemory, err)
	}
	var initialSourceKind, initialSourceID, initialSourceConversationID string
	if err := store.pool.QueryRow(ctx, `SELECT source_kind,source_id,source_conversation_id
FROM personal_memory_sources WHERE owner_id=$1 AND object_id=$2 AND object_revision=1`, owner.ID, personalMemoryPCSourceID).
		Scan(&initialSourceKind, &initialSourceID, &initialSourceConversationID); err != nil ||
		initialSourceKind != "conversation_message" || initialSourceID != sourceCreated.Message.ID ||
		initialSourceConversationID != sourceConversation.ID {
		t.Fatalf("revision 1 memory source = kind=%q id=%q conversation=%q err=%v; want the seeded source message",
			initialSourceKind, initialSourceID, initialSourceConversationID, err)
	}

	portalRoot, err := filepath.Abs(filepath.Clean("../../../../web/portal/dist"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(portalRoot, "index.html")); err != nil {
		t.Fatalf("locate the PC Portal build that make pc will refresh: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serviceInstanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatalf("read isolated service identity for PC capability checks: %v", err)
	}
	router, err := api.New(api.Deps{
		Hosts: nativeAcceptanceHosts{}, TLSAuthorizer: nativeAcceptanceTLS{}, Verifier: credentials,
		Auth: nativeAcceptanceAuth{credentials: credentials, users: users}, ClientLogin: clients,
		PersonalData: personalData, PersonalSync: personalSync, PersonalDataQuotaUser: quotaUser,
		PersonalAttachments: attachments, MCPDisabled: true,
	}, api.Options{
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), ServiceInstanceID: serviceInstanceID,
		APIMaxBodyBytes: 16 << 20, MCPMaxBodyBytes: 16 << 20,
		SessionTTL: 12 * time.Hour, HostScheme: "http", PortalDomain: listener.Addr().String(), HostSuffix: "hosts.invalid",
		PreviewPortMin: 1024, PortalFiles: os.DirFS(portalRoot),
	})
	if err != nil {
		t.Fatal(err)
	}
	var uiHistoryRequests atomic.Int64
	var uiHistoryResponsesMu sync.Mutex
	var uiHistoryResponses []personalMemoryPCHistoryHTTPResponse
	uiHistoryEvidence := make(chan personalMemoryPCHistoryUIEvidence, 1)
	serverHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/__acceptance/personal-memory-history-ui-evidence" {
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
			var evidence personalMemoryPCHistoryUIEvidence
			if err := json.NewDecoder(r.Body).Decode(&evidence); err != nil {
				http.Error(w, "invalid PC history UI evidence", http.StatusBadRequest)
				return
			}
			select {
			case uiHistoryEvidence <- evidence:
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "PC history UI evidence already recorded", http.StatusConflict)
			}
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/personal-data/memory/"+personalMemoryPCSourceID+"/history" {
			uiHistoryRequests.Add(1)
			capture := &personalMemoryPCResponseRecorder{ResponseWriter: w}
			router.ServeHTTP(capture, r)
			uiHistoryResponsesMu.Lock()
			uiHistoryResponses = append(uiHistoryResponses, personalMemoryPCHistoryHTTPResponse{
				Status: capture.status, Body: capture.body.String(),
			})
			uiHistoryResponsesMu.Unlock()
			return
		}
		router.ServeHTTP(w, r)
	})
	server := &http.Server{Handler: serverHandler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
		if serveErr := <-serverDone; !errors.Is(serveErr, http.ErrServerClosed) {
			t.Errorf("stop loopback Personal Data API: %v", serveErr)
		}
	})

	profileDir := filepath.Join(t.TempDir(), "morde-m08-pc-user-data")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pc, err := startPersonalMemoryPCForAcceptance(t, profileDir)
	if err != nil {
		t.Fatalf("start isolated morde-pc: %v", err)
	}
	t.Cleanup(func() { stopPersonalMemoryPCForAcceptance(t, pc) })
	localPortal := "http://" + listener.Addr().String()
	personalMemoryPCStage(t, "真实 PC 已启动：Electron PID=%d，make pc PID=%d，MORDE_TEST_USER_DATA=%s", pc.electronPID, pc.command.Process.Pid, profileDir)
	personalMemoryPCStage(t, "仅使用 loopback 登录：LOCAL_PORTAL=%s TEST_USER=%s TEST_PASSWORD=client-password-123；进入个人数据 > 记忆。把现有记忆改成“%s”并保存。点击“历史版本”前记录 MemoryRow 与按钮的 AX ID，点击后观察 aria-expanded 和加载状态。", localPortal, owner.Name, personalMemoryPCCurrentText)

	var editedMemory personal.Object
	waitPersonalMemoryPC(t, ctx, "PC 保存记忆第二版", func() (bool, error) {
		value, readErr := store.GetPersonalObject(ctx, principal, personal.ObjectTypeMemory, personalMemoryPCSourceID)
		if domain.CodeOf(readErr) == domain.CodeNotFound {
			return false, nil
		}
		if readErr != nil {
			return false, readErr
		}
		if value.Revision < 2 {
			return false, nil
		}
		editedMemory = value
		return true, nil
	})
	if editedMemory.Revision != 2 || !strings.Contains(string(editedMemory.Body), personalMemoryPCCurrentText) {
		t.Fatalf("PC edited memory = revision %d body %s; want revision 2 with the acceptance text", editedMemory.Revision, editedMemory.Body)
	}
	waitPersonalMemoryPC(t, ctx, "PC 打开记忆历史 API", func() (bool, error) {
		uiHistoryResponsesMu.Lock()
		defer uiHistoryResponsesMu.Unlock()
		return len(uiHistoryResponses) > 0, nil
	})
	uiHistoryResponsesMu.Lock()
	historyHTTP := uiHistoryResponses[0]
	uiHistoryResponsesMu.Unlock()
	var historyHTTPPage struct {
		Items []struct {
			Revision             int64           `json:"revision"`
			SupersededByRevision *int64          `json:"superseded_by_revision"`
			Body                 json.RawMessage `json:"body"`
			Sources              []struct {
				Kind           string `json:"kind"`
				ID             string `json:"id"`
				ConversationID string `json:"conversation_id"`
			} `json:"sources"`
		} `json:"items"`
	}
	if historyHTTP.Status != http.StatusOK || json.Unmarshal([]byte(historyHTTP.Body), &historyHTTPPage) != nil ||
		len(historyHTTPPage.Items) != 2 || historyHTTPPage.Items[0].Revision != 2 ||
		historyHTTPPage.Items[1].Revision != 1 || historyHTTPPage.Items[1].SupersededByRevision == nil ||
		*historyHTTPPage.Items[1].SupersededByRevision != 2 || len(historyHTTPPage.Items[1].Sources) != 1 ||
		historyHTTPPage.Items[1].Sources[0].Kind != "conversation_message" ||
		historyHTTPPage.Items[1].Sources[0].ID != sourceCreated.Message.ID ||
		historyHTTPPage.Items[1].Sources[0].ConversationID != sourceConversation.ID {
		t.Fatalf("PC history route response status=%d body=%s; want HTTP 200 with revisions 2 and 1 -> 2",
			historyHTTP.Status, historyHTTP.Body)
	}
	personalMemoryPCStage(t, "PC history API response: status=%d body=%s", historyHTTP.Status, historyHTTP.Body)
	history, err := personalSync.GetMemoryHistory(ctx, principal, personal.ObjectTypeMemory, personalMemoryPCSourceID, "", 25)
	if err != nil || len(history.Items) != 2 || history.Items[0].Revision != 2 || history.Items[0].SupersededByRevision != nil ||
		history.Items[1].Revision != 1 || history.Items[1].SupersededByRevision == nil || *history.Items[1].SupersededByRevision != 2 ||
		!strings.Contains(string(history.Items[1].Body), "我希望先看详细分析") {
		t.Fatalf("memory history direct successor = %+v, err=%v; want revision 1 -> 2", history.Items, err)
	}
	personalMemoryPCAssertHistoryUI(t, localPortal+"/__acceptance/personal-memory-history-ui-evidence", uiHistoryEvidence, uiHistoryRequests.Load())
	personalMemoryPCStage(t, "服务端记录 revision 1 -> 2，PC 历史版本 UI 已由上一步实际 AX 证据验收。请在记忆行点“提议为规则”，再点“保存为候选”，不要确认启用。")

	pendingCandidate := waitPersonalMemoryPCRuleCandidate(t, ctx, personalSync, principal, personalMemoryPCSourceID, personal.RuleCandidatePending)
	pendingContext := personalMemoryPCInitialContext(t, ctx, store, conversations, principal, owner.ID, broker, "pending")
	if m08ContextHasRuleCandidate(pendingContext, pendingCandidate.ObjectID) {
		t.Fatalf("pending rule candidate %s entered the synthetic DSH initial Task context", pendingCandidate.ObjectID)
	}
	personalMemoryPCStage(t, "候选保持待审核；合成服务端 InitialContext 未包含该候选。现在在 PC 点“确认并启用”并确认弹窗。")

	activeCandidate := waitPersonalMemoryPCRuleCandidate(t, ctx, personalSync, principal, personalMemoryPCSourceID, personal.RuleCandidateActive)
	activeContext := personalMemoryPCInitialContext(t, ctx, store, conversations, principal, owner.ID, broker, "active")
	if !m08ContextHasActiveRuleCandidate(activeContext, activeCandidate.ObjectID, personalMemoryPCSourceID, 2, personalMemoryPCCurrentText) {
		t.Fatalf("confirmed rule candidate %s did not enter the synthetic DSH initial Task context", activeCandidate.ObjectID)
	}
	beforeDeleteSearch := personalMemoryPCSearchHistory(t, ctx, store, conversations, principal, owner.ID, broker, "before-delete")
	if !strings.Contains(beforeDeleteSearch, sourceCreated.Message.ID) || !strings.Contains(beforeDeleteSearch, personalMemoryPCSearchQuery) {
		t.Fatalf("synthetic DSH history search did not find the source conversation before deletion: %s", beforeDeleteSearch)
	}
	personalMemoryPCStage(t, "服务端 broker 已确认 active 候选进入合成 Task context，且删除前会话搜索能找到来源。现在把来源记忆改为“%s”并保存。", personalMemoryPCRevisedText)

	var revisedMemory personal.Object
	waitPersonalMemoryPC(t, ctx, "PC 更新候选来源记忆", func() (bool, error) {
		value, readErr := store.GetPersonalObject(ctx, principal, personal.ObjectTypeMemory, personalMemoryPCSourceID)
		if domain.CodeOf(readErr) == domain.CodeNotFound {
			return false, nil
		}
		if readErr != nil {
			return false, readErr
		}
		if value.Revision < 3 {
			return false, nil
		}
		revisedMemory = value
		return true, nil
	})
	if revisedMemory.Revision != 3 || !strings.Contains(string(revisedMemory.Body), personalMemoryPCRevisedText) {
		t.Fatalf("revised source memory = revision %d body %s; want revision 3", revisedMemory.Revision, revisedMemory.Body)
	}
	staleContext := personalMemoryPCInitialContext(t, ctx, store, conversations, principal, owner.ID, broker, "stale-source")
	if m08ContextHasRuleCandidate(staleContext, activeCandidate.ObjectID) {
		t.Fatalf("active rule candidate %s remained in context after its source memory moved from revision 2 to 3", activeCandidate.ObjectID)
	}
	personalMemoryPCStage(t, "候选来源已从第 2 版变为第 3 版，合成 Task context 已排除该候选。现在在 PC 删除这条记忆并确认删除提示。")

	waitPersonalMemoryPC(t, ctx, "PC 删除记忆", func() (bool, error) {
		_, readErr := store.GetPersonalObject(ctx, principal, personal.ObjectTypeMemory, personalMemoryPCSourceID)
		if domain.CodeOf(readErr) == domain.CodeNotFound {
			return true, nil
		}
		return false, readErr
	})
	if _, err := personalSync.GetMemoryHistory(ctx, principal, personal.ObjectTypeMemory, personalMemoryPCSourceID, "", 25); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("deleted memory history error = %v; want not_found", err)
	}
	var retainedBodies int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM personal_memory_history
WHERE owner_id=$1 AND object_id=$2 AND body IS NOT NULL`, owner.ID, personalMemoryPCSourceID).Scan(&retainedBodies); err != nil || retainedBodies != 0 {
		t.Fatalf("deleted memory retained %d historical bodies, err=%v", retainedBodies, err)
	}
	deletedContext := personalMemoryPCInitialContext(t, ctx, store, conversations, principal, owner.ID, broker, "deleted")
	if m08ContextHasRuleCandidate(deletedContext, activeCandidate.ObjectID) || strings.Contains(deletedContext, personalMemoryPCCurrentText) ||
		strings.Contains(deletedContext, personalMemoryPCRevisedText) {
		t.Fatalf("deleted memory or its rule candidate remained in the synthetic DSH initial Task context")
	}
	afterDeleteSearch := personalMemoryPCSearchHistory(t, ctx, store, conversations, principal, owner.ID, broker, "after-delete")
	if strings.Contains(afterDeleteSearch, personalMemoryPCSearchQuery) || strings.Contains(afterDeleteSearch, sourceCreated.Message.ID) {
		t.Fatalf("synthetic DSH history search recalled the suppressed deleted-memory source: %s", afterDeleteSearch)
	}
	personalMemoryPCStage(t, "完成：PC UI 经过 loopback API 删除记忆；服务端历史正文已清空，删除后的合成会话搜索没有来源命中。UI 证据与合成 broker 证据分开记录。")
	uiHistoryResponsesMu.Lock()
	capturedHistoryResponses := append([]personalMemoryPCHistoryHTTPResponse(nil), uiHistoryResponses...)
	uiHistoryResponsesMu.Unlock()
	t.Logf("PC history API captured %d response(s) from %d request(s)", len(capturedHistoryResponses), uiHistoryRequests.Load())
	for index, response := range capturedHistoryResponses {
		t.Logf("PC history API response %d: status=%d body=%s", index+1, response.Status, response.Body)
	}
	t.Logf("PC UI history route requests=%d; synthetic server checks: pending/active/stale context and pre/post-delete history search passed",
		uiHistoryRequests.Load())
	t.Log("PC history UI rows were asserted from live accessibility evidence; the HTTP response alone is not treated as a rendering pass.")
}

type personalMemoryPCHistoryUIEvidence struct {
	HistoryButtonExpanded bool    `json:"history_button_expanded"`
	HistoryStatusText     string  `json:"history_status_text"`
	RowIDBefore           string  `json:"row_id_before"`
	RowIDAfter            string  `json:"row_id_after"`
	HistoryButtonIDBefore string  `json:"history_button_id_before"`
	HistoryButtonIDAfter  string  `json:"history_button_id_after"`
	RevisionNumbers       []int64 `json:"revision_numbers"`
	HistoryLoading        bool    `json:"history_loading"`
	LoadHistoryCalls      int64   `json:"load_history_calls"`
	HistoryError          string  `json:"history_error"`
}

func personalMemoryPCAssertHistoryUI(t *testing.T, reportURL string, evidenceChannel <-chan personalMemoryPCHistoryUIEvidence, historyRequestCount int64) {
	t.Helper()
	personalMemoryPCStage(t, "请检查真实 morde-pc 当前记忆行（含内部滚动区域），并将 AX 证据 POST 到 %s。JSON 字段：history_button_expanded、history_status_text、row_id_before/after、history_button_id_before/after、revision_numbers、history_loading、load_history_calls、history_error。状态文本预期为“已读取 2 个历史版本。”，卡片版本为 [2,1]，展开=true，加载结束、行/按钮 ID 不变、调用次数=%d；未渲染或出现错误会使验收失败。", reportURL, historyRequestCount)
	var evidence personalMemoryPCHistoryUIEvidence
	select {
	case evidence = <-evidenceChannel:
	case <-time.After(10 * time.Minute):
		t.Fatalf("timed out waiting for live PC history AX evidence from %s", reportURL)
	}
	evidenceJSON, _ := json.Marshal(evidence)
	t.Logf("PC history UI accessibility evidence: %s", evidenceJSON)
	if !evidence.HistoryButtonExpanded || evidence.HistoryStatusText != "已读取 2 个历史版本。" ||
		evidence.RowIDBefore == "" || evidence.RowIDBefore != evidence.RowIDAfter ||
		evidence.HistoryButtonIDBefore == "" || evidence.HistoryButtonIDBefore != evidence.HistoryButtonIDAfter ||
		len(evidence.RevisionNumbers) != 2 || evidence.RevisionNumbers[0] != 2 || evidence.RevisionNumbers[1] != 1 ||
		evidence.HistoryLoading || evidence.LoadHistoryCalls != historyRequestCount || historyRequestCount != 1 ||
		evidence.HistoryError != "" {
		t.Fatalf("live PC history UI did not satisfy the rendering acceptance: %+v; request_count=%d", evidence, historyRequestCount)
	}
}

type personalMemoryPCProcess struct {
	command     *exec.Cmd
	electronPID int
	done        chan error
}

type personalMemoryPCFenceReader struct{}

type personalMemoryPCHistoryHTTPResponse struct {
	Status int
	Body   string
}

type personalMemoryPCResponseRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *personalMemoryPCResponseRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *personalMemoryPCResponseRecorder) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(body)
	if n > 0 {
		_, _ = w.body.Write(body[:n])
	}
	return n, err
}

func (personalMemoryPCFenceReader) ReadTaskFence(context.Context, string, string) (app.AgentHostFence, error) {
	return app.AgentHostFence{}, nil
}

func startPersonalMemoryPCForAcceptance(t *testing.T, profileDir string) (*personalMemoryPCProcess, error) {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Clean("../../../../"))
	if err != nil {
		return nil, err
	}
	personalMemoryPCStage(t, "通过 make pc 重建当前 Portal 与 PC 客户端；MORDE_TEST_USER_DATA 指向隔离 profile=%s。", profileDir)
	command := exec.Command("make", "pc")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Dir = repoRoot
	command.Env = personalMemoryPCIsolatedClientEnvironment(profileDir)
	command.Stdout, command.Stderr = os.Stderr, os.Stderr
	if err := command.Start(); err != nil {
		return nil, err
	}
	if personalMemoryPCProtectedPID(command.Process.Pid) {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		return nil, fmt.Errorf("safety refusal: isolated morde-pc launcher unexpectedly matched an existing process PID %d", command.Process.Pid)
	}
	process := &personalMemoryPCProcess{command: command, done: make(chan error, 1)}
	go func() { process.done <- command.Wait() }()
	electronPID, err := waitForPersonalMemoryPCElectronPID(process, 5*time.Minute)
	if err != nil {
		stopPersonalMemoryPCForAcceptance(t, process)
		return nil, err
	}
	if personalMemoryPCProtectedPID(electronPID) {
		stopPersonalMemoryPCForAcceptance(t, process)
		return nil, fmt.Errorf("safety refusal: make pc Electron unexpectedly matched an existing process PID %d", electronPID)
	}
	process.electronPID = electronPID
	return process, nil
}

func waitForPersonalMemoryPCElectronPID(process *personalMemoryPCProcess, timeout time.Duration) (int, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		pid, err := personalMemoryPCElectronPID(process.command.Process.Pid)
		if err != nil {
			return 0, fmt.Errorf("find Electron launched by make pc: %w", err)
		}
		if pid != 0 {
			return pid, nil
		}
		select {
		case exitErr := <-process.done:
			process.done <- exitErr
			if exitErr != nil {
				return 0, fmt.Errorf("make pc exited before launching Electron: %w", exitErr)
			}
			return 0, errors.New("make pc exited successfully before launching Electron")
		case <-ticker.C:
		case <-deadline.C:
			return 0, fmt.Errorf("make pc did not launch Electron within %s", timeout)
		}
	}
}

func personalMemoryPCElectronPID(processGroup int) (int, error) {
	output, err := exec.Command("ps", "-axo", "pid=,pgid=,command=").Output()
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		groupID, groupErr := strconv.Atoi(fields[1])
		if pidErr != nil || groupErr != nil || groupID != processGroup {
			continue
		}
		if strings.EqualFold(filepath.Base(fields[2]), "electron") {
			return pid, nil
		}
	}
	return 0, nil
}

func personalMemoryPCProtectedPID(pid int) bool {
	return pid == 26213 || pid == 26241 || pid == 63434
}

func personalMemoryPCIsolatedClientEnvironment(profileDir string) []string {
	allowed := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, "LC_ALL": true,
		"USER": true, "LOGNAME": true, "DISPLAY": true, "WAYLAND_DISPLAY": true, "XDG_RUNTIME_DIR": true}
	environment := make([]string, 0, len(allowed)+1)
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if ok && allowed[name] {
			environment = append(environment, entry)
		}
	}
	return append(environment, "MORDE_TEST_USER_DATA="+profileDir)
}

func stopPersonalMemoryPCForAcceptance(t *testing.T, process *personalMemoryPCProcess) {
	t.Helper()
	if process == nil || process.command == nil || process.command.Process == nil ||
		personalMemoryPCProtectedPID(process.command.Process.Pid) || personalMemoryPCProtectedPID(process.electronPID) {
		return
	}
	processGroup := -process.command.Process.Pid
	_ = syscall.Kill(processGroup, syscall.SIGINT)
	select {
	case exitErr := <-process.done:
		if exitErr != nil {
			t.Logf("isolated morde-pc process exited while stopping: %v", exitErr)
		}
		_ = syscall.Kill(processGroup, syscall.SIGKILL)
		return
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(processGroup, syscall.SIGKILL)
		select {
		case exitErr := <-process.done:
			if exitErr != nil {
				t.Logf("isolated morde-pc process exited after forced stop: %v", exitErr)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("isolated morde-pc PID %d did not stop after the acceptance", process.command.Process.Pid)
		}
	}
}

func personalMemoryPCStage(t *testing.T, format string, arguments ...any) {
	t.Helper()
	message := fmt.Sprintf(format, arguments...)
	t.Log(message)
	_, _ = fmt.Fprintf(os.Stderr, "M08 PC ACCEPTANCE: %s\n", message)
}

func waitPersonalMemoryPC(t *testing.T, ctx context.Context, description string, condition func() (bool, error)) {
	t.Helper()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		ready, err := condition()
		if err != nil {
			t.Fatalf("%s: %v", description, err)
		}
		if ready {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s: %v", description, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitPersonalMemoryPCRuleCandidate(t *testing.T, ctx context.Context, syncService *app.PersonalSyncService,
	principal identity.Principal, sourceMemoryID, status string) personal.Object {
	t.Helper()
	var found personal.Object
	waitPersonalMemoryPC(t, ctx, "PC rule candidate status="+status, func() (bool, error) {
		cursor := ""
		for pageNumber := 0; pageNumber < 10; pageNumber++ {
			page, err := syncService.GetSnapshot(ctx, principal, cursor, 100)
			if err != nil {
				return false, err
			}
			for _, object := range page.Items {
				if object.ObjectType != personal.ObjectTypeRuleCandidate {
					continue
				}
				var body personal.RuleCandidateBody
				if json.Unmarshal(object.Body, &body) != nil || body.SourceMemoryID != sourceMemoryID || body.Status != status {
					continue
				}
				found = object
				return true, nil
			}
			if page.NextCursor == "" {
				return false, nil
			}
			cursor = page.NextCursor
		}
		return false, fmt.Errorf("personal-data snapshot exceeded the local acceptance page bound")
	})
	return found
}

func personalMemoryPCInitialContext(t *testing.T, ctx context.Context, store *Store, conversations *app.ConversationService,
	principal identity.Principal, ownerID string, broker *app.CloudDSHToolBroker, label string) string {
	t.Helper()
	claimed := personalMemoryPCClaimSyntheticTask(t, ctx, store, conversations, principal, ownerID,
		"m08-context-"+label, "M08 synthetic context check: "+label)
	value, err := broker.InitialContext(ctx, claimed)
	if err != nil {
		t.Fatalf("build synthetic DSH InitialContext for %s: %v", label, err)
	}
	personalMemoryPCFinishSyntheticTask(t, ctx, store, claimed.ID)
	return value
}

func personalMemoryPCClaimSyntheticTask(t *testing.T, ctx context.Context, store *Store, conversations *app.ConversationService,
	principal identity.Principal, ownerID, key, objective string) task.Task {
	t.Helper()
	if principal.UserID != ownerID || principal.TokenID == "" {
		t.Fatalf("synthetic Task session does not match owner %s", ownerID)
	}
	conversationValue, _, err := conversations.Create(ctx, principal, app.CreateConversationCommand{
		Title: "M08 合成 DSH 证据 " + key, IdempotencyKey: key + "-conversation",
	})
	if err != nil {
		t.Fatalf("create synthetic personal conversation %s: %v", key, err)
	}
	submission := newTestTaskSubmission(t, ownerID, "", key, objective)
	submission.Task.ConversationID, submission.Message.ConversationID = conversationValue.ID, conversationValue.ID
	submission.Task.ExecutionLocation = "server"
	token, err := store.GetTokenByID(ctx, principal.TokenID)
	if err != nil || token.OwnerID != ownerID || token.Kind != identity.KindSession || token.RevokedAt != nil {
		t.Fatalf("load the authorized local session token for synthetic Task: token_id=%s owner=%s kind=%s revoked=%t err=%v",
			token.ID, token.OwnerID, token.Kind, token.RevokedAt != nil, err)
	}
	submission.Task.Authorization = task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit synthetic server Task %s: %v", key, err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "m08-pc-acceptance", 25*time.Second)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim synthetic server Task %s: claimed=%t id=%s err=%v", key, found, claimed.ID, err)
	}
	return claimed
}

func personalMemoryPCFinishSyntheticTask(t *testing.T, ctx context.Context, store *Store, taskID string) {
	t.Helper()
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status=$2,finished_at=clock_timestamp(),lease_owner='',lease_expires_at=NULL,
updated_at=clock_timestamp() WHERE id=$1 AND status='running'`, taskID, string(task.StatusCompleted)); err != nil {
		t.Fatalf("finish synthetic Task %s: %v", taskID, err)
	}
}

func personalMemoryPCMutationProgress(t *testing.T, principal identity.Principal, service *app.PersonalMutationService,
	claimed task.Task, objectID, callID string, baseRevision int64, body json.RawMessage) app.TaskProgress {
	t.Helper()
	mutationID := postgresCloudDSHPersonalMutationID(claimed.ID, claimed.AgentSessionID, callID)
	mutation, err := service.PreparePersonalMutation(principal, app.PersonalMutationCommand{
		MutationID: mutationID, ObjectType: personal.ObjectTypeMemory, ObjectID: objectID,
		BaseRevision: baseRevision, Body: body,
	})
	if err != nil {
		t.Fatal(err)
	}
	commit := app.CloudDSHPersonalMutationCommit{
		TaskID: claimed.ID, OwnerID: claimed.OwnerID, AgentSessionID: claimed.AgentSessionID,
		AuthorizationVersion: claimed.Authorization.Version, CallID: callID, Mutation: mutation,
	}
	commit.FingerprintSHA256 = postgresCloudDSHPersonalMutationFingerprint(commit)
	stepID, err := domain.NewStepID()
	if err != nil {
		t.Fatal(err)
	}
	step := task.Step{ID: stepID, TaskID: claimed.ID, Sequence: 1, Kind: "tool", Status: task.StepSucceeded,
		Title: "Update personal data", Summary: "Saved an authorized personal object with revision checking."}
	usage := claimed.Usage
	usage.ToolCalls++
	detail := map[string]any{
		"tool": "morde_personal_object_update", "personal_call_id": callID,
		"personal_mutation_id": mutation.MutationID, "agent_session_id": commit.AgentSessionID,
		"authorization_version": commit.AuthorizationVersion, "object_type": mutation.ObjectType,
		"object_id": mutation.ObjectID, "revision": mutation.BaseRevision + 1,
		"request_sha256": fmt.Sprintf("%x", mutation.RequestHash), "fingerprint_sha256": commit.FingerprintSHA256,
	}
	payload, err := json.Marshal(map[string]any{
		"step_id": step.ID, "sequence": step.Sequence, "kind": step.Kind, "status": step.Status,
		"title": step.Title, "summary": step.Summary, "usage_model_tokens_known": false,
		"usage_active_seconds": usage.ActiveSeconds, "usage_tool_calls": usage.ToolCalls, "detail": detail,
	})
	if err != nil {
		t.Fatal(err)
	}
	return app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusRunning, CurrentStep: step.ID, Usage: usage, Steps: []task.Step{step},
		Event: task.Event{TaskID: claimed.ID, Type: "task.step", Payload: payload}, CloudDSHPersonalMutation: &commit,
	}
}

type personalMemoryPCContextObject struct {
	Type     string          `json:"type"`
	ID       string          `json:"id"`
	Revision int64           `json:"revision"`
	Body     json.RawMessage `json:"body"`
}

func personalMemoryPCContextObjects(value string) []personalMemoryPCContextObject {
	objects := make([]personalMemoryPCContextObject, 0)
	for _, line := range strings.Split(value, "\n") {
		var object personalMemoryPCContextObject
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &object) == nil && object.Type != "" && object.ID != "" {
			objects = append(objects, object)
		}
	}
	return objects
}

func m08ContextHasRuleCandidate(contextValue, candidateID string) bool {
	for _, object := range personalMemoryPCContextObjects(contextValue) {
		if object.Type == personal.ObjectTypeRuleCandidate && object.ID == candidateID {
			return true
		}
	}
	return false
}

func m08ContextHasActiveRuleCandidate(contextValue, candidateID, sourceMemoryID string, sourceRevision int64, expectedText string) bool {
	for _, object := range personalMemoryPCContextObjects(contextValue) {
		if object.Type != personal.ObjectTypeRuleCandidate || object.ID != candidateID {
			continue
		}
		var body struct {
			Status               string `json:"status"`
			SourceMemoryID       string `json:"source_memory_id"`
			SourceMemoryRevision int64  `json:"source_memory_revision"`
			Content              string `json:"content"`
		}
		return json.Unmarshal(object.Body, &body) == nil && body.Status == personal.RuleCandidateActive &&
			body.SourceMemoryID == sourceMemoryID && body.SourceMemoryRevision == sourceRevision && body.Content == expectedText
	}
	return false
}

func personalMemoryPCSearchHistory(t *testing.T, ctx context.Context, store *Store, conversations *app.ConversationService,
	principal identity.Principal, ownerID string, broker *app.CloudDSHToolBroker, label string) string {
	t.Helper()
	claimed := personalMemoryPCClaimSyntheticTask(t, ctx, store, conversations, principal, ownerID,
		"m08-search-"+label, "M08 synthetic DSH history search: "+label)
	key := make([]byte, 32)
	key[0] = 0x4d
	authenticator, err := app.NewAgentStopAuthenticator("supervisor-1", key)
	if err != nil {
		t.Fatal(err)
	}
	agents, err := app.NewAgentInstanceService(store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	var nonce [32]byte
	copy(nonce[:], []byte("m08-"+label))
	agentInstanceID, agentSessionID := "agent-"+label, "session-"+label
	started, _, err := agents.Start(ctx, app.AgentInstanceStart{
		Fence: app.AgentInstanceFence{TaskID: claimed.ID, OwnerID: ownerID, InitiatorID: claimed.InitiatorID,
			AgentInstanceID: agentInstanceID, AgentSessionID: agentSessionID, SupervisorID: "supervisor-1",
			LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
			AuthorizationVersion: claimed.Authorization.Version, EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1"},
		SupervisorNonce: nonce[:], LeaseDuration: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("start synthetic DSH Agent instance for history search: %v", err)
	}
	details, err := store.GetTask(ctx, claimed.ID, ownerID)
	if err != nil {
		t.Fatalf("read synthetic history-search Task after Agent start: %v", err)
	}
	conversationContext := claimed.ConversationContext
	claimed = details.Task
	claimed.ConversationContext = conversationContext
	if claimed.ExecutionLocation != "server" || claimed.InitiatorID != claimed.OwnerID || claimed.ConversationContext == nil ||
		claimed.ConversationContext.Scope != "personal" || claimed.ConversationContext.OwnerID != claimed.OwnerID ||
		claimed.ConversationContext.ConversationID != claimed.ConversationID || claimed.Authorization.TokenID == "" ||
		claimed.Authorization.ExpiresAt == nil || !claimed.Authorization.ExpiresAt.After(time.Now()) ||
		claimed.AgentSessionID != agentSessionID {
		t.Fatalf("synthetic history-search Task lost its personal authorization/context: execution=%q owner=%s initiator=%s conversation=%s context=%+v authorization_kind=%q token_id_present=%t expires_at_present=%t agent_session=%s",
			claimed.ExecutionLocation, claimed.OwnerID, claimed.InitiatorID, claimed.ConversationID, claimed.ConversationContext,
			claimed.Authorization.Kind, claimed.Authorization.TokenID != "", claimed.Authorization.ExpiresAt != nil, claimed.AgentSessionID)
	}
	fence := started.Fence
	taskFence := &app.ModelTaskFence{TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		AgentInstanceID: fence.AgentInstanceID, AgentGeneration: fence.AgentGeneration,
		AgentSessionID: fence.AgentSessionID, AgentRootSessionID: fence.AgentSessionID,
		AuthorizationVersion: claimed.Authorization.Version, ThroughSequence: claimed.CheckpointThroughSequence,
		EngineVersion: fence.EngineVersion, ProfileVersion: fence.ProfileVersion}
	result, err := broker.Execute(ctx, claimed, app.ModelToolCall{
		CallID: "m08-history-search-" + label, Name: "morde_conversation_history_search",
		Arguments: `{"query":"` + personalMemoryPCSearchQuery + `","limit":10,"cursor":null}`,
		TaskFence: taskFence,
	})
	if err != nil || result.Status != app.AgentToolSucceeded {
		t.Fatalf("synthetic DSH history search %s: status=%q output=%s err=%v", label, result.Status, result.Output, err)
	}
	personalMemoryPCFinishSyntheticTask(t, ctx, store, claimed.ID)
	return result.Output
}
