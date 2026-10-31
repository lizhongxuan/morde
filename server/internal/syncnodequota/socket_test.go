package syncnodequota

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestEndpointServesOwnerBoundHTTPOverUnixSocket(t *testing.T) {
	group := os.Getegid()
	dir := newSocketTestDir(t, group)
	socket := filepath.Join(dir, "quota.sock")
	if len(socket) > maxUnixSocketPath {
		t.Skip("temporary path exceeds Unix socket path limit")
	}
	service := &socketTestService{calls: make(chan socketTestCall, 1)}
	endpoint, err := Listen(socket, "u1234567", service, group)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- endpoint.Serve(ctx) }()

	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socket)
		},
	}
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	request, err := http.NewRequest(http.MethodPost, "http://quota.local/v1/reservations", strings.NewReader(`{"workspace_id":"w1234567","folder_id":"morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","operation_id":"op-1","path":"file.txt","operation":"upsert","size_bytes":3}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		cancel()
		_ = endpoint.Close()
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	select {
	case call := <-service.calls:
		if response.StatusCode != http.StatusOK || call.owner != "u1234567" || call.request.Path != "file.txt" {
			t.Fatalf("HTTP over socket response=%d owner=%q path=%q", response.StatusCode, call.owner, call.request.Path)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("HTTP over socket response=%d without a service call", response.StatusCode)
	}
	info, err := os.Lstat(socket)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Gid) != group || info.Mode().Perm() != 0o660 || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket has unsafe mode or group: mode=%v stat=%#v", info.Mode(), info.Sys())
	}
	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		_ = endpoint.Close()
		t.Fatal("endpoint did not stop after context cancellation")
	}
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Fatalf("socket remains after close: %v", err)
	}
	transport.CloseIdleConnections()
}

type socketTestCall struct {
	owner   string
	request ReserveRequest
}

type socketTestService struct {
	calls chan socketTestCall
}

func (s *socketTestService) Reserve(_ context.Context, owner string, request ReserveRequest) (Receipt, error) {
	s.calls <- socketTestCall{owner: owner, request: request}
	return Receipt{ReservationID: "r1234567", State: "reserved", BaseRevision: 0}, nil
}

func (*socketTestService) Settle(context.Context, string, string) (Receipt, error) {
	return Receipt{}, nil
}

func (*socketTestService) Abort(_ context.Context, _ string, reservationID string) (Receipt, error) {
	return Receipt{ReservationID: reservationID, State: "aborted", BaseRevision: 0}, nil
}

func TestListenRejectsUnsafeSocketParentAndSymlink(t *testing.T) {
	group := os.Getegid()
	unsafe := t.TempDir()
	if err := os.Chmod(unsafe, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(filepath.Join(unsafe, "quota.sock"), "u1234567", &fakeService{}, group); err == nil {
		t.Fatal("group-writable socket parent was accepted")
	}

	realDir := newSocketTestDir(t, group)
	symlinkDir := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realDir, symlinkDir); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(filepath.Join(symlinkDir, "quota.sock"), "u1234567", &fakeService{}, group); err == nil {
		t.Fatal("symlink socket parent was accepted")
	}
}

func TestEndpointCloseDoesNotRemoveReplacementPath(t *testing.T) {
	group := os.Getegid()
	dir := newSocketTestDir(t, group)
	socket := filepath.Join(dir, "quota.sock")
	if len(socket) > maxUnixSocketPath {
		t.Skip("temporary path exceeds Unix socket path limit")
	}
	endpoint, err := Listen(socket, "u1234567", &fakeService{}, group)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socket, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(socket)
	if err != nil || string(contents) != "replacement" {
		t.Fatalf("replacement path changed after endpoint close: contents=%q err=%v", contents, err)
	}
}

func TestListenRemovesOnlyStaleOwnerSocket(t *testing.T) {
	group := os.Getegid()
	dir := newSocketTestDir(t, group)
	socket := filepath.Join(dir, "quota.sock")
	if len(socket) > maxUnixSocketPath {
		t.Skip("temporary path exceeds Unix socket path limit")
	}
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	endpoint, err := Listen(socket, "u1234567", &fakeService{}, group)
	if err != nil {
		t.Fatalf("listen did not replace stale socket: %v", err)
	}
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Fatalf("socket remains after endpoint close: %v", err)
	}
}

func newSocketTestDir(t *testing.T, gid int) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chown(dir, -1, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o2710); err != nil {
		t.Fatal(err)
	}
	return dir
}
