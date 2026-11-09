package postgres

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/model"
	"morde/server/internal/worker"
)

type nodeSSEProvider struct {
	first   chan struct{}
	release chan struct{}
	probes  atomic.Int32
	tasks   atomic.Int32
}

func (p *nodeSSEProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Instructions string `json:"instructions"`
		Stream       bool   `json:"stream"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&request) != nil || !request.Stream || r.Header.Get("Accept") != "text/event-stream" {
		http.Error(w, "expected a synthetic streaming request", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	if request.Instructions == "Reply exactly OK using text streaming." {
		p.probes.Add(1)
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n")
		flusher.Flush()
		return
	}
	if p.tasks.Add(1) != 1 {
		http.Error(w, "duplicate synthetic request", http.StatusConflict)
		return
	}
	_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello \"}\n\n")
	flusher.Flush()
	close(p.first)
	select {
	case <-r.Context().Done():
		return
	case <-p.release:
	}
	_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"world.\"}\n\n")
	_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello world.\"}]}],\"usage\":{\"input_tokens\":8,\"output_tokens\":3}}}\n\n")
	flusher.Flush()
}

type nodeSSEResolver struct{ version time.Time }

func (r nodeSSEResolver) ResolveGatewayEndpoint(_ context.Context, _ string, _ string, needsTools, _ bool) (app.ModelGatewayEndpoint, error) {
	if needsTools {
		return app.ModelGatewayEndpoint{}, domain.NewError(domain.CodeUnsupported, "synthetic model is text only")
	}
	return app.ModelGatewayEndpoint{Endpoint: app.ModelEndpoint{BaseURL: "https://api.openai.com/v1", APIKey: "synthetic-key", ModelID: "test"},
		Version: r.version, Personal: true}, nil
}

func (r nodeSSEResolver) ResolveProbeEndpoint(ctx context.Context, principal identity.Principal, modelID string) (app.ModelGatewayEndpoint, error) {
	return r.ResolveGatewayEndpoint(ctx, principal.UserID, modelID, false, false)
}

func (nodeSSEResolver) SaveProbeCapabilities(_ context.Context, _ identity.Principal, _ string, _ time.Time, value app.ModelCapabilities) (app.ModelCapabilities, error) {
	return value, nil
}

func syntheticNodeSSECertificate(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "api.openai.com"},
		DNSNames: []string{"api.openai.com"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	certPath, keyPath := filepath.Join(directory, "synthetic-ca.pem"), filepath.Join(directory, "synthetic-key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKey}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func syntheticNodeSSEProxy(t *testing.T, provider *nodeSSEProvider, certPath, keyPath string) string {
	t.Helper()
	certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(provider)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "api.openai.com:443" {
			http.Error(w, "unexpected synthetic proxy target", http.StatusBadRequest)
			return
		}
		upstream, err := net.Dial("tcp", server.Listener.Addr().String())
		if err != nil {
			http.Error(w, "synthetic provider unavailable", http.StatusBadGateway)
			return
		}
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		defer func() { _ = client.Close() }()
		defer func() { _ = upstream.Close() }()
		if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if err := buffered.Flush(); err != nil {
			return
		}
		go func() { _, _ = io.Copy(upstream, client) }()
		_, _ = io.Copy(client, upstream)
	}))
	t.Cleanup(proxy.Close)
	return proxy.URL
}

func TestRealDSHTextStreamingPersistsSameMessageAndMeteredUsage(t *testing.T) {
	if os.Getenv("MORDE_SYNTHETIC_SSE_CHILD") != "1" {
		certificate, key := syntheticNodeSSECertificate(t)
		command := exec.Command(os.Args[0], "-test.run=^TestRealDSHTextStreamingPersistsSameMessageAndMeteredUsage$", "-test.v", "-test.timeout=90s")
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "SSL_CERT_FILE=") && !strings.HasPrefix(entry, "SSL_CERT_DIR=") && !strings.HasPrefix(entry, "GODEBUG=") {
				command.Env = append(command.Env, entry)
			}
		}
		command.Env = append(command.Env, "MORDE_SYNTHETIC_SSE_CHILD=1", "MORDE_SYNTHETIC_SSE_CERT="+certificate,
			"MORDE_SYNTHETIC_SSE_KEY="+key, "SSL_CERT_FILE="+certificate, "GODEBUG="+os.Getenv("GODEBUG")+",x509sslcertoverrideplatform=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated synthetic TLS/SSE test failed: %v\n%s", err, output)
		}
		return
	}
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	provider := &nodeSSEProvider{first: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(provider.release) })
	proxyURL := syntheticNodeSSEProxy(t, provider, os.Getenv("MORDE_SYNTHETIC_SSE_CERT"), os.Getenv("MORDE_SYNTHETIC_SSE_KEY"))
	client, err := model.NewResponsesClient([]string{"api.openai.com"}, proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	var version time.Time
	if err := f.store.pool.QueryRow(ctx, `SELECT updated_at FROM model_connections WHERE id='p1234567'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	gateway, err := app.NewModelGatewayService(f.store, nodeSSEResolver{version: version}, client)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: f.ownerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: f.authorization.TokenID}
	proof, err := gateway.CheckModelCapability(ctx, principal, "p1234567", "node-text-stream-proof-001", "text_stream")
	if err != nil || !proof.TextStream || provider.probes.Load() != 1 {
		t.Fatalf("explicit stream proof = %+v probes=%d err=%v", proof, provider.probes.Load(), err)
	}
	claim := f.receive(t, "node-text-stream-message-001", "Say hello", app.ConversationRoutingInput{})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, claim.Message.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET budget_model_tokens=50000 WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "node-stream-worker", time.Minute)
	if err != nil || !found || claimed.ID != taskID {
		t.Fatalf("claim streaming Task: found=%t err=%v", found, err)
	}
	completion, err := app.NewCloudDSHToolBroker(f.store, &app.RuntimeToolExecutor{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	executor := revisionTestExecutorWithOptions(t, f.store, gateway, nil, func(options *worker.CloudDSHExecutorOptions) {
		options.TextStreams = f.store
		options.Completion = completion
	})
	service, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan error, 1)
	go func() { done <- executor.ExecuteTask(app.WithModelTaskLifecycle(service, service), claimed) }()
	select {
	case <-provider.first:
	case err := <-done:
		t.Fatalf("DSH stopped before its first stream delta: %v", err)
	case <-time.After(25 * time.Second):
		t.Fatal("DSH did not dispatch the synthetic streaming request")
	}
	var messageID, visible, status, ledgerState string
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = f.store.pool.QueryRow(ctx, `SELECT m.id,m.content,m.output_status,l.state
FROM conversation_messages m JOIN model_text_streaming_dispatches d ON d.task_id=m.task_id
JOIN model_call_ledger l ON l.owner_id=d.owner_id AND l.source=d.source AND l.call_id=d.call_id
WHERE m.task_id=$1 AND m.role='assistant'`, taskID).Scan(&messageID, &visible, &status, &ledgerState)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil || visible != "Hello " || status != "streaming" || ledgerState != "dispatched" {
		t.Fatalf("first visible delta = %q status=%s ledger=%s err=%v", visible, status, ledgerState, err)
	}
	release.Do(func() { close(provider.release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("complete real DSH streaming Task: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("DSH streaming Task did not finish")
	}
	var finalID, finalContent, finalStatus string
	var input, output int64
	var evidenceMatches bool
	err = f.store.pool.QueryRow(ctx, `SELECT m.id,m.content,m.output_status,l.state,l.input_tokens,l.output_tokens,
 e.task_revision=m.output_task_revision AND e.result_content_sha256=sha256(convert_to(m.content,'UTF8'))
FROM conversation_messages m JOIN task_events e ON e.task_id=m.task_id AND e.sequence=m.output_result_event_sequence
JOIN model_text_streaming_dispatches d ON d.task_id=m.task_id
JOIN model_call_ledger l ON l.owner_id=d.owner_id AND l.source=d.source AND l.call_id=d.call_id
WHERE m.task_id=$1 AND m.role='assistant'`, taskID).
		Scan(&finalID, &finalContent, &finalStatus, &ledgerState, &input, &output, &evidenceMatches)
	if err != nil || finalID != messageID || finalContent != "Hello world." || finalStatus != "completed" ||
		ledgerState != "settled" || input != 8 || output != 3 || !evidenceMatches || provider.tasks.Load() != 1 {
		t.Fatalf("final stream message id_match=%t content=%q status=%s ledger=%s usage=%d/%d evidence=%t calls=%d err=%v",
			finalID == messageID, finalContent, finalStatus, ledgerState, input, output, evidenceMatches, provider.tasks.Load(), err)
	}
}
