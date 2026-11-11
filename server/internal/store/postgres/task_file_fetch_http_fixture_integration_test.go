package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

// TestK4NodeHTTPFixture is an opt-in Node live-test fixture. It uses the same
// Postgres store and API router as production, but it is compiled only into the
// test binary and binds exclusively to loopback.
func TestK4NodeHTTPFixture(t *testing.T) {
	if os.Getenv("CH_K4_FIXTURE") != "1" {
		t.Skip("set CH_K4_FIXTURE=1 to run the local K4 HTTP fixture")
	}
	if strings.TrimSpace(os.Getenv("CH_TEST_DATABASE_URL")) == "" {
		t.Fatal("CH_K4_FIXTURE=1 requires CH_TEST_DATABASE_URL for a local PostgreSQL server")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, _ := newTemporaryStore(t)
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	targetID := mustID(t, domain.NewClientInstallationID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO client_installations (id,owner_id,display_name,platform,expires_at)
VALUES ($1,$2,'K4 synthetic PC','desktop',clock_timestamp()+interval '1 day')`, targetID, owner.ID); err != nil {
		t.Fatalf("insert K4 fixture installation: %v", err)
	}
	submission := newTestTaskSubmission(t, owner.ID, "", "k4-http-fixture", "Read a synthetic local report")
	submission.Task.ExecutionLocation = "server"
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit K4 fixture Task: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "k4-http-fixture-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim K4 fixture Task: found=%t task=%+v err=%v", found, claimed, err)
	}
	const revision int64 = 1
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET current_revision=$3,desired_revision=$3,status='running'
WHERE id=$1 AND owner_id=$2`, claimed.ID, owner.ID, revision); err != nil {
		t.Fatalf("set K4 fixture Task revision: %v", err)
	}

	operation := app.TaskHubOperation{
		OwnerID: owner.ID, TaskID: claimed.ID, Key: "agent:k4-http-fixture", ToolName: "morde_hub_file_fetch_" + targetID + "_v5",
		TargetInstallationID: targetID, Action: "file.fetch", TaskAuthorizationVersion: claimed.Authorization.Version,
		TargetGrantVersion: 5, TaskRevision: revision, LeaseVersion: claimed.Lease.Version,
		RequestHash: hashBytes("k4-http-fixture-file-fetch"), Status: task.OperationPrepared,
	}
	prepared, replayed, err := store.PrepareTaskHubOperation(ctx, operation, claimed.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("prepare K4 fixture operation: replayed=%t err=%v", replayed, err)
	}
	fetchID := mustID(t, domain.NewOperationID)
	fetch := app.TaskFileFetch{
		ID: fetchID, OwnerID: owner.ID, TaskID: claimed.ID, OperationKey: prepared.Key,
		SourceInstallationID: targetID, SourceDeviceName: "K4 synthetic PC", TargetGrantVersion: 5,
		TaskAuthorizationVersion: claimed.Authorization.Version, TaskRevision: revision,
		RelativePath: "reports/synthetic.pdf", Status: task.FileFetchQueued,
	}
	fetch, replayed, err = store.CreateTaskFileFetch(ctx, app.TaskFileFetchCreate{Operation: prepared, Fetch: fetch}, claimed.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("create K4 fixture fetch: replayed=%t err=%v", replayed, err)
	}
	ticketBytes := make([]byte, 32)
	if _, err := rand.Read(ticketBytes); err != nil {
		t.Fatalf("generate K4 fixture ticket: %v", err)
	}
	ticket := base64.RawURLEncoding.EncodeToString(ticketBytes)
	ticketHash := sha256.Sum256([]byte(ticket))
	ticketExpiry := time.Now().UTC().Add(app.TaskFileFetchTicketLifetime)
	dispatched, err := store.DispatchTaskFileFetch(ctx, prepared, fetch.ID, ticketHash[:], &ticketExpiry, true, claimed.Lease.Owner)
	if err != nil || !dispatched {
		t.Fatalf("dispatch K4 fixture fetch: dispatched=%t err=%v", dispatched, err)
	}

	tasks, err := app.NewTaskService(store, k4FixtureAuthorization{}, k4FixtureModelCatalog{})
	if err != nil {
		t.Fatalf("create K4 fixture Task service: %v", err)
	}
	router, err := api.New(api.Deps{
		Hosts: k4FixtureHostService{}, TLSAuthorizer: k4FixtureHostService{},
		Verifier: k4FixtureVerifier{ownerID: owner.ID}, Auth: k4FixtureAuth{},
		Tasks: tasks, TaskFileFetch: tasks,
	}, api.Options{
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), APIMaxBodyBytes: 1 << 20, MCPMaxBodyBytes: 1 << 20,
		SessionTTL: time.Hour, HostScheme: "http", PortalDomain: "morde.test", HostSuffix: "hosts.invalid", PreviewPortMin: 1024,
	})
	if err != nil {
		t.Fatalf("create K4 fixture API router: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for K4 fixture: %v", err)
	}
	shutdown := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/__test/k4-fixture/shutdown" && isK4LoopbackRemote(r.RemoteAddr) {
			w.WriteHeader(http.StatusNoContent)
			select {
			case <-shutdown:
			default:
				close(shutdown)
			}
			return
		}
		router.ServeHTTP(w, r)
	})}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = server.Shutdown(closeCtx)
		_ = listener.Close()
	})

	address := listener.Addr().(*net.TCPAddr)
	fixture := struct {
		BaseURL            string `json:"base_url"`
		TaskID             string `json:"task_id"`
		FetchID            string `json:"fetch_id"`
		Ticket             string `json:"ticket"`
		TaskRevision       int64  `json:"task_revision"`
		TargetGrantVersion int64  `json:"target_grant_version"`
		ChunkBytes         int64  `json:"chunk_bytes"`
		MaxFileBytes       int64  `json:"max_file_bytes"`
	}{fmt.Sprintf("http://127.0.0.1:%d", address.Port), claimed.ID, fetch.ID, ticket, revision, 5,
		app.TaskFileFetchChunkBytes, app.TaskFileFetchMaxFileBytes}
	encoded, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("encode K4 fixture contract: %v", err)
	}
	fmt.Printf("K4_FIXTURE_READY %s\n", encoded)

	select {
	case <-shutdown:
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("K4 fixture server stopped unexpectedly: %v", err)
		}
	case <-time.After(15 * time.Minute):
		t.Fatal("K4 fixture timed out waiting for local client shutdown")
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	if err := server.Shutdown(closeCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("shut down K4 fixture server: %v", err)
	}
}

func isK4LoopbackRemote(value string) bool {
	hostValue, _, err := net.SplitHostPort(value)
	if err != nil {
		return false
	}
	ip := net.ParseIP(hostValue)
	return ip != nil && ip.IsLoopback()
}

type k4FixtureAuthorization struct{}

func (k4FixtureAuthorization) AuthorizeTaskAction(context.Context, string, identity.Kind, string, string, []identity.Scope, identity.Scope) error {
	return nil
}

func (k4FixtureAuthorization) AuthorizeTaskExecution(_ context.Context, principal identity.Principal, _ string, _ identity.Scope) (task.Authorization, error) {
	expiresAt := time.Now().UTC().Add(time.Hour)
	return task.Authorization{Kind: string(principal.Kind), TokenID: principal.TokenID, Version: 1, ExpiresAt: &expiresAt,
		Scopes: []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit), string(identity.ScopeTaskControl)}}, nil
}

type k4FixtureModelCatalog struct{}

func (k4FixtureModelCatalog) IsConfiguredModel(context.Context, string, string) (bool, error) {
	return true, nil
}

type k4FixtureHostService struct{}

func (k4FixtureHostService) Create(context.Context, identity.Principal, app.CreateHostCommand) (host.Host, error) {
	return host.Host{}, domain.NewError(domain.CodeUnsupported, "fixture Host service is unavailable")
}
func (k4FixtureHostService) EnsureDefaultStopped(context.Context, identity.User) (host.Host, bool, error) {
	return host.Host{}, false, domain.NewError(domain.CodeUnsupported, "fixture Host service is unavailable")
}
func (k4FixtureHostService) List(context.Context, identity.Principal) ([]host.Host, error) {
	return nil, domain.NewError(domain.CodeUnsupported, "fixture Host service is unavailable")
}
func (k4FixtureHostService) Get(context.Context, identity.Principal, string) (host.Host, error) {
	return host.Host{}, domain.NewError(domain.CodeUnsupported, "fixture Host service is unavailable")
}
func (k4FixtureHostService) BindWorkspace(context.Context, identity.Principal, string, string, *int64, int64, string) (workspace.Binding, error) {
	return workspace.Binding{}, domain.NewError(domain.CodeUnsupported, "fixture Host service is unavailable")
}
func (k4FixtureHostService) UnbindWorkspace(context.Context, identity.Principal, string, string, int64, bool, string) (workspace.Binding, error) {
	return workspace.Binding{}, domain.NewError(domain.CodeUnsupported, "fixture Host service is unavailable")
}
func (k4FixtureHostService) RetryWorkspaceSync(context.Context, identity.Principal, string, string, int64) (workspace.Binding, error) {
	return workspace.Binding{}, domain.NewError(domain.CodeUnsupported, "fixture Host service is unavailable")
}
func (k4FixtureHostService) Delete(context.Context, identity.Principal, app.DeleteHostCommand) (host.Host, error) {
	return host.Host{}, domain.NewError(domain.CodeUnsupported, "fixture Host service is unavailable")
}
func (k4FixtureHostService) Release(context.Context, identity.Principal, string, bool) (host.Host, error) {
	return host.Host{}, domain.NewError(domain.CodeUnsupported, "fixture Host service is unavailable")
}
func (k4FixtureHostService) Restore(context.Context, identity.Principal, string) (host.Host, error) {
	return host.Host{}, domain.NewError(domain.CodeUnsupported, "fixture Host service is unavailable")
}
func (k4FixtureHostService) Renew(context.Context, identity.Principal, string) (host.Host, error) {
	return host.Host{}, domain.NewError(domain.CodeUnsupported, "fixture Host service is unavailable")
}
func (k4FixtureHostService) IssueTicket(context.Context, identity.Principal, string) (app.EnterInfo, error) {
	return app.EnterInfo{}, domain.NewError(domain.CodeUnsupported, "fixture Host service is unavailable")
}
func (k4FixtureHostService) IssuePreviewTicket(context.Context, identity.Principal, string, int) (app.EnterInfo, error) {
	return app.EnterInfo{}, domain.NewError(domain.CodeUnsupported, "fixture Host service is unavailable")
}
func (k4FixtureHostService) AuthorizeTLS(context.Context, string) bool { return false }

type k4FixtureVerifier struct{ ownerID string }

func (v k4FixtureVerifier) VerifySession(context.Context, string) (identity.Principal, error) {
	return identity.Principal{UserID: v.ownerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: testSessionTokenID(v.ownerID)}, nil
}
func (v k4FixtureVerifier) VerifyAPIKey(context.Context, string) (identity.Principal, error) {
	return identity.Principal{}, domain.NewError(domain.CodeUnauthenticated, "fixture credential is unavailable")
}
func (v k4FixtureVerifier) ConsumeTicket(context.Context, string) (app.Ticket, error) {
	return app.Ticket{}, domain.NewError(domain.CodeUnauthenticated, "fixture credential is unavailable")
}
func (v k4FixtureVerifier) VerifyClient(context.Context, string) (identity.Principal, error) {
	return identity.Principal{}, domain.NewError(domain.CodeUnauthenticated, "fixture credential is unavailable")
}
func (v k4FixtureVerifier) VerifyDevice(context.Context, string) (identity.Principal, error) {
	return identity.Principal{}, domain.NewError(domain.CodeUnauthenticated, "fixture credential is unavailable")
}

type k4FixtureAuth struct{}

func (k4FixtureAuth) Register(context.Context, string, string, string, ...string) (api.UserView, error) {
	return api.UserView{}, domain.NewError(domain.CodeUnsupported, "fixture Auth service is unavailable")
}
func (k4FixtureAuth) Login(context.Context, string, string, string) (api.LoginResult, error) {
	return api.LoginResult{}, domain.NewError(domain.CodeUnsupported, "fixture Auth service is unavailable")
}
func (k4FixtureAuth) Logout(context.Context, identity.Principal) error {
	return domain.NewError(domain.CodeUnsupported, "fixture Auth service is unavailable")
}
func (k4FixtureAuth) ChangePassword(context.Context, identity.Principal, string, string) error {
	return domain.NewError(domain.CodeUnsupported, "fixture Auth service is unavailable")
}
func (k4FixtureAuth) CurrentUser(context.Context, identity.Principal) (api.UserView, error) {
	return api.UserView{}, domain.NewError(domain.CodeUnsupported, "fixture Auth service is unavailable")
}
