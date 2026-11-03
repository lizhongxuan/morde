package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
)

func TestHostControlWebSocketInputRequiresCurrentLeasePermit(t *testing.T) {
	upstream := &rfbCaptureBody{}
	permits := &testActivityLeaseStore{inputErr: domain.NewError(domain.CodeConflict, "control lease expired")}
	body := &hostControlInputBody{
		ReadWriteCloser: upstream, ctx: context.Background(), permits: permits,
		ref:     host.Ref{ID: testHostID, RuntimeGeneration: 3, ControlVersion: 8, ControlState: host.ControlUser},
		actorID: testUserID, permission: host.PermissionDesktopControl,
	}
	if n, err := body.Write([]byte("stale keyboard frame")); domain.CodeOf(err) != domain.CodeConflict || n != 0 {
		t.Fatalf("stale input write = %d, %v; want conflict", n, err)
	}
	if upstream.written.Len() != 0 {
		t.Fatalf("stale input reached upstream: %q", upstream.written.String())
	}

	permits.inputErr = nil
	message := []byte("current controller frame")
	if n, err := body.Write(message); err != nil || n != len(message) {
		t.Fatalf("current input write = %d, %v; want %d bytes", n, err, len(message))
	}
	if !bytes.Equal(upstream.written.Bytes(), message) {
		t.Fatalf("current input = %q, want %q", upstream.written.Bytes(), message)
	}
}

type captureRoundTripper struct{ called bool }

func (r *captureRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	r.called = true
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("response"))}, nil
}

func TestHostControlProxyRequestRequiresCurrentLeasePermit(t *testing.T) {
	base := &captureRoundTripper{}
	permits := &testActivityLeaseStore{inputErr: domain.NewError(domain.CodeConflict, "control lease expired")}
	transport := &hostControlRoundTripper{
		base: base, permits: permits,
		ref:     host.Ref{ID: testHostID, RuntimeGeneration: 3, ControlVersion: 8, ControlState: host.ControlUser},
		actorID: testUserID, permission: host.PermissionTerminalControl,
	}
	request, err := http.NewRequest(http.MethodPost, "http://upstream.example/terminal/action", strings.NewReader("input"))
	if err != nil {
		t.Fatal(err)
	}
	staleResponse, err := transport.RoundTrip(request)
	if staleResponse != nil && staleResponse.Body != nil {
		_ = staleResponse.Body.Close()
	}
	if domain.CodeOf(err) != domain.CodeConflict || base.called {
		t.Fatalf("stale proxy request error=%v upstream_called=%t; want conflict before forwarding", err, base.called)
	}

	permits.inputErr = nil
	//nolint:bodyclose // The response body is read and closed immediately below.
	response, err := transport.RoundTrip(request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("current proxy request error=%v upstream_called=%t; want forwarding", err, base.called)
	}
	if !base.called || response == nil || response.Body == nil {
		t.Fatalf("current proxy response=%#v upstream_called=%t; want a response body", response, base.called)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close current proxy response: %v", err)
		}
	}()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "response" {
		t.Fatalf("current proxy response body = %q, %v; want response to remain readable", body, err)
	}
}

type blockingRoundTripper struct{}

func (blockingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	<-request.Context().Done()
	return nil, request.Context().Err()
}

type readWriteRoundTripper struct{ body io.ReadWriteCloser }

func (r readWriteRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusSwitchingProtocols, Body: r.body}, nil
}

func TestHostControlProxyUpgradeKeepsReadWriteBody(t *testing.T) {
	upstream := &rfbCaptureBody{}
	transport := &hostControlRoundTripper{
		base: readWriteRoundTripper{body: upstream}, permits: &testActivityLeaseStore{},
		ref:     host.Ref{ID: testHostID, RuntimeGeneration: 3, ControlVersion: 8, ControlState: host.ControlUser},
		actorID: testUserID, permission: host.PermissionDesktopControl,
	}
	request, err := http.NewRequest(http.MethodGet, "http://upstream.example/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	//nolint:bodyclose // The upgraded response body is closed via its read/write interface below.
	response, err := transport.RoundTrip(request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.Fatal(err)
	}
	if response == nil || response.Body == nil {
		t.Fatal("upgraded proxy response has no body")
	}
	upgraded, ok := response.Body.(io.ReadWriteCloser)
	if !ok {
		t.Fatalf("upgraded proxy response body %T does not support bidirectional WebSocket I/O", response.Body)
	}
	defer func() {
		if err := upgraded.Close(); err != nil {
			t.Errorf("close upgraded proxy response: %v", err)
		}
	}()
	if n, err := upgraded.Write([]byte("frame")); err != nil || n != len("frame") || upstream.written.String() != "frame" {
		t.Fatalf("upgraded proxy body write = %d, %v; forwarded %q", n, err, upstream.written.String())
	}
}

func TestHostControlProxyRequestPermitHasBoundedUpstreamWait(t *testing.T) {
	transport := &hostControlRoundTripper{
		base: blockingRoundTripper{}, permits: &testActivityLeaseStore{},
		ref:     host.Ref{ID: testHostID, RuntimeGeneration: 3, ControlVersion: 8, ControlState: host.ControlUser},
		actorID: testUserID, permission: host.PermissionDesktopControl, timeout: 20 * time.Millisecond,
	}
	request, err := http.NewRequest(http.MethodPost, "http://upstream.example/action", strings.NewReader("input"))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	response, err := transport.RoundTrip(request)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("blocked proxy request error=%v duration=%s; want a bounded deadline", err, time.Since(started))
	}
}
