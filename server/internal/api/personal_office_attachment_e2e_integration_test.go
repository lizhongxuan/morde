package api

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"
	"morde/server/internal/store/postgres"
)

const (
	personalOfficeDOCXMediaType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	personalOfficeXLSXMediaType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	personalOfficePPTXMediaType = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
)

// TestPersonalOfficeAttachmentUploadTaskReaderE2E exercises the user upload
// endpoints, PostgreSQL-backed attachment persistence, Task message linkage,
// the DSH attachment tool, and account isolation using only synthetic files.
func TestPersonalOfficeAttachmentUploadTaskReaderE2E(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, pool := newPersonalOfficeAttachmentTestStore(t, ctx)

	owner, ownerPrincipal := createPersonalOfficeAttachmentTestUser(t, ctx, store)
	_, otherPrincipal := createPersonalOfficeAttachmentTestUser(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatalf("enable synthetic Task execution: %v", err)
	}

	personalData, err := app.NewPersonalMutationService(store)
	if err != nil {
		t.Fatal(err)
	}
	quota, err := app.NewPersonalDataQuotaService(store)
	if err != nil {
		t.Fatal(err)
	}
	attachmentService, err := app.NewPersonalAttachmentService(store, quota)
	if err != nil {
		t.Fatal(err)
	}
	router := personalOfficeAttachmentRouter(ownerPrincipal, attachmentService)
	otherRouter := personalOfficeAttachmentRouter(otherPrincipal, attachmentService)

	fixtures := []struct {
		objectID, fileName, mediaType, key, marker string
		content                                    []byte
	}{
		{objectID: "library-item_e2e-docx", fileName: "synthetic-report.docx", mediaType: personalOfficeDOCXMediaType,
			key: "office-e2e-docx-upload-001", marker: "DOCX synthetic finding: launch window is 2026-11-09.",
			content: personalOfficeZip(t, map[string]string{
				"[Content_Types].xml": `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"></Types>`,
				"word/document.xml":   `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>DOCX synthetic finding: launch window is 2026-11-09.</w:t></w:r></w:p></w:body></w:document>`,
			})},
		{objectID: "library-item_e2e-xlsx", fileName: "synthetic-metrics.xlsx", mediaType: personalOfficeXLSXMediaType,
			key: "office-e2e-xlsx-upload-001", marker: "A1=XLSX synthetic metric: signups=42",
			content: personalOfficeZip(t, map[string]string{
				"xl/workbook.xml":          `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"></workbook>`,
				"xl/sharedStrings.xml":     `<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><si><t>XLSX synthetic metric: signups=42</t></si></sst>`,
				"xl/worksheets/sheet1.xml": `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="1"><c r="A1" t="s"><v>0</v></c></row></sheetData></worksheet>`,
			})},
		{objectID: "library-item_e2e-pptx", fileName: "synthetic-briefing.pptx", mediaType: personalOfficePPTXMediaType,
			key: "office-e2e-pptx-upload-001", marker: "Slide 1:\nPPTX synthetic decision: proceed with staged rollout.",
			content: personalOfficeZip(t, map[string]string{
				"ppt/presentation.xml": `<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:sldIdLst><p:sldId id="1" r:id="rIdSlide1"/></p:sldIdLst></p:presentation>`,
				"ppt/_rels/presentation.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rIdSlide1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide1.xml"/></Relationships>`,
				"ppt/slides/slide1.xml": `<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><p:cSld><p:spTree><p:sp><p:txBody><a:p><a:r><a:t>PPTX synthetic decision: proceed with staged rollout.</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`,
			})},
	}

	attachmentIDs := make([]string, 0, len(fixtures))
	for _, fixture := range fixtures {
		body, err := json.Marshal(map[string]any{"name": fixture.fileName, "kind": "artifact"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := personalData.ApplyPersonalMutation(ctx, ownerPrincipal, app.PersonalMutationCommand{
			MutationID: "office-e2e-seed-" + strings.TrimPrefix(fixture.objectID, "library-item_"),
			ObjectType: personal.ObjectTypeLibraryItem, ObjectID: fixture.objectID, BaseRevision: 0, Body: body,
		}); err != nil {
			t.Fatalf("create synthetic library item %s: %v", fixture.fileName, err)
		}
		attachmentID := uploadPersonalOfficeAttachment(t, router, fixture.objectID, fixture.key, fixture.mediaType, fixture.content)
		attachmentIDs = append(attachmentIDs, attachmentID)

		// The attachment endpoint must not disclose an owner's document to another account.
		response := performPersonalOfficeRequest(otherRouter, http.MethodGet,
			"/api/personal-data/attachments/"+attachmentID, nil, nil)
		if response.Code != http.StatusNotFound {
			t.Fatalf("foreign owner attachment read status=%d body=%s; want 404", response.Code, response.Body.String())
		}
	}

	modelID := "p1234567"
	authorizer := personalOfficeTaskAuthorizer{}
	taskService, err := app.NewTaskService(store, authorizer, personalOfficeModelCatalog{})
	if err != nil {
		t.Fatal(err)
	}
	submission, err := taskService.Submit(ctx, ownerPrincipal, app.SubmitTaskCommand{
		AttachmentIDs: attachmentIDs, ModelID: modelID, Objective: "Summarize the attached synthetic office documents.",
		Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 8, ModelTokens: 4000}, IdempotencyKey: "office-e2e-task-submit-001",
	})
	if err != nil {
		t.Fatalf("submit attachment-linked Task: %v", err)
	}
	if submission.Message.TaskID != submission.Task.ID || len(submission.Message.AttachmentIDs) != len(attachmentIDs) {
		t.Fatalf("submitted message did not retain Task attachments: message=%+v task=%s", submission.Message, submission.Task.ID)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "office-attachment-e2e", time.Minute)
	if err != nil || !found || claimed.ID != submission.Task.ID {
		t.Fatalf("claim attachment Task: task=%s found=%t err=%v", claimed.ID, found, err)
	}
	stopAuthenticator, err := app.NewAgentStopAuthenticator("office-e2e-supervisor", bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, stopAuthenticator)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = instances.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
		TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		AgentInstanceID: "office-agent-" + claimed.ID, AgentSessionID: "office-session-" + claimed.ID,
		SupervisorID: "office-e2e-supervisor", LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		AuthorizationVersion: claimed.Authorization.Version, EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1",
	}, SupervisorNonce: bytes.Repeat([]byte{7}, 32), LeaseDuration: app.MaxAgentLease})
	if err != nil {
		t.Fatalf("start synthetic DSH Agent identity for Task: %v", err)
	}
	details, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil {
		t.Fatalf("reload Agent-bound Task: %v", err)
	}
	details.Task.ConversationContext = claimed.ConversationContext
	claimed = details.Task

	personalSync, err := app.NewPersonalSyncService(store, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	goals, err := app.NewGoalService(personalData, store)
	if err != nil {
		t.Fatal(err)
	}
	recordCapture, err := app.NewRecordCaptureService(store)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := app.NewCloudDSHToolBrokerWithPersonalData(store, &app.RuntimeToolExecutor{}, nil, nil, recordCapture,
		personalData, personalSync, goals, attachmentService, nil)
	if err != nil {
		t.Fatal(err)
	}

	for index, fixture := range fixtures {
		result, err := broker.Execute(ctx, claimed, app.ModelToolCall{
			CallID: fmt.Sprintf("office-attachment-read-%d", index+1), Name: "morde_read_task_attachment",
			Arguments: `{"attachment_id":"` + attachmentIDs[index] + `"}`,
		})
		if err != nil || result.Status != app.AgentToolSucceeded || !strings.Contains(result.Output, fixture.marker) {
			t.Fatalf("read %s through Task tool: status=%s output=%q err=%v", fixture.fileName, result.Status, result.Output, err)
		}
	}

	// An attachment ID owned by this account but absent from this Task is rejected
	// before its bytes can be exposed to the Task model.
	forgedAttachmentID := "pa_ffffffffffffffffffffffffffffffff"
	result, err := broker.Execute(ctx, claimed, app.ModelToolCall{
		CallID: "office-attachment-read-unlinked", Name: "morde_read_task_attachment",
		Arguments: `{"attachment_id":"` + forgedAttachmentID + `"}`,
	})
	if err == nil || result.Status == app.AgentToolSucceeded {
		t.Fatalf("unlinked attachment read unexpectedly succeeded: result=%+v err=%v", result, err)
	}
}

type personalOfficeModelCatalog struct{}

func (personalOfficeModelCatalog) IsConfiguredModel(context.Context, string, string) (bool, error) {
	return true, nil
}

type personalOfficeTaskAuthorizer struct{}

func (personalOfficeTaskAuthorizer) AuthorizeTaskAction(context.Context, string, identity.Kind, string, string, []identity.Scope, identity.Scope) error {
	return nil
}

func (personalOfficeTaskAuthorizer) AuthorizeTaskExecution(_ context.Context, principal identity.Principal, _ string, _ identity.Scope) (task.Authorization, error) {
	expires := time.Now().UTC().Add(time.Hour)
	return task.Authorization{Kind: string(principal.Kind), TokenID: principal.TokenID, Version: 1, ExpiresAt: &expires,
		Scopes: []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)}}, nil
}

func personalOfficeAttachmentRouter(principal identity.Principal, service personalAttachmentService) *gin.Engine {
	router := gin.New()
	group := router.Group("/api/personal-data", func(c *gin.Context) {
		c.Set(principalKey, principal)
		c.Next()
	})
	RegisterPersonalAttachmentRoutes(group, service)
	return router
}

func uploadPersonalOfficeAttachment(t *testing.T, router http.Handler, objectID, key, mediaType string, content []byte) string {
	t.Helper()
	digest := sha256.Sum256(content)
	requestBody, err := json.Marshal(personalAttachmentUploadRequest{
		ObjectID: objectID, BaseRevision: pointerInt64(1), SizeBytes: pointerInt64(int64(len(content))),
		SHA256: hex.EncodeToString(digest[:]), MediaType: mediaType,
	})
	if err != nil {
		t.Fatal(err)
	}
	created := performPersonalOfficeRequest(router, http.MethodPost, "/api/personal-data/uploads", requestBody,
		http.Header{"Content-Type": []string{"application/json"}, "Idempotency-Key": []string{key}})
	if created.Code != http.StatusOK {
		t.Fatalf("create office upload status=%d body=%s", created.Code, created.Body.String())
	}
	var upload personalAttachmentUploadResponse
	if err := json.Unmarshal(created.Body.Bytes(), &upload); err != nil || upload.UploadID == "" || upload.ChunkCount != 1 {
		t.Fatalf("decode office upload receipt=%+v err=%v body=%s", upload, err, created.Body.String())
	}

	chunkDigest := sha256.Sum256(content)
	chunkHeaders := http.Header{
		personalAttachmentChunkDigestHeader: []string{hex.EncodeToString(chunkDigest[:])},
		"Content-Length":                    []string{fmt.Sprint(len(content))},
	}
	chunk := performPersonalOfficeRequest(router, http.MethodPut,
		fmt.Sprintf("/api/personal-data/uploads/%s/chunks/0", upload.UploadID), content, chunkHeaders)
	if chunk.Code != http.StatusOK {
		t.Fatalf("upload office chunk status=%d body=%s", chunk.Code, chunk.Body.String())
	}

	commit := performPersonalOfficeRequest(router, http.MethodPost,
		"/api/personal-data/uploads/"+upload.UploadID+"/commit", nil,
		http.Header{"Idempotency-Key": []string{key + "-commit"}})
	if commit.Code != http.StatusOK {
		t.Fatalf("commit office upload status=%d body=%s", commit.Code, commit.Body.String())
	}
	var receipt personalAttachmentCommitResponse
	if err := json.Unmarshal(commit.Body.Bytes(), &receipt); err != nil || !personal.ValidPersonalAttachmentID(receipt.AttachmentID) {
		t.Fatalf("decode office attachment receipt=%+v err=%v body=%s", receipt, err, commit.Body.String())
	}

	read := performPersonalOfficeRequest(router, http.MethodGet,
		"/api/personal-data/attachments/"+receipt.AttachmentID, nil, nil)
	if read.Code != http.StatusOK || !bytes.Equal(read.Body.Bytes(), content) {
		t.Fatalf("read back persisted office file status=%d bytes=%d", read.Code, read.Body.Len())
	}
	return receipt.AttachmentID
}

func performPersonalOfficeRequest(handler http.Handler, method, path string, body []byte, headers http.Header) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func personalOfficeZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range entries {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatalf("create synthetic Office part %q: %v", name, err)
		}
		if _, err := io.WriteString(file, content); err != nil {
			t.Fatalf("write synthetic Office part %q: %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close synthetic Office archive: %v", err)
	}
	return buffer.Bytes()
}

func pointerInt64(value int64) *int64 { return &value }

func newPersonalOfficeAttachmentTestStore(t *testing.T, ctx context.Context) (*postgres.Store, *pgxpool.Pool) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CH_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("CH_TEST_DATABASE_URL is unset; PostgreSQL office attachment integration test was not run")
	}
	baseConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse CH_TEST_DATABASE_URL: %v", err)
	}
	adminPool, err := pgxpool.NewWithConfig(ctx, baseConfig.Copy())
	if err != nil {
		t.Fatalf("connect to local test PostgreSQL: %v", err)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	databaseName := "morde_office_e2e_" + hex.EncodeToString(random[:])
	if _, err := adminPool.Exec(ctx, fmt.Sprintf(`CREATE DATABASE "%s"`, databaseName)); err != nil {
		adminPool.Close()
		t.Fatalf("create temporary office test database: %v", err)
	}
	testConfig := baseConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testPool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, databaseName))
		adminPool.Close()
		t.Fatalf("connect to temporary office test database: %v", err)
	}
	if err := postgres.Migrate(ctx, testPool); err != nil {
		testPool.Close()
		_, _ = adminPool.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, databaseName))
		adminPool.Close()
		t.Fatalf("migrate temporary office test database: %v", err)
	}
	t.Cleanup(func() {
		testPool.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, fmt.Sprintf(`DROP DATABASE "%s" WITH (FORCE)`, databaseName)); err != nil {
			t.Errorf("drop temporary office test database: %v", err)
		}
		adminPool.Close()
	})
	return postgres.New(testPool), testPool
}

func createPersonalOfficeAttachmentTestUser(t *testing.T, ctx context.Context, store *postgres.Store) (identity.User, identity.Principal) {
	t.Helper()
	userID, err := domain.NewUserID()
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.CreateUser(ctx, identity.User{ID: userID, Name: "office-e2e-" + userID,
		PasswordHash: "synthetic-test-hash", Role: identity.RoleUser, Status: identity.StatusPending,
		QuotaHosts: 1, QuotaCPUMilli: 1000, QuotaMemMiB: 2048, DefaultHostCPUMilli: 1000, DefaultHostMemMiB: 2048})
	if err != nil {
		t.Fatalf("create synthetic user: %v", err)
	}
	user.Status = identity.StatusApproved
	user, err = store.UpdateUser(ctx, "system", identity.StatusPending, user)
	if err != nil {
		t.Fatalf("approve synthetic user: %v", err)
	}
	tokenID, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(24 * time.Hour)
	secretHash := sha256.Sum256([]byte("synthetic-session:" + tokenID))
	if _, err := store.CreateToken(ctx, identity.Token{ID: tokenID, OwnerID: user.ID, Kind: identity.KindSession,
		Name: "office attachment integration", SecretHash: secretHash[:], ExpiresAt: expires}); err != nil {
		t.Fatalf("create synthetic session token: %v", err)
	}
	return user, identity.Principal{UserID: user.ID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: tokenID}
}
