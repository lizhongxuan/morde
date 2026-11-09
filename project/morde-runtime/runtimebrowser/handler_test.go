package runtimebrowser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"math/rand"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"morde-runtime/contract"
	"morde-runtime/runtimecore"
)

func TestBrowserHandlersAreOwnedAndIsolated(t *testing.T) {
	controller, state := newTestController(t)
	defer controller.Close()
	if got := len(controller.Handlers()); got != 9 {
		t.Fatalf("handler count = %d, want 9", got)
	}

	_, err := executeBrowser(t, controller, "q0000001", "o0000001", "snapshot", `{"target_id":"user-tab-1"}`)
	if err != runtimecore.ErrFenceRejected {
		t.Fatalf("default-profile target error = %v, want fence rejection", err)
	}

	resultA, err := executeBrowser(t, controller, "q0000001", "o0000002", "open_tab", `{"url":"https://example.test/a"}`)
	if err != nil {
		t.Fatalf("open task A tab: %v", err)
	}
	resultB, err := executeBrowser(t, controller, "q0000002", "o0000003", "open_tab", `{"url":"https://example.test/b"}`)
	if err != nil {
		t.Fatalf("open task B tab: %v", err)
	}
	var tabA, tabB struct {
		TargetID string `json:"target_id"`
	}
	if err := json.Unmarshal(resultA, &tabA); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(resultB, &tabB); err != nil {
		t.Fatal(err)
	}
	if tabA.TargetID == "" || tabB.TargetID == "" || tabA.TargetID == tabB.TargetID {
		t.Fatalf("unexpected target IDs: A=%q B=%q", tabA.TargetID, tabB.TargetID)
	}
	if _, err := executeBrowser(t, controller, "q0000002", "o0000004", "snapshot", `{"target_id":"`+tabA.TargetID+`"}`); err != runtimecore.ErrFenceRejected {
		t.Fatalf("cross-task target error = %v, want fence rejection", err)
	}
	assertTargetPresent(t, state, "user-tab-1", "")
	assertTaskTargetPresent(t, controller, state, "q0000001", tabA.TargetID)
	assertTaskTargetPresent(t, controller, state, "q0000002", tabB.TargetID)
}

func TestBrowserHandoffActivatesOnlyTheOwnedTaskTab(t *testing.T) {
	controller, state := newTestController(t)
	defer controller.Close()

	open := func(taskID, operationID, pageURL string) string {
		t.Helper()
		result, err := executeBrowser(t, controller, taskID, operationID, "open_tab", `{"url":"`+pageURL+`"}`)
		if err != nil {
			t.Fatalf("open tab for %s: %v", taskID, err)
		}
		var value struct {
			TargetID string `json:"target_id"`
		}
		if err := json.Unmarshal(result, &value); err != nil || value.TargetID == "" {
			t.Fatalf("open tab result = %s, %v", result, err)
		}
		return value.TargetID
	}
	targetA := open("q0000001", "o0000101", "https://example.test/a")
	targetB := open("q0000002", "o0000102", "https://example.test/b")

	result, err := executeBrowser(t, controller, "q0000001", "o0000103", "handoff", `{"target_id":"`+targetA+`"}`)
	if err != nil {
		t.Fatalf("activate Task A page for user takeover: %v", err)
	}
	var activated struct {
		TargetID string `json:"target_id"`
		Active   bool   `json:"active"`
	}
	if err := json.Unmarshal(result, &activated); err != nil || activated.TargetID != targetA || !activated.Active {
		t.Fatalf("handoff result = %#v, %s, %v", activated, result, err)
	}

	state.mu.Lock()
	activeTarget := state.activeTarget
	state.mu.Unlock()
	if activeTarget != targetA {
		t.Fatalf("active browser target = %q, want Task A target %q", activeTarget, targetA)
	}
	if _, err := executeBrowser(t, controller, "q0000002", "o0000104", "handoff", `{"target_id":"`+targetA+`"}`); err != runtimecore.ErrFenceRejected {
		t.Fatalf("cross-Task browser handoff error = %v, want fence rejection", err)
	}
	state.mu.Lock()
	activeTarget = state.activeTarget
	state.mu.Unlock()
	if activeTarget != targetA || targetB == targetA {
		t.Fatalf("cross-Task handoff changed the active page: active=%q TaskA=%q TaskB=%q", activeTarget, targetA, targetB)
	}
}

func TestCancelledOpenTabStopsOnlyItsTaskAndPreservesUserTab(t *testing.T) {
	controller, state := newTestController(t)
	defer controller.Close()
	resultB, err := executeBrowser(t, controller, "q0000002", "o0000010", "open_tab", `{"url":"https://example.test/b"}`)
	if err != nil {
		t.Fatalf("open task B tab: %v", err)
	}
	var tabB struct {
		TargetID string `json:"target_id"`
	}
	if err := json.Unmarshal(resultB, &tabB); err != nil {
		t.Fatal(err)
	}

	state.mu.Lock()
	state.blockNextCreateTarget = true
	state.createTargetEntered = make(chan string, 1)
	state.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	executeDone := make(chan error, 1)
	go func() {
		_, executeErr := executeBrowserContext(ctx, controller, "q0000001", "o0000011", "open_tab", `{"url":"https://example.test/a"}`)
		executeDone <- executeErr
	}()
	var taskAContext string
	select {
	case taskAContext = <-state.createTargetEntered:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("open_tab did not reach its owned target creation")
	}
	if taskAContext == "" {
		cancel()
		t.Fatal("fake CDP did not report task A context")
	}
	cancel()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	stop, err := handlerFor(controller, capabilityBrowserActions, "open_tab").Stop(stopCtx, contract.Operation{
		OperationID: "o0000011", TaskID: "q0000001", Capability: capabilityBrowserActions, Action: "open_tab",
	})
	if err != nil || !stop.VerifiedStopped {
		t.Fatalf("Stop result=%+v err=%v, want verified task stop", stop, err)
	}
	if executeErr := <-executeDone; !errors.Is(executeErr, runtimecore.ErrOperationUncertain) {
		t.Fatalf("cancelled open_tab error = %v, want uncertain", executeErr)
	}
	if hasContext(state, taskAContext) {
		t.Fatalf("task A context %q remains after verified Stop", taskAContext)
	}
	assertTaskTargetPresent(t, controller, state, "q0000002", tabB.TargetID)
	assertTargetPresent(t, state, "user-tab-1", "")
}

func TestStopFailsClosedAcrossCDPDisconnectAndReconnect(t *testing.T) {
	controller, state := newTestController(t)
	defer controller.Close()
	result, err := executeBrowser(t, controller, "q0000001", "o0000020", "open_tab", `{"url":"https://example.test/a"}`)
	if err != nil {
		t.Fatal(err)
	}
	var tab struct {
		TargetID string `json:"target_id"`
	}
	if err := json.Unmarshal(result, &tab); err != nil {
		t.Fatal(err)
	}
	_, err = executeBrowser(t, controller, "q0000001", "o0000021", "snapshot", `{"target_id":"`+tab.TargetID+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	contextValue, ok := controller.registry.taskContext("q0000001")
	if !ok {
		t.Fatal("task context is missing")
	}
	state.setUnavailable(true)
	stop, err := handlerFor(controller, capabilityBrowserObserve, "snapshot").Stop(context.Background(), contract.Operation{
		OperationID: "o0000021", TaskID: "q0000001", Capability: capabilityBrowserObserve, Action: "snapshot",
	})
	if err != nil || stop.VerifiedStopped {
		t.Fatalf("disconnected Stop result=%+v err=%v, want unverified", stop, err)
	}
	if !hasContext(state, contextValue.ContextID) {
		t.Fatal("disconnected Stop unexpectedly changed browser context")
	}
	state.setUnavailable(false)
	stop, err = handlerFor(controller, capabilityBrowserObserve, "snapshot").Stop(context.Background(), contract.Operation{
		OperationID: "o0000021", TaskID: "q0000001", Capability: capabilityBrowserObserve, Action: "snapshot",
	})
	if err != nil || !stop.VerifiedStopped {
		t.Fatalf("reconnected Stop result=%+v err=%v, want verified stop", stop, err)
	}
	if hasContext(state, contextValue.ContextID) {
		t.Fatal("task context remains after verified Stop")
	}
	assertTargetPresent(t, state, "user-tab-1", "")
}

func TestRestartReconcilesOwnedContextButLeavesOperationUncertain(t *testing.T) {
	controller, state, storeDir := newTestControllerWithStore(t)
	result, err := executeBrowser(t, controller, "q0000001", "o0000030", "open_tab", `{"url":"https://example.test/a"}`)
	if err != nil {
		t.Fatal(err)
	}
	var tab struct {
		TargetID string `json:"target_id"`
	}
	if err := json.Unmarshal(result, &tab); err != nil {
		t.Fatal(err)
	}
	contextValue, ok := controller.registry.taskContext("q0000001")
	if !ok {
		t.Fatal("task context is missing")
	}
	if err := controller.registry.begin(operationBinding{
		OperationID: "o0000031", TaskID: "q0000001", Action: "navigate",
		ContextID: contextValue.ContextID, TargetID: tab.TargetID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := controller.registry.markEffectStarted("o0000031"); err != nil {
		t.Fatal(err)
	}
	if err := controller.registry.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewHandlers(Options{StoreDir: storeDir, ExpectedOwnerUID: uint32(os.Geteuid()), connector: &fakeConnector{state: state}})
	if err != nil {
		t.Fatalf("restart controller: %v", err)
	}
	defer restarted.Close()
	if hasContext(state, contextValue.ContextID) {
		t.Fatal("restart left an owned task context live")
	}
	operation, ok := restarted.registry.operation("o0000031")
	if !ok || operation.Completed || !operation.EffectMayHaveRun {
		t.Fatalf("restart rewrote unresolved operation: %+v exists=%v", operation, ok)
	}
	assertTargetPresent(t, state, "user-tab-1", "")
	_ = controller.Close()
}

func TestScreenshotIsBoundedJPEGAndReturnsContractShape(t *testing.T) {
	controller, state := newTestController(t)
	defer controller.Close()
	result, err := executeBrowser(t, controller, "q0000001", "o0000040", "open_tab", `{"url":"https://example.test/a"}`)
	if err != nil {
		t.Fatal(err)
	}
	var tab struct {
		TargetID string `json:"target_id"`
	}
	if err := json.Unmarshal(result, &tab); err != nil {
		t.Fatal(err)
	}
	largeScreenshot := encodeTestJPEG(t, 1920, 1080, 95, true)
	if len(largeScreenshot) <= maxScreenshotBytes {
		t.Fatalf("test screenshot is only %d bytes; want input larger than %d", len(largeScreenshot), maxScreenshotBytes)
	}
	state.mu.Lock()
	state.screenshotData = largeScreenshot
	state.mu.Unlock()
	result, err = executeBrowser(t, controller, "q0000001", "o0000041", "screenshot", `{"target_id":"`+tab.TargetID+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	var screenshotResult struct {
		TargetID   string `json:"target_id"`
		MIMEType   string `json:"mime_type"`
		DataBase64 string `json:"data_base64"`
	}
	if err := json.Unmarshal(result, &screenshotResult); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(screenshotResult.DataBase64)
	if err != nil || screenshotResult.TargetID != tab.TargetID || screenshotResult.MIMEType != "image/jpeg" || len(decoded) > maxScreenshotBytes {
		t.Fatalf("unexpected screenshot result: %+v decodedBytes=%d err=%v", screenshotResult, len(decoded), err)
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(decoded))
	if err != nil || config.Width > maxBrowserScreenshotWidth || config.Height > maxBrowserScreenshotHeight {
		t.Fatalf("screenshot dimensions = %dx%d, err=%v; want at most %dx%d", config.Width, config.Height, err, maxBrowserScreenshotWidth, maxBrowserScreenshotHeight)
	}
}

func TestNormalizeBrowserScreenshotRejectsOversizedInput(t *testing.T) {
	if _, err := normalizeBrowserScreenshot(make([]byte, maxBrowserScreenshotInputBytes+1)); err != runtimecore.ErrResultTooLarge {
		t.Fatalf("oversized screenshot error = %v, want %v", err, runtimecore.ErrResultTooLarge)
	}
}

func TestBrowserScreenshotInputLimitFitsCDPMessage(t *testing.T) {
	encodedBytes := base64.StdEncoding.EncodedLen(maxBrowserScreenshotInputBytes)
	if encodedBytes+maxBrowserScreenshotMessageReserveBytes > maxCDPMessageBytes {
		t.Fatalf("screenshot base64 (%d) plus protocol reserve (%d) exceeds CDP message limit (%d)",
			encodedBytes, maxBrowserScreenshotMessageReserveBytes, maxCDPMessageBytes)
	}
}

func TestFieldObservationReturnsOnlyBoundedFingerprint(t *testing.T) {
	controller, state := newTestController(t)
	defer controller.Close()
	state.mu.Lock()
	state.observeText = "private account text"
	state.observeValue = "private input value"
	state.mu.Unlock()

	for index, test := range []struct {
		selector string
		field    string
		value    string
	}{
		{selector: "#account", field: "text", value: "private account text"},
		{selector: "input[name=secret]", field: "value", value: "private input value"},
	} {
		args, err := json.Marshal(fieldArguments{URL: "https://example.test/profile?token=private", Selector: test.selector, Field: test.field})
		if err != nil {
			t.Fatal(err)
		}
		result, err := executeBrowser(t, controller, "q0000001", "o000010"+itoa(index+1), "field", string(args))
		if err != nil {
			t.Fatalf("observe %s field: %v", test.field, err)
		}
		var got struct {
			Fingerprint string `json:"fingerprint"`
			Origin      string `json:"origin"`
			MatchCount  int    `json:"match_count"`
		}
		if err := json.Unmarshal(result, &got); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(test.value))
		if got.Fingerprint != hex.EncodeToString(digest[:]) || got.Origin != "https://example.test" || got.MatchCount != 1 {
			t.Fatalf("unexpected observation result: %+v", got)
		}
		if strings.Contains(string(result), test.value) || strings.Contains(string(result), "token=private") {
			t.Fatalf("observation result exposed source data: %s", result)
		}
	}
}

func TestFieldObservationFailsClosedForInvalidURLOrMatchCount(t *testing.T) {
	controller, state := newTestController(t)
	defer controller.Close()

	if _, err := executeBrowser(t, controller, "q0000001", "o0000110", "field", `{"url":"file:///etc/passwd","selector":"#account","field":"text"}`); err != runtimecore.ErrInvalidRequest {
		t.Fatalf("invalid URL error = %v, want invalid request", err)
	}
	for index, count := range []int{0, 2} {
		state.mu.Lock()
		state.observeCount = count
		state.mu.Unlock()
		args := `{"url":"https://example.test/profile","selector":"#account","field":"text"}`
		result, err := executeBrowser(t, controller, "q0000001", "o000011"+itoa(index+1), "field", args)
		if err == nil || result != nil {
			t.Fatalf("match count %d result=%s err=%v, want fail-closed error", count, result, err)
		}
		if strings.Contains(err.Error(), "private") {
			t.Fatalf("error exposed observed data: %v", err)
		}
	}
}

func newTestController(t *testing.T) (*Controller, *fakeBrowserState) {
	controller, state, _ := newTestControllerWithStore(t)
	return controller, state
}

func newTestControllerWithStore(t *testing.T) (*Controller, *fakeBrowserState, string) {
	t.Helper()
	storeDir := t.TempDir()
	if err := os.Chmod(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	state := &fakeBrowserState{
		contexts: make(map[string]uint64), targets: map[string]targetInfo{
			"user-tab-1": {TargetID: "user-tab-1", Type: "page", URL: "https://user.example", Title: "User tab"},
		}, available: true, observeCount: 1, observeOrigin: "https://example.test",
		screenshotData: encodeTestJPEG(t, 16, 16, 70, false),
	}
	controller, err := NewHandlers(Options{StoreDir: storeDir, ExpectedOwnerUID: uint32(os.Geteuid()), connector: &fakeConnector{state: state}})
	if err != nil {
		t.Fatalf("new browser controller: %v", err)
	}
	t.Cleanup(func() { _ = controller.Close() })
	return controller, state, storeDir
}

func encodeTestJPEG(t *testing.T, width, height, quality int, noise bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	if noise {
		random := rand.New(rand.NewSource(1))
		for index := 0; index < len(img.Pix); index += 4 {
			img.Pix[index] = byte(random.Intn(256))
			img.Pix[index+1] = byte(random.Intn(256))
			img.Pix[index+2] = byte(random.Intn(256))
			img.Pix[index+3] = 0xff
		}
	}
	buffer := &bytes.Buffer{}
	if err := jpeg.Encode(buffer, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func executeBrowser(t *testing.T, controller *Controller, taskID, operationID, action, args string) (json.RawMessage, error) {
	t.Helper()
	return executeBrowserContext(context.Background(), controller, taskID, operationID, action, args)
}

func executeBrowserContext(ctx context.Context, controller *Controller, taskID, operationID, action, args string) (json.RawMessage, error) {
	capability := capabilityBrowserActions
	if action == "snapshot" || action == "screenshot" || action == "field" {
		capability = capabilityBrowserObserve
	}
	return handlerFor(controller, capability, action).Execute(ctx, contract.ExecuteRequest{
		TaskID: taskID, OperationID: operationID, Capability: capability, Action: action,
		Arguments: json.RawMessage(args),
	})
}

func handlerFor(controller *Controller, capability, action string) runtimecore.ActionHandler {
	for _, handler := range controller.Handlers() {
		if handler.Capability() == capability && handler.Action() == action {
			return handler
		}
	}
	panic("missing browser action handler: " + capability + "/" + action)
}

func assertTargetPresent(t *testing.T, state *fakeBrowserState, targetID, contextID string) {
	t.Helper()
	state.mu.Lock()
	defer state.mu.Unlock()
	target, ok := state.targets[targetID]
	if !ok || target.BrowserContext != contextID {
		t.Fatalf("target %q missing or in wrong context: %+v exists=%v", targetID, target, ok)
	}
}

func assertTaskTargetPresent(t *testing.T, controller *Controller, state *fakeBrowserState, taskID, targetID string) {
	t.Helper()
	owned, ok := controller.registry.target(taskID, targetID)
	if !ok {
		t.Fatalf("target %q is not owned by task %q", targetID, taskID)
	}
	assertTargetPresent(t, state, targetID, owned.ContextID)
}

func hasContext(state *fakeBrowserState, contextID string) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	_, ok := state.contexts[contextID]
	return ok
}

type fakeBrowserState struct {
	mu sync.Mutex

	contexts  map[string]uint64
	targets   map[string]targetInfo
	nextConn  int
	nextCtx   int
	nextTab   int
	available bool

	blockNextCreateTarget bool
	createTargetEntered   chan string
	observeText           string
	observeValue          string
	observeCount          int
	observeOrigin         string
	screenshotData        []byte
	activeTarget          string
}

func (s *fakeBrowserState) setUnavailable(value bool) {
	s.mu.Lock()
	s.available = !value
	s.mu.Unlock()
}

type fakeConnector struct{ state *fakeBrowserState }

func (c *fakeConnector) Connect(ctx context.Context) (browserConnection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	if !c.state.available {
		return nil, errors.New("fake CDP disconnected")
	}
	c.state.nextConn++
	return &fakeBrowserConnection{state: c.state, id: uint64(c.state.nextConn)}, nil
}

type fakeBrowserConnection struct {
	state  *fakeBrowserState
	id     uint64
	mu     sync.Mutex
	closed bool
}

func (c *fakeBrowserConnection) Version(context.Context) (string, error) { return "FakeChrome/1", nil }

func (c *fakeBrowserConnection) BrowserContexts(ctx context.Context) ([]string, error) {
	if err := c.check(ctx); err != nil {
		return nil, err
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	ids := make([]string, 0, len(c.state.contexts))
	for id := range c.state.contexts {
		ids = append(ids, id)
	}
	return ids, nil
}

func (c *fakeBrowserConnection) CreateBrowserContext(ctx context.Context, disposeOnDetach bool) (string, error) {
	if err := c.check(ctx); err != nil {
		return "", err
	}
	if !disposeOnDetach {
		return "", errors.New("fake rejects persistent contexts")
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	c.state.nextCtx++
	id := "ctx-" + itoa(c.state.nextCtx)
	c.state.contexts[id] = c.id
	return id, nil
}

func (c *fakeBrowserConnection) DisposeBrowserContext(ctx context.Context, contextID string) error {
	if err := c.check(ctx); err != nil {
		return err
	}
	c.state.mu.Lock()
	deleteContext(c.state, contextID)
	c.state.mu.Unlock()
	return nil
}

func (c *fakeBrowserConnection) Targets(ctx context.Context) ([]targetInfo, error) {
	if err := c.check(ctx); err != nil {
		return nil, err
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	targets := make([]targetInfo, 0, len(c.state.targets))
	for _, target := range c.state.targets {
		targets = append(targets, target)
	}
	return targets, nil
}

func (c *fakeBrowserConnection) CreateTarget(ctx context.Context, contextID, rawURL string, background bool) (string, error) {
	c.state.mu.Lock()
	block := c.state.blockNextCreateTarget
	if block {
		c.state.blockNextCreateTarget = false
	}
	entered := c.state.createTargetEntered
	c.state.mu.Unlock()
	if block {
		select {
		case entered <- contextID:
		default:
		}
		<-ctx.Done()
		_ = c.Close()
		return "", ctx.Err()
	}
	if err := c.check(ctx); err != nil {
		return "", err
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	if _, ok := c.state.contexts[contextID]; !ok {
		return "", errors.New("fake context is absent")
	}
	c.state.nextTab++
	id := "target-" + itoa(c.state.nextTab)
	if rawURL != "about:blank" && !stringsHasPrefix(rawURL, "about:blank#") {
		return "", errors.New("fake target must start blank")
	}
	c.state.targets[id] = targetInfo{TargetID: id, Type: "page", URL: rawURL, BrowserContext: contextID}
	return id, nil
}

func (c *fakeBrowserConnection) ActivateTarget(ctx context.Context, targetID string) error {
	if err := c.check(ctx); err != nil {
		return err
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	if _, ok := c.state.targets[targetID]; !ok {
		return errors.New("fake target is absent")
	}
	c.state.activeTarget = targetID
	return nil
}

func (c *fakeBrowserConnection) AttachTarget(ctx context.Context, targetID string) (targetConnection, error) {
	if err := c.check(ctx); err != nil {
		return nil, err
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	if _, ok := c.state.targets[targetID]; !ok {
		return nil, errors.New("fake target is absent")
	}
	return &fakeTargetConnection{connection: c, targetID: targetID}, nil
}

func (c *fakeBrowserConnection) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	c.state.mu.Lock()
	for contextID, owner := range c.state.contexts {
		if owner == c.id {
			deleteContext(c.state, contextID)
		}
	}
	c.state.mu.Unlock()
	return nil
}

func (c *fakeBrowserConnection) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return errors.New("fake connection is closed")
	}
	c.state.mu.Lock()
	available := c.state.available
	c.state.mu.Unlock()
	if !available {
		return errors.New("fake CDP disconnected")
	}
	return nil
}

type fakeTargetConnection struct {
	connection *fakeBrowserConnection
	targetID   string
}

func (t *fakeTargetConnection) Call(ctx context.Context, method string, params any, result any) error {
	if err := t.connection.check(ctx); err != nil {
		return err
	}
	t.connection.state.mu.Lock()
	target, exists := t.connection.state.targets[t.targetID]
	if !exists {
		t.connection.state.mu.Unlock()
		return errors.New("fake target is absent")
	}
	switch method {
	case "Page.enable":
		t.connection.state.mu.Unlock()
		return nil
	case "Page.navigate":
		args := params.(map[string]any)
		target.URL = args["url"].(string)
		target.Title = "Example page"
		t.connection.state.targets[t.targetID] = target
		t.connection.state.mu.Unlock()
		return nil
	case "Runtime.evaluate":
		args := params.(map[string]any)
		expression := args["expression"].(string)
		var value any
		if expression == "document.title" {
			value = target.Title
		} else if stringsHasPrefix(expression, "(async()=>{if(document.readyState") {
			observed := t.connection.state.observeText
			if strings.Contains(expression, "const value=node.value;") {
				observed = t.connection.state.observeValue
			}
			observation := map[string]any{"origin": t.connection.state.observeOrigin, "count": t.connection.state.observeCount}
			if t.connection.state.observeCount == 1 {
				observation["valid"] = true
				observation["value"] = observed
			}
			value = observation
		} else {
			value = map[string]any{"url": target.URL, "title": target.Title, "text": "fake page text"}
		}
		t.connection.state.mu.Unlock()
		return assignFakeResult(result, map[string]any{"result": map[string]any{"value": value}})
	case "Page.captureScreenshot":
		jpegData := append([]byte(nil), t.connection.state.screenshotData...)
		t.connection.state.mu.Unlock()
		return assignFakeResult(result, map[string]any{"data": base64.StdEncoding.EncodeToString(jpegData)})
	default:
		t.connection.state.mu.Unlock()
		return errors.New("unsupported fake target method: " + method)
	}
}

func (t *fakeTargetConnection) Close() error { return nil }

func assignFakeResult(target any, value any) error {
	if target == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func deleteContext(state *fakeBrowserState, contextID string) {
	delete(state.contexts, contextID)
	for targetID, target := range state.targets {
		if target.BrowserContext == contextID {
			delete(state.targets, targetID)
		}
	}
}

func itoa(value int) string {
	return fmt.Sprintf("%d", value)
}

func stringsHasPrefix(value, prefix string) bool {
	return len(value) >= len(prefix) && value[:len(prefix)] == prefix
}
