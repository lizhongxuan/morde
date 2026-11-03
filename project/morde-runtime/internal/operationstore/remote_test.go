package operationstore

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const remoteTestHostID = "h1234567"

type remoteFixture struct {
	mu                  sync.Mutex
	events              map[string][]Event
	status              int
	commitThenErrorOnce bool
}

func (f *remoteFixture) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status != 0 {
		http.Error(writer, "unavailable", f.status)
		return
	}
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if len(parts) != 7 || parts[0] != "api" || parts[1] != "host-replicas" || parts[2] != remoteTestHostID ||
		parts[3] != "generations" || parts[4] != "4" || parts[5] != "operation-ledger" {
		http.NotFound(writer, request)
		return
	}
	namespace := parts[6]
	if request.Header.Get("Authorization") != "Bearer 1234567890123456789012345678901234567890123" {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	key := namespace
	events := f.events[key]
	switch request.Method {
	case http.MethodGet:
		after, err := strconv.ParseUint(request.URL.Query().Get("after_sequence"), 10, 64)
		if err != nil {
			http.Error(writer, "bad cursor", http.StatusBadRequest)
			return
		}
		limit, err := strconv.Atoi(request.URL.Query().Get("limit"))
		if err != nil || limit < 1 {
			http.Error(writer, "bad limit", http.StatusBadRequest)
			return
		}
		start := int(after)
		if start > len(events) {
			start = len(events)
		}
		end := start + limit
		if end > len(events) {
			end = len(events)
		}
		_ = json.NewEncoder(writer).Encode(pageResponse{Items: cloneEvents(events[start:end]), HasMore: end < len(events)})
	case http.MethodPost:
		var event Event
		decoder := json.NewDecoder(request.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&event); err != nil {
			http.Error(writer, "bad event", http.StatusBadRequest)
			return
		}
		if int(event.Sequence) <= len(events) {
			if !sameEvent(events[event.Sequence-1], event) {
				http.Error(writer, "conflict", http.StatusConflict)
				return
			}
		} else if err := validateNextEvent(events, totalEventBytes(events), event); err != nil {
			http.Error(writer, "invalid chain", http.StatusConflict)
			return
		} else {
			f.events[key] = append(events, cloneEvent(event))
		}
		if f.commitThenErrorOnce {
			f.commitThenErrorOnce = false
			http.Error(writer, "lost acknowledgement", http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(writer).Encode(event)
	default:
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func TestRemoteConfirmsEventAfterLostCommitAcknowledgement(t *testing.T) {
	fixture := &remoteFixture{events: make(map[string][]Event), commitThenErrorOnce: true}
	server := httptest.NewTLSServer(fixture)
	defer server.Close()
	legacy := openTestStore(t)
	remote := openTestRemote(t, server, RemoteNamespaceCore)
	if err := remote.Reconcile(legacy); err != nil {
		t.Fatal(err)
	}
	if err := remote.Append("runtime_state", map[string]any{"generation": 4}); err != nil {
		t.Fatalf("append with lost acknowledgement: %v", err)
	}
	events, err := remote.Events()
	if err != nil || len(events) != 1 {
		t.Fatalf("confirmed remote events = %#v, err=%v", events, err)
	}
}

func TestRemoteReconcilesLegacyJournalAndRequiresDurableAcknowledgement(t *testing.T) {
	fixture := &remoteFixture{events: make(map[string][]Event)}
	server := httptest.NewTLSServer(fixture)
	defer server.Close()
	legacy := openTestStore(t)
	if err := legacy.Append("operation", map[string]any{"state": "accepted"}); err != nil {
		t.Fatal(err)
	}
	localBefore, err := legacy.Events()
	if err != nil {
		t.Fatal(err)
	}
	remote := openTestRemote(t, server, RemoteNamespaceCore)
	if err := remote.Reconcile(legacy); err != nil {
		t.Fatal(err)
	}
	if err := remote.Append("operation", map[string]any{"state": "succeeded"}); err != nil {
		t.Fatal(err)
	}
	remoteEvents, err := remote.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(remoteEvents) != 2 || remoteEvents[0].Hash != localBefore[0].Hash || remoteEvents[1].Sequence != 2 {
		t.Fatalf("remote events = %#v", remoteEvents)
	}
	localAfter, err := legacy.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(localAfter) != 1 || localAfter[0].Hash != localBefore[0].Hash {
		t.Fatalf("legacy rollback evidence changed: %#v", localAfter)
	}
	if !remote.Ready() {
		t.Fatal("remote ledger is not ready after acknowledged writes")
	}
}

func TestRemoteRejectsForkAndStaysUnhealthy(t *testing.T) {
	fixture := &remoteFixture{events: make(map[string][]Event)}
	server := httptest.NewTLSServer(fixture)
	defer server.Close()
	legacy := openTestStore(t)
	if err := legacy.Append("runtime_state", map[string]any{"control_version": 1}); err != nil {
		t.Fatal(err)
	}
	fixture.events[RemoteNamespaceCore] = []Event{mustMakeEvent(t, 1, "runtime_state", `{"control_version":2}`, "")}
	remote := openTestRemote(t, server, RemoteNamespaceCore)
	if err := remote.Reconcile(legacy); err == nil {
		t.Fatal("diverged local and remote ledgers were reconciled")
	}
	if remote.Ready() {
		t.Fatal("diverged ledger remained ready")
	}
	if err := remote.Append("operation", map[string]any{"state": "accepted"}); !errors.Is(err, ErrStoreUnhealthy) {
		t.Fatalf("append after fork error = %v", err)
	}
}

func TestRemoteFailsClosedWhenPortalUnavailable(t *testing.T) {
	fixture := &remoteFixture{events: make(map[string][]Event), status: http.StatusServiceUnavailable}
	server := httptest.NewTLSServer(fixture)
	defer server.Close()
	if _, err := OpenRemote(RemoteOptions{
		HostID: remoteTestHostID, Generation: 4, Namespace: RemoteNamespaceCore,
		PortalURL: server.URL, Credential: remoteTestCredential, Client: server.Client(),
	}); err == nil {
		t.Fatal("remote ledger opened while Portal was unavailable")
	}
}

func TestRemoteLoadsHashChainAcrossPages(t *testing.T) {
	fixture := &remoteFixture{events: make(map[string][]Event)}
	var previous string
	for sequence := uint64(1); sequence <= 129; sequence++ {
		event, err := makeEvent(sequence, "runtime_state", json.RawMessage(`{"generation":4}`), previous)
		if err != nil {
			t.Fatal(err)
		}
		fixture.events[RemoteNamespaceCore] = append(fixture.events[RemoteNamespaceCore], event)
		previous = event.Hash
	}
	server := httptest.NewTLSServer(fixture)
	defer server.Close()
	remote := openTestRemote(t, server, RemoteNamespaceCore)
	events, err := remote.Events()
	if err != nil || len(events) != 129 || events[0].Sequence != 1 || events[128].Sequence != 129 {
		t.Fatalf("paged remote events count=%d err=%v", len(events), err)
	}
}

func TestRemoteRequiresReconciliationBeforeFirstAppend(t *testing.T) {
	fixture := &remoteFixture{events: make(map[string][]Event)}
	server := httptest.NewTLSServer(fixture)
	defer server.Close()
	remote := openTestRemote(t, server, RemoteNamespaceBrowser)
	if err := remote.Append("browser.operation", map[string]any{"state": "started"}); err == nil {
		t.Fatal("remote ledger accepted a write before legacy reconciliation")
	}
}

const remoteTestCredential = "1234567890123456789012345678901234567890123"

func openTestRemote(t *testing.T, server *httptest.Server, namespace string) *Remote {
	t.Helper()
	remote, err := OpenRemote(RemoteOptions{
		HostID: remoteTestHostID, Generation: 4, Namespace: namespace,
		PortalURL: server.URL, Credential: remoteTestCredential, Client: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = remote.Close() })
	return remote
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func mustMakeEvent(t *testing.T, sequence uint64, kind string, data, previousHash string) Event {
	t.Helper()
	event, err := makeEvent(sequence, kind, json.RawMessage(data), previousHash)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func totalEventBytes(events []Event) int64 {
	var total int64
	for _, event := range events {
		total += int64(len(event.Data))
	}
	return total
}
