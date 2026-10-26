package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/host"
)

func TestRFBClientParserAcceptsFixedHandshakeAndClassifiesInput(t *testing.T) {
	var parser rfbClientParser
	var stream []byte
	stream = append(stream, []byte("RFB 003.008\n")...)
	stream = append(stream, 1, 1) // None security and shared desktop.
	stream = append(stream, make([]byte, 20)...)
	stream[len(stream)-20] = 0                                     // SetPixelFormat.
	stream = append(stream, 2, 0, 0, 1, 0, 0, 0, 0)                // SetEncodings.
	stream = append(stream, 3, 0, 0, 0, 0, 0, 1, 0, 1, 0)          // FramebufferUpdateRequest.
	stream = append(stream, 4, 1, 0, 0, 0, 0, 0, 0)                // KeyEvent.
	stream = append(stream, 5, 1, 0, 1, 0, 1)                      // PointerEvent.
	stream = append(stream, 6, 0, 0, 0, 0, 0, 0, 3, 'a', 'b', 'c') // ClientCutText.
	stream = append(stream, 150, 1, 0, 0, 0, 0, 0, 1, 0, 1)        // Continuous updates.
	stream = append(stream, 251, 0, 0, 0, 1, 0, 1, 0)
	stream = append(stream, make([]byte, 16)...)                    // One resized screen.
	stream = append(stream, 255, 0, 0, 1, 0, 0, 0, 'a', 0, 0, 0, 4) // QEMU ExtendedKeyEvent.

	first, err := parser.feed(stream[:7])
	if err != nil || len(first) != 0 {
		t.Fatalf("partial handshake = %#v, err=%v", first, err)
	}
	messages, err := parser.feed(stream[7:])
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 12 {
		t.Fatalf("parsed %d messages, want 12", len(messages))
	}
	wantInput := []bool{false, false, false, false, false, false, true, true, true, false, true, true}
	for index, want := range wantInput {
		if messages[index].input != want {
			t.Errorf("message %d input=%t, want %t (type %d)", index, messages[index].input, want, messages[index].data[0])
		}
	}
}

func TestRFBClientParserAcceptsExtendedClipboardCapabilitiesAsViewerTraffic(t *testing.T) {
	var parser rfbClientParser
	parser.stage = 3
	capabilities := []byte{
		6, 0, 0, 0, 0xff, 0xff, 0xff, 0xf8,
		0x1f, 0, 0, 2, 0, 0, 0, 0,
	}
	messages, err := parser.feed(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].input || !bytes.Equal(messages[0].data, capabilities) {
		t.Fatalf("extended clipboard capabilities = %#v, want one non-input message", messages)
	}
}

func TestRFBClientParserBuffersPartialExtendedKeyEvent(t *testing.T) {
	var parser rfbClientParser
	parser.stage = 3
	message := []byte{255, 0, 0, 1, 0, 0, 0, 'a', 0, 0, 0, 4}
	if messages, err := parser.feed(message[:5]); err != nil || len(messages) != 0 {
		t.Fatalf("partial extended key event = %#v, err=%v", messages, err)
	}
	messages, err := parser.feed(message[5:])
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || !messages[0].input || !bytes.Equal(messages[0].data, message) {
		t.Fatalf("extended key event = %#v, want one exact input message", messages)
	}
}

func TestRFBClientParserClassifiesFenceResponsesAsViewerTraffic(t *testing.T) {
	var parser rfbClientParser
	parser.stage = 3
	response := []byte{248, 0, 0, 0, 0, 0, 0, 3, 2, 'o', 'k'}
	request := []byte{248, 0, 0, 0, 0x80, 0, 0, 0, 0}
	messages, err := parser.feed(append(response, request...))
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].input || !messages[1].input {
		t.Fatalf("fence messages = %#v, want a viewer response and blocked request", messages)
	}
}

func TestRFBClientParserRejectsUnknownAndOversizedClipboardMessages(t *testing.T) {
	for name, input := range map[string][]byte{
		"unknown client message":                  {127},
		"oversized clipboard":                     {6, 0, 0, 0, 0xff, 0xff, 0xff, 0xff},
		"oversized extended clipboard":            {6, 0, 0, 0, 0x80, 0, 0, 0},
		"invalid extended clipboard capabilities": {6, 0, 0, 0, 0xff, 0xff, 0xff, 0xfc, 0x1f, 0, 0, 2},
		"unknown QEMU client subtype":             {255, 1},
		"invalid extended key state":              {255, 0, 0, 2},
	} {
		t.Run(name, func(t *testing.T) {
			var parser rfbClientParser
			parser.stage = 3
			if _, err := parser.feed(input); err == nil {
				t.Fatal("malformed or unsupported RFB message was accepted")
			}
		})
	}
	var parser rfbClientParser
	if _, err := parser.feed([]byte("RFB 003.007\n")); err == nil {
		t.Fatal("unsupported RFB version was accepted")
	}
}

func TestRFBWebSocketBodyAllowsViewerTrafficAndBlocksAIMutations(t *testing.T) {
	mutations := map[string][]byte{
		"keyboard":      {4, 1, 0, 0, 0, 0, 0, 0},
		"pointer":       {5, 1, 0, 1, 0, 1},
		"clipboard":     {6, 0, 0, 0, 0, 0, 0, 3, 'a', 'b', 'c'},
		"fence request": {248, 0, 0, 0, 0x80, 0, 0, 0, 0},
		"desktop size":  {251, 0, 0, 1, 0, 1, 0, 0},
		"extended key":  {255, 0, 0, 1, 0, 0, 0, 'a', 0, 0, 0, 4},
		"unknown":       {127},
	}
	for name, mutation := range mutations {
		t.Run(name, func(t *testing.T) {
			ref := host.Ref{ID: testHostID, RuntimeGeneration: 1, ControlVersion: 4, ControlState: host.ControlAI}
			upstream := &rfbCaptureBody{}
			permits := &testActivityLeaseStore{}
			body := newRFBFilteringBody(upstream, context.Background(), ref, permits, true, nil)
			viewerMessages := append([]byte("RFB 003.008\n"), 1, 1)
			viewerMessages = append(viewerMessages, 3, 0, 0, 0, 0, 0, 1, 0, 1, 0)
			frame := testMaskedWebSocketFrame(2, true, viewerMessages)
			if n, err := body.Write(frame); err != nil || n != len(frame) {
				t.Fatalf("write RFB viewer frame = %d, %v", n, err)
			}
			before := append([]byte(nil), upstream.written.Bytes()...)
			if _, err := body.Write(testMaskedWebSocketFrame(2, true, mutation)); err == nil {
				t.Fatalf("AI mode forwarded %s input", name)
			}
			if !bytes.Equal(before, upstream.written.Bytes()) {
				t.Fatalf("AI %s input reached the upstream", name)
			}
			if len(permits.inputRefs) != 0 {
				t.Fatalf("AI mode requested a user input permit: %#v", permits.inputRefs)
			}
			if got := decodeProxyWebSocketPayloads(t, upstream.written.Bytes()); !bytes.Equal(bytes.Join(got, nil), viewerMessages) {
				t.Fatalf("RFB viewer bytes changed: %x", bytes.Join(got, nil))
			}
		})
	}
}

func TestRFBWebSocketBodyDropsAllMemberInputWithoutPermits(t *testing.T) {
	ref := host.Ref{ID: testHostID, RuntimeGeneration: 1, ControlVersion: 4, ControlState: host.ControlAI}
	upstream := &rfbCaptureBody{}
	permits := &testActivityLeaseStore{}
	body := newRFBFilteringBody(upstream, context.Background(), ref, permits, false, nil)
	viewerTraffic := append([]byte("RFB 003.008\n"), 1, 1)
	viewerTraffic = append(viewerTraffic, 3, 0, 0, 0, 0, 0, 1, 0, 1, 0) // framebuffer update request
	if _, err := body.Write(testMaskedWebSocketFrame(2, true, viewerTraffic)); err != nil {
		t.Fatalf("write viewer protocol traffic: %v", err)
	}
	input := []byte{4, 1, 0, 0, 0, 0, 0, 0} // key event
	if _, err := body.Write(testMaskedWebSocketFrame(2, true, input)); err != nil {
		t.Fatalf("read-only viewer input should be silently discarded: %v", err)
	}
	forwarded := decodeProxyWebSocketPayloads(t, upstream.written.Bytes())
	if len(forwarded) != 1 || !bytes.Equal(forwarded[0], viewerTraffic) {
		t.Fatalf("forwarded viewer payloads = %#v, want only protocol/view traffic", forwarded)
	}
	if len(permits.inputRefs) != 0 {
		t.Fatalf("read-only viewer requested input permits: %#v", permits.inputRefs)
	}
}

func TestRFBWebSocketBodyAllowsClipboardCapabilitiesButBlocksClipboardDataInAI(t *testing.T) {
	ref := host.Ref{ID: testHostID, RuntimeGeneration: 1, ControlVersion: 4, ControlState: host.ControlAI}
	upstream := &rfbCaptureBody{}
	body := newRFBFilteringBody(upstream, context.Background(), ref, &testActivityLeaseStore{}, true, nil)
	handshake := append([]byte("RFB 003.008\n"), 1, 1)
	if _, err := body.Write(testMaskedWebSocketFrame(2, true, handshake)); err != nil {
		t.Fatal(err)
	}
	capabilities := []byte{6, 0, 0, 0, 0xff, 0xff, 0xff, 0xf8, 0x1f, 0, 0, 2, 0, 0, 0, 0}
	if _, err := body.Write(testMaskedWebSocketFrame(2, true, capabilities)); err != nil {
		t.Fatalf("AI mode rejected noVNC clipboard capability negotiation: %v", err)
	}
	fenceResponse := []byte{248, 0, 0, 0, 0, 0, 0, 3, 2, 'o', 'k'}
	if _, err := body.Write(testMaskedWebSocketFrame(2, true, fenceResponse)); err != nil {
		t.Fatalf("AI mode rejected noVNC fence response: %v", err)
	}
	forwarded := decodeProxyWebSocketPayloads(t, upstream.written.Bytes())
	if len(forwarded) != 3 || !bytes.Equal(forwarded[1], capabilities) || !bytes.Equal(forwarded[2], fenceResponse) {
		t.Fatalf("forwarded RFB payloads = %#v, want handshake, capabilities and fence response", forwarded)
	}

	clipboard := []byte{6, 0, 0, 0, 0, 0, 0, 3, 'a', 'b', 'c'}
	if _, err := body.Write(testMaskedWebSocketFrame(2, true, clipboard)); err == nil {
		t.Fatal("AI mode accepted clipboard data")
	}
	if got := decodeProxyWebSocketPayloads(t, upstream.written.Bytes()); len(got) != 3 {
		t.Fatalf("AI clipboard data reached the upstream: %#v", got)
	}
}

func TestRFBWebSocketBodyRequiresCurrentUserPermit(t *testing.T) {
	ref := host.Ref{ID: testHostID, RuntimeGeneration: 3, ControlVersion: 8, ControlState: host.ControlUser}
	upstream := &rfbCaptureBody{}
	permits := &testActivityLeaseStore{}
	var filterError error
	body := newRFBFilteringBody(upstream, context.Background(), ref, permits, true, func(err error) { filterError = err })
	handshake := append([]byte("RFB 003.008\n"), 1, 1)
	if _, err := body.Write(testMaskedWebSocketFrame(2, true, handshake)); err != nil {
		t.Fatal(err)
	}
	mutations := [][]byte{
		{4, 1, 0, 0, 0, 0, 0, 0},
		{5, 1, 0, 1, 0, 1},
		{6, 0, 0, 0, 0, 0, 0, 3, 'a', 'b', 'c'},
		{251, 0, 0, 1, 0, 1, 0, 0},
		{255, 0, 0, 1, 0, 0, 0, 'a', 0, 0, 0, 4},
	}
	for _, mutation := range mutations {
		if _, err := body.Write(testMaskedWebSocketFrame(2, true, mutation)); err != nil {
			t.Fatalf("authorized user input type %d: %v", mutation[0], err)
		}
	}
	if len(permits.inputRefs) != len(mutations) {
		t.Fatalf("input permit count = %d, want %d", len(permits.inputRefs), len(mutations))
	}
	for _, got := range permits.inputRefs {
		if got != ref {
			t.Fatalf("input permit fence = %#v, want %#v", got, ref)
		}
	}
	payloads := decodeProxyWebSocketPayloads(t, upstream.written.Bytes())
	if len(payloads) != len(mutations)+1 {
		t.Fatalf("forwarded RFB payload count = %d, want %d", len(payloads), len(mutations)+1)
	}
	for index, mutation := range mutations {
		if !bytes.Equal(payloads[index+1], mutation) {
			t.Fatalf("user input %d was not forwarded as the exact RFB message: %x", index, payloads[index+1])
		}
	}
	permits.inputErr = context.Canceled
	blocked := []byte{5, 1, 0, 1, 0, 1}
	before := append([]byte(nil), upstream.written.Bytes()...)
	if _, err := body.Write(testMaskedWebSocketFrame(2, true, blocked)); err == nil {
		t.Fatal("stale user input permit was accepted")
	} else if filterError == nil || filterError.Error() != err.Error() {
		t.Fatalf("filter error callback = %v, write error = %v", filterError, err)
	}
	if !bytes.Equal(before, upstream.written.Bytes()) {
		t.Fatal("input was forwarded after permit rejection")
	}
}

func TestRFBInputWriteTimeoutClosesSocket(t *testing.T) {
	upstream := &rfbBlockingBody{closed: make(chan struct{})}
	permits := &testActivityLeaseStore{}
	body := newRFBFilteringBody(upstream, context.Background(), host.Ref{
		ID: testHostID, RuntimeGeneration: 1, ControlVersion: 4, ControlState: host.ControlUser,
	}, permits, true, nil)
	body.rfb.stage = 3

	started := time.Now()
	_, err := body.Write(testMaskedWebSocketFrame(2, true, []byte{4, 1, 0, 0, 0, 0, 0, 0}))
	if err == nil {
		t.Fatal("stalled upstream input write unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed < rfbInputWriteTimeout || elapsed > rfbInputWriteTimeout+2*time.Second {
		t.Fatalf("RFB input write timeout elapsed = %s", elapsed)
	}
	select {
	case <-upstream.closed:
	default:
		t.Fatal("stalled input write did not close its WebSocket")
	}
}

func TestClientWebSocketFrameParserRejectsMalformedFrames(t *testing.T) {
	valid := testMaskedWebSocketFrame(2, true, []byte("RFB 003.008\n"))
	valid[0] |= 0x40 // RSV1 requires a negotiated compression extension.
	if _, _, _, _, _, err := clientWebSocketFrameLength(valid); err == nil {
		t.Fatal("compressed/RSV WebSocket frame was accepted")
	}
	if _, _, _, _, _, err := clientWebSocketFrameLength([]byte{0x82, 1, 'x'}); err == nil {
		t.Fatal("unmasked client WebSocket frame was accepted")
	}
}

type rfbCaptureBody struct{ written bytes.Buffer }

type rfbBlockingBody struct {
	closed chan struct{}
	once   sync.Once
}

func (b *rfbBlockingBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, io.EOF
}

func (b *rfbBlockingBody) Write([]byte) (int, error) {
	<-b.closed
	return 0, io.ErrClosedPipe
}

func (b *rfbBlockingBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func (*rfbCaptureBody) Read([]byte) (int, error)      { return 0, io.EOF }
func (b *rfbCaptureBody) Write(p []byte) (int, error) { return b.written.Write(p) }
func (*rfbCaptureBody) Close() error                  { return nil }

func testMaskedWebSocketFrame(opcode byte, fin bool, payload []byte) []byte {
	mask := [4]byte{0x13, 0x25, 0x37, 0x49}
	first := opcode
	if fin {
		first |= 0x80
	}
	frame := []byte{first}
	switch length := len(payload); {
	case length < 126:
		frame = append(frame, 0x80|byte(length))
	case length <= 65535:
		frame = append(frame, 0x80|126, byte(length>>8), byte(length))
	default:
		frame = append(frame, 0x80|127)
		var extended [8]byte
		binary.BigEndian.PutUint64(extended[:], uint64(length))
		frame = append(frame, extended[:]...)
	}
	frame = append(frame, mask[:]...)
	for index, value := range payload {
		frame = append(frame, value^mask[index&3])
	}
	return frame
}

func decodeProxyWebSocketPayloads(t *testing.T, raw []byte) [][]byte {
	t.Helper()
	var payloads [][]byte
	for len(raw) > 0 {
		frameLength, start, length, _, opcode, err := clientWebSocketFrameLength(raw)
		if err != nil || frameLength == 0 {
			t.Fatalf("invalid proxy WebSocket frame: length=%d err=%v", frameLength, err)
		}
		if opcode != 2 {
			t.Fatalf("proxy emitted non-binary frame opcode %d", opcode)
		}
		mask := raw[start-4 : start]
		payload := make([]byte, length)
		for index := range payload {
			payload[index] = raw[start+index] ^ mask[index&3]
		}
		payloads = append(payloads, payload)
		raw = raw[frameLength:]
	}
	return payloads
}

func TestMainAIORequestPolicy(t *testing.T) {
	for _, test := range []struct {
		name   string
		state  host.ControlState
		method string
		path   string
		ws     bool
		want   bool
	}{
		{name: "AI noVNC assets", state: host.ControlAI, method: http.MethodGet, path: "/vnc/index.html", want: true},
		{name: "AI code-server denied", state: host.ControlAI, method: http.MethodGet, path: "/code-server/"},
		{name: "AI code-server socket denied", state: host.ControlAI, method: http.MethodGet, path: "/code-server/", ws: true},
		{name: "AI terminal denied", state: host.ControlAI, method: http.MethodPost, path: "/v1/shell/exec"},
		{name: "AI file read denied", state: host.ControlAI, method: http.MethodPost, path: "/v1/file/read"},
		{name: "AI VNC socket allowed", state: host.ControlAI, method: http.MethodGet, path: "/ws", ws: true, want: true},
		{name: "AI shell socket denied", state: host.ControlAI, method: http.MethodGet, path: "/v1/shell/ws", ws: true},
		{name: "AI CDP socket denied", state: host.ControlAI, method: http.MethodGet, path: "/cdp/devtools/browser/id", ws: true},
		{name: "user shell socket allowed", state: host.ControlUser, method: http.MethodGet, path: "/v1/shell/ws", ws: true, want: true},
		{name: "user code-server allowed", state: host.ControlUser, method: http.MethodGet, path: "/code-server/", want: true},
		{name: "user code-server socket allowed", state: host.ControlUser, method: http.MethodGet, path: "/code-server/", ws: true, want: true},
		{name: "user code-server lookalike socket denied", state: host.ControlUser, method: http.MethodGet, path: "/code-server.evil/", ws: true},
		{name: "user unknown socket denied", state: host.ControlUser, method: http.MethodGet, path: "/unregistered/ws", ws: true},
		{name: "AI path traversal denied", state: host.ControlAI, method: http.MethodGet, path: "/vnc/../v1/shell/exec"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(test.method, "http://host.test"+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if test.ws {
				request.Header.Set("Upgrade", "websocket")
				request.Header.Set("Connection", "keep-alive, Upgrade")
			}
			if got := allowMainAIORequest(test.state, request); got != test.want {
				t.Fatalf("allowMainAIORequest() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestGatewayNoVNCConnectionFiltersAIInputAndPermitsHumanInput(t *testing.T) {
	for _, test := range []struct {
		name        string
		state       host.ControlState
		actorID     string
		permissions []host.Permission
		wantInput   bool
	}{
		{name: "AI controlled owner", state: host.ControlAI, actorID: testUserID},
		{name: "user controlled owner", state: host.ControlUser, actorID: testUserID, wantInput: true},
		{name: "user controlled desktop controller", state: host.ControlUser, actorID: testMemberID,
			permissions: []host.Permission{host.PermissionDesktopControl}, wantInput: true},
		{name: "user controlled viewer", state: host.ControlUser, actorID: testMemberID,
			permissions: []host.Permission{host.PermissionDesktopView}},
	} {
		t.Run(test.name, func(t *testing.T) {
			frames := make(chan []byte, 8)
			extensions := make(chan string, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				extensions <- r.Header.Get("Sec-WebSocket-Extensions")
				connection, buffer, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				defer func() { _ = connection.Close() }()
				accept := websocketAccept(r.Header.Get("Sec-WebSocket-Key"))
				_, _ = fmt.Fprintf(buffer, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\nSec-WebSocket-Extensions: permessage-deflate\r\n\r\n", accept)
				if buffer.Flush() != nil {
					return
				}
				for {
					opcode, payload, err := readMaskedWebSocketFrame(buffer.Reader)
					if err != nil {
						return
					}
					if opcode == 2 {
						frames <- payload
					}
				}
			}))
			defer upstream.Close()

			handler, gatewayValue, runtime, _, _ := newTestDeps(t, app.Upstream{BaseURL: upstream.URL, APIKey: "aio-secret"})
			runtime.ref.ControlState = test.state
			if test.actorID != testUserID {
				setMemberPermissionGrants(gatewayValue, test.permissions...)
			}
			cookie := hostCookie(gatewayValue, testHostID, test.actorID, time.Hour)
			server := httptest.NewServer(handler)
			defer server.Close()
			connection, _ := connectGatewayVNCWebSocket(t, server, cookie)
			defer func() { _ = connection.Close() }()
			select {
			case offered := <-extensions:
				if offered != "" {
					t.Fatalf("Gateway offered WebSocket compression upstream: %q", offered)
				}
			case <-time.After(time.Second):
				t.Fatal("upstream WebSocket handshake did not arrive")
			}

			viewer := append([]byte("RFB 003.008\n"), 1, 1)
			viewer = append(viewer, 3, 0, 0, 0, 0, 0, 1, 0, 1, 0)
			if _, err := connection.Write(testMaskedWebSocketFrame(2, true, viewer)); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-frames:
				if !bytes.Equal(got, viewer) {
					t.Fatalf("viewer RFB bytes = %x, want %x", got, viewer)
				}
			case <-time.After(time.Second):
				t.Fatal("read-only RFB traffic did not reach AIO")
			}

			input := []byte{4, 1, 0, 0, 0, 0, 0, 0}
			if _, err := connection.Write(testMaskedWebSocketFrame(2, true, input)); err != nil {
				t.Fatal(err)
			}
			if test.wantInput {
				select {
				case got := <-frames:
					if !bytes.Equal(got, input) {
						t.Fatalf("authorized user input = %x, want %x", got, input)
					}
				case <-time.After(time.Second):
					t.Fatal("user-controlled RFB input did not reach AIO")
				}
				leases := gatewayValue.deps.ActivityLeases.(*testActivityLeaseStore)
				leases.mu.Lock()
				defer leases.mu.Unlock()
				if len(leases.inputRefs) != 1 || leases.inputRefs[0].ControlVersion != runtime.ref.ControlVersion {
					t.Fatalf("user input was not fenced by the live Host control version: %#v", leases.inputRefs)
				}
			} else {
				select {
				case got := <-frames:
					t.Fatalf("unauthorized keyboard input reached AIO: %x", got)
				case <-time.After(100 * time.Millisecond):
				}
			}
		})
	}
}

func connectGatewayVNCWebSocket(t *testing.T, server *httptest.Server, cookie string) (net.Conn, *bufio.Reader) {
	t.Helper()
	connection, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	_, err = fmt.Fprintf(connection,
		"GET /ws HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Extensions: permessage-deflate\r\nOrigin: https://%s\r\nCookie: %s=%s\r\n\r\n",
		hostAuthority(), key, hostAuthority(), hostCookieName, cookie)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "101 Switching Protocols") {
		t.Fatalf("Gateway VNC handshake = %q, err=%v", status, err)
	}
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.HasPrefix(strings.ToLower(line), "sec-websocket-extensions:") {
			t.Fatalf("Gateway negotiated unsupported WebSocket extensions: %q", line)
		}
		if line == "\r\n" {
			break
		}
	}
	return connection, reader
}

func readMaskedWebSocketFrame(reader *bufio.Reader) (byte, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		var extended [2]byte
		if _, err := io.ReadFull(reader, extended[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(extended[:]))
	case 127:
		var extended [8]byte
		if _, err := io.ReadFull(reader, extended[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(extended[:])
	}
	if length > maxRFBMessageBytes || header[1]&0x80 == 0 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	var mask [4]byte
	if _, err := io.ReadFull(reader, mask[:]); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	for index := range payload {
		payload[index] ^= mask[index&3]
	}
	return header[0] & 0x0f, payload, nil
}
