package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/template"
	"morde/server/internal/storage"
	"morde/server/internal/storage/workspacefs"
)

// Opt-in native smoke test: a loopback Portal backed by a disposable PostgreSQL database.
func TestNativeDesktopAcceptance(t *testing.T) {
	if os.Getenv("MORDE_NATIVE_ACCEPTANCE") != "1" {
		t.Skip("set MORDE_NATIVE_ACCEPTANCE=1 to run the interactive desktop acceptance")
	}
	store, credentials, clients, owner := clientLoginFixture(t)
	ctx := context.Background()
	admin, err := store.GetUserByID(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	admin.Role = identity.RoleAdmin
	owner, err = store.UpdateUser(ctx, "system", admin.Status, admin)
	if err != nil {
		t.Fatal(err)
	}
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	users, err := app.NewUserService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "blocks"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocks.Close() })
	capacity, err := storage.NewWorkspaceCapacity(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	workspaces, err := app.NewWorkspaceService(store, blocks, capacity, audit, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	syncService, err := app.NewWorkspaceSyncService(store, blocks, 0, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := app.NewTokenService(store, audit, 24*time.Hour, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	notifications, err := app.NewNotificationService(store)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	model := nativeAcceptanceModel{}
	models, err := app.NewModelConnectionService(store, sealer, model, []string{"example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	login, err := credentials.Login(ctx, owner.Name, "client-password-123", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifySession(ctx, login.Credential)
	if err != nil {
		t.Fatal(err)
	}
	checked, err := models.Create(ctx, principal, app.ModelConnectionCommand{
		Name: "本地验收模型", BaseURL: "https://example.invalid/v1", ModelName: "morde-local-qa", APIKey: "synthetic-test-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_account_budgets(owner_id,token_limit,max_concurrent) VALUES ($1,50000,2)`, owner.ID); err != nil {
		t.Fatal(err)
	}
	modelGateway, err := app.NewModelGatewayService(store, models, model)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := modelGateway.CheckModelCapability(ctx, principal, checked.ID, "native-model-probe-0001", "all"); err != nil {
		t.Fatal(err)
	}
	chats, err := app.NewInstantChatService(store, modelGateway)
	if err != nil {
		t.Fatal(err)
	}
	hosts := nativeAcceptanceHosts{}
	onboarding, err := app.NewOnboardingService(store, hosts, models, "small")
	if err != nil {
		t.Fatal(err)
	}
	portalRoot := t.TempDir()
	if err := os.CopyFS(portalRoot, os.DirFS("../../../../web/portal/dist")); err != nil {
		t.Fatalf("build the current Portal first: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	downloadName := "morde-desktop-0.0.0-localtest-linux-x64.AppImage"
	downloadBytes := []byte("Morde local download acceptance fixture\n")
	downloadDir := filepath.Join(portalRoot, "downloads", "files")
	if err := os.MkdirAll(downloadDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(downloadDir, downloadName), downloadBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(downloadBytes)
	manifest, err := json.Marshal(map[string]any{
		"schema_version": 3, "version": "0.0.0-localtest", "portal_url": "http://" + listener.Addr().String() + "/",
		"artifacts": []any{map[string]any{
			"platform": "linux", "architecture": "x64", "filename": downloadName,
			"download_url": "/downloads/files/" + downloadName, "bytes": len(downloadBytes), "sha256": hex.EncodeToString(digest[:]),
			"acceptance": map[string]any{
				"status": "verified", "verified_at": "2026-10-27T00:00:00.000Z", "evidence_ref": "local-native-download-fixture",
				"signature_status": "not_applicable", "capabilities": []string{"install", "launch_idle_network", "user_web_workbench", "workspace_sync_recovery", "update", "uninstall"},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(portalRoot, "downloads", "manifest.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	portalFiles := os.DirFS(portalRoot)
	router, err := api.New(api.Deps{
		Hosts: hosts, HostSharing: nativeAcceptanceSharing{}, TLSAuthorizer: nativeAcceptanceTLS{}, Verifier: credentials,
		Auth: nativeAcceptanceAuth{credentials: credentials, users: users}, ClientLogin: clients,
		WorkspaceOps: workspaces, WorkspaceSync: syncService, Tokens: nativeAcceptanceTokens{tokens},
		Notifications: notifications, ModelConnections: models, ModelGateway: modelGateway, InstantChat: chats, Onboarding: onboarding,
		Templates: nativeAcceptanceTemplates{}, MCPDisabled: true,
	}, api.Options{
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), APIMaxBodyBytes: 16 << 20,
		MCPMaxBodyBytes: 16 << 20, SessionTTL: 12 * time.Hour, HostScheme: "http",
		PortalDomain: listener.Addr().String(), HostSuffix: "hosts.invalid", PreviewPortMin: 1024, PortalFiles: portalFiles,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: router, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Shutdown(context.Background())
		if err := <-serverDone; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve test Portal: %v", err)
		}
	})
	stop := filepath.Join(t.TempDir(), "finish")
	downloadTarget := filepath.Join(t.TempDir(), downloadName)
	t.Logf("LOCAL_PORTAL=%s TEST_USER=%s DOWNLOAD_FILE=%s STOP_FILE=%s", "http://"+listener.Addr().String(), owner.Name, downloadTarget, stop)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	timeout := time.NewTimer(15 * time.Minute)
	defer timeout.Stop()
	for {
		select {
		case <-ticker.C:
			if _, err := os.Stat(stop); err == nil {
				goto verify
			} else if !os.IsNotExist(err) {
				t.Fatal(err)
			}
		case <-timeout.C:
			t.Fatal("interactive native acceptance timed out")
		}
	}

verify:
	file, err := os.ReadFile(downloadTarget)
	if err != nil || string(file) != string(downloadBytes) {
		t.Errorf("native download = %q, %v; want synthetic fixture", file, err)
	}
	for _, check := range []struct {
		name  string
		query string
		min   int
		max   int
	}{
		{"instant chat replies", `SELECT count(*) FROM conversation_messages WHERE owner_id=$1 AND role='assistant' AND task_id IS NULL`, 1, 1 << 30},
		{"user workspaces", `SELECT count(*) FROM workspaces WHERE owner_id=$1`, 1, 1 << 30},
		{"retired task records", `SELECT count(*) FROM tasks WHERE owner_id=$1`, 0, 0},
	} {
		var count int
		if err := store.pool.QueryRow(ctx, check.query, owner.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count < check.min || count > check.max {
			t.Errorf("%s = %d, want %d..%d", check.name, count, check.min, check.max)
		}
	}
}

type nativeAcceptanceModel struct{}

func (nativeAcceptanceModel) Complete(_ context.Context, request app.ModelRequest) (app.ModelResponse, error) {
	if request.RequiredTool != "" {
		return app.ModelResponse{Text: "RED", ToolCalls: []app.ModelToolCall{{CallID: "probe-call-1", Name: request.RequiredTool,
			Arguments: `{"color":"red"}`}}, Usage: app.ModelUsage{Known: true, InputTokens: 24, OutputTokens: 4}}, nil
	}
	return app.ModelResponse{Text: "本地验收回复", Usage: app.ModelUsage{Known: true, InputTokens: 12, OutputTokens: 5}}, nil
}

func (nativeAcceptanceModel) Check(context.Context, app.ModelEndpoint) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{Text: true}, nil
}

type nativeAcceptanceHosts struct{ app.HostService }

func (nativeAcceptanceHosts) List(context.Context, identity.Principal) ([]host.Host, error) {
	return []host.Host{}, nil
}

type nativeAcceptanceSharing struct{ app.HostSharingService }

func (nativeAcceptanceSharing) ListSharedHosts(context.Context, identity.Principal) ([]host.SharedHost, error) {
	return []host.SharedHost{}, nil
}

type nativeAcceptanceTLS struct{}

func (nativeAcceptanceTLS) AuthorizeTLS(context.Context, string) bool { return false }

type nativeAcceptanceTemplates struct{}

func (nativeAcceptanceTemplates) ListTemplates(context.Context) ([]api.TemplateRecord, error) {
	return []api.TemplateRecord{{ID: "small", Name: "本地验收规格", Isolation: "container",
		Resources:        api.ResourceRecord{CPUMilli: 1000, MemMiB: 2048, CPULimitMilli: 1000, MemLimitMiB: 2048},
		WorkspaceSizeMiB: 1024, TTLSeconds: 3600, IdleTimeoutSeconds: 1800, OnIdle: template.OnIdleDisabled}}, nil
}

type nativeAcceptanceAuth struct {
	credentials *app.CredentialService
	users       *app.UserService
}

func (a nativeAcceptanceAuth) Register(ctx context.Context, name, password, ip string) (api.UserView, error) {
	user, err := a.users.Register(ctx, name, password, ip)
	return nativeAcceptanceUserView(user), err
}

func (a nativeAcceptanceAuth) Login(ctx context.Context, name, password, ip string) (api.LoginResult, error) {
	login, err := a.credentials.Login(ctx, name, password, ip)
	return api.LoginResult{Credential: login.Credential, ExpiresAt: login.ExpiresAt, User: nativeAcceptanceUserView(login.User)}, err
}

func (a nativeAcceptanceAuth) Logout(ctx context.Context, principal identity.Principal) error {
	return a.credentials.Logout(ctx, principal)
}

func (a nativeAcceptanceAuth) ChangePassword(ctx context.Context, principal identity.Principal, current, next string) error {
	return a.credentials.ChangePassword(ctx, principal, current, next)
}

func (a nativeAcceptanceAuth) CurrentUser(ctx context.Context, principal identity.Principal) (api.UserView, error) {
	user, err := a.credentials.CurrentUser(ctx, principal)
	return nativeAcceptanceUserView(user), err
}

func nativeAcceptanceUserView(user app.UserSummary) api.UserView {
	return api.UserView{ID: user.ID, Name: user.Name, Role: user.Role, Status: user.Status,
		MustChangePassword: user.MustChangePassword,
		Quota:              api.TotalsView{Hosts: user.Quota.Hosts, CPUMilli: user.Quota.CPUMilli, MemMiB: user.Quota.MemMiB},
		Usage:              api.TotalsView{Hosts: user.Usage.Hosts, CPUMilli: user.Usage.CPUMilli, MemMiB: user.Usage.MemMiB}}
}

type nativeAcceptanceTokens struct{ service *app.TokenService }

func (a nativeAcceptanceTokens) ListAPIKeys(ctx context.Context, principal identity.Principal) ([]identity.Token, error) {
	return a.service.ListAPIKeys(ctx, principal)
}

func (a nativeAcceptanceTokens) CreateAPIKey(ctx context.Context, principal identity.Principal, name string, ttl time.Duration) (api.CreatedAPIKey, error) {
	key, err := a.service.CreateAPIKey(ctx, principal, name, ttl)
	return api.CreatedAPIKey{ID: key.ID, Key: key.Key, ExpiresAt: key.ExpiresAt}, err
}

func (a nativeAcceptanceTokens) DeleteAPIKey(ctx context.Context, principal identity.Principal, id string) error {
	return a.service.DeleteAPIKey(ctx, principal, id)
}
