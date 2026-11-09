package runtimebrowser

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"morde-runtime/contract"
	"morde-runtime/internal/operationstore"
	"morde-runtime/runtimecore"
)

const (
	capabilityBrowserActions    = "browser.actions"
	capabilityBrowserObserve    = "browser.observe"
	maxBrowserTargetsPerContext = 256
	maxBrowserRoleCandidates    = 10000
	contextProbeInterval        = 50 * time.Millisecond
	contextStopTimeout          = 5 * time.Second
	browserStartupTimeout       = 30 * time.Second
	browserStartupRetry         = 200 * time.Millisecond
)

var ErrOperationLedgerUnavailable = errors.New("browser operation ledger is unavailable")

type Options struct {
	StoreDir         string
	ExpectedOwnerUID uint32
	Store            operationstore.EventStore
	Endpoint         string

	connector browserConnector
}

// Controller owns isolated browser contexts. Each task has a dedicated CDP
// control socket so disconnecting one task cannot dispose another task's pages.
type Controller struct {
	connector browserConnector
	registry  *registry

	mu       sync.Mutex
	closed   bool
	locks    map[string]chan struct{}
	active   map[string]chan struct{}
	controls map[string]browserConnection
}

type actionHandler struct {
	controller *Controller
	capability string
	action     string
}

var _ runtimecore.ActionHandler = (*actionHandler)(nil)

// NewHandlers opens the root-owned operation registry and proves that the CDP
// endpoint can create and dispose an isolated context without changing the
// existing browser profile. Capabilities are absent when that proof fails.
func NewHandlers(options Options) (*Controller, error) {
	if options.StoreDir == "" {
		return nil, errors.New("browser data directory is required")
	}
	connector := options.connector
	if connector == nil {
		var err error
		connector, err = newCDPConnector(options.Endpoint)
		if err != nil {
			return nil, err
		}
	}
	registry, err := openRegistry(options.StoreDir, options.ExpectedOwnerUID, options.Store)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOperationLedgerUnavailable, err)
	}
	controller := &Controller{
		connector: connector, registry: registry,
		locks: make(map[string]chan struct{}), active: make(map[string]chan struct{}), controls: make(map[string]browserConnection),
	}
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), browserStartupTimeout)
	probeConnection, err := connectUntilReady(startupCtx, connector, browserStartupRetry)
	cancelStartup()
	if err != nil {
		_ = registry.Close()
		return nil, errors.New("browser CDP did not become ready")
	}
	_ = probeConnection.Close()
	ctx, cancel := context.WithTimeout(context.Background(), cdpConnectTimeout*3)
	defer cancel()
	if err := controller.reconcilePreviousContexts(ctx); err != nil {
		_ = registry.Close()
		return nil, fmt.Errorf("previous browser operation could not be reconciled: %w", err)
	}
	if err := controller.healthProbe(ctx); err != nil {
		_ = registry.Close()
		return nil, errors.New("browser CDP safety probe failed")
	}
	return controller, nil
}

func (c *Controller) Handlers() []runtimecore.ActionHandler {
	if c == nil {
		return nil
	}
	definitions := [][2]string{
		{capabilityBrowserActions, "open_tab"},
		{capabilityBrowserActions, "navigate"},
		{capabilityBrowserActions, "handoff"},
		{capabilityBrowserObserve, "snapshot"},
		{capabilityBrowserObserve, "field"},
		{capabilityBrowserActions, "click"},
		{capabilityBrowserActions, "type"},
		{capabilityBrowserActions, "press_key"},
		{capabilityBrowserObserve, "screenshot"},
	}
	handlers := make([]runtimecore.ActionHandler, 0, len(definitions))
	for _, definition := range definitions {
		handlers = append(handlers, &actionHandler{controller: c, capability: definition[0], action: definition[1]})
	}
	return handlers
}

// Close releases each task's CDP control socket. Because every owned context
// uses disposeOnDetach=true, only those isolated contexts are disposed.
func (c *Controller) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	controls := make([]browserConnection, 0, len(c.controls))
	for _, control := range c.controls {
		controls = append(controls, control)
	}
	c.controls = make(map[string]browserConnection)
	c.mu.Unlock()
	var closeErr error
	for _, control := range controls {
		if err := control.Close(); closeErr == nil {
			closeErr = err
		}
	}
	if registryErr := c.registry.Close(); closeErr == nil {
		closeErr = registryErr
	}
	return closeErr
}

func (h *actionHandler) Capability() string { return h.capability }
func (h *actionHandler) Action() string     { return h.action }

func (h *actionHandler) Execute(ctx context.Context, request contract.ExecuteRequest) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if h == nil || h.controller == nil || request.Capability != h.capability || request.Action != h.action ||
		!validOperationID(request.OperationID) || !validTaskID(request.TaskID) {
		return nil, runtimecore.ErrInvalidRequest
	}
	return h.controller.execute(ctx, request, h.action)
}

func (h *actionHandler) Stop(ctx context.Context, operation contract.Operation) (runtimecore.StopResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if h == nil || h.controller == nil || operation.Capability != h.capability || operation.Action != h.action ||
		!validOperationID(operation.OperationID) || !validTaskID(operation.TaskID) {
		return runtimecore.StopResult{}, runtimecore.ErrInvalidRequest
	}
	return h.controller.stop(ctx, operation)
}

type openTabArguments struct {
	URL string `json:"url"`
}

type navigateArguments struct {
	TargetID string `json:"target_id"`
	URL      string `json:"url"`
}

type targetArguments struct {
	TargetID string `json:"target_id"`
}

type elementArguments struct {
	TargetID string `json:"target_id"`
	Role     string `json:"role"`
	Name     string `json:"name"`
}

type typeArguments struct {
	TargetID string `json:"target_id"`
	Role     string `json:"role"`
	Name     string `json:"name"`
	Text     string `json:"text"`
}

type pressKeyArguments struct {
	TargetID string `json:"target_id"`
	Key      string `json:"key"`
}

type fieldArguments struct {
	URL      string `json:"url"`
	Selector string `json:"selector"`
	Field    string `json:"field"`
}

func (c *Controller) execute(ctx context.Context, request contract.ExecuteRequest, action string) (json.RawMessage, error) {
	if !c.registry.healthy() {
		return nil, runtimecore.ErrRuntimeStateUnavailable
	}
	arguments, err := decodeArguments(request.Arguments, action)
	if err != nil {
		return nil, runtimecore.ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lock := c.taskLock(request.TaskID)
	activeDone, err := c.startActive(request.OperationID)
	if err != nil {
		return nil, err
	}
	defer c.finishActive(request.OperationID, activeDone)
	select {
	case lock <- struct{}{}:
		defer func() { <-lock }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if action == "open_tab" {
		return c.openTab(ctx, request, arguments.(openTabArguments))
	}
	if action == "field" {
		return c.observeField(ctx, request, arguments.(fieldArguments))
	}
	return c.runTargetAction(ctx, request, action, arguments)
}

func (c *Controller) taskLock(taskID string) chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	lock := c.locks[taskID]
	if lock == nil {
		lock = make(chan struct{}, 1)
		c.locks[taskID] = lock
	}
	return lock
}

func (c *Controller) startActive(operationID string) (chan struct{}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, runtimecore.ErrRuntimeStateUnavailable
	}
	if _, exists := c.active[operationID]; exists {
		return nil, runtimecore.ErrOperationConflict
	}
	done := make(chan struct{})
	c.active[operationID] = done
	return done, nil
}

func (c *Controller) finishActive(operationID string, done chan struct{}) {
	c.mu.Lock()
	if current := c.active[operationID]; current == done {
		delete(c.active, operationID)
		close(done)
	}
	c.mu.Unlock()
}

func decodeArguments(body []byte, action string) (any, error) {
	decode := func(target any) error {
		if len(body) == 0 || len(body) > 64<<10 {
			return errors.New("browser arguments are invalid")
		}
		decoder := json.NewDecoder(strings.NewReader(string(body)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(target); err != nil {
			return err
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return errors.New("browser arguments contain trailing JSON")
		}
		return nil
	}
	switch action {
	case "open_tab":
		var args openTabArguments
		if err := decode(&args); err != nil {
			return nil, err
		}
		if _, err := cleanURL(args.URL); err != nil {
			return nil, err
		}
		return args, nil
	case "navigate":
		var args navigateArguments
		if err := decode(&args); err != nil || !validTargetID(args.TargetID) {
			return nil, errors.New("browser navigation arguments are invalid")
		}
		if _, err := cleanURL(args.URL); err != nil {
			return nil, err
		}
		return args, nil
	case "snapshot", "screenshot", "handoff":
		var args targetArguments
		if err := decode(&args); err != nil || !validTargetID(args.TargetID) {
			return nil, errors.New("browser target arguments are invalid")
		}
		return args, nil
	case "field":
		var args fieldArguments
		if err := decode(&args); err != nil || !validCSSSelector(args.Selector) || args.Field != "text" && args.Field != "value" {
			return nil, errors.New("browser field arguments are invalid")
		}
		if _, err := cleanURL(args.URL); err != nil {
			return nil, err
		}
		return args, nil
	case "click":
		var args elementArguments
		if err := decode(&args); err != nil || !validTargetID(args.TargetID) || !validAccessibleRole(args.Role) || !validAccessibleName(args.Name) {
			return nil, errors.New("browser click arguments are invalid")
		}
		return args, nil
	case "type":
		var args typeArguments
		if err := decode(&args); err != nil || !validTargetID(args.TargetID) || !validEditableRole(args.Role) || !validAccessibleName(args.Name) || !validBrowserText(args.Text) {
			return nil, errors.New("browser type arguments are invalid")
		}
		return args, nil
	case "press_key":
		var args pressKeyArguments
		if err := decode(&args); err != nil || !validTargetID(args.TargetID) || !validKey(args.Key) {
			return nil, errors.New("browser key arguments are invalid")
		}
		return args, nil
	default:
		return nil, runtimecore.ErrCapabilityUnavailable
	}
}

func (c *Controller) observeField(ctx context.Context, request contract.ExecuteRequest, args fieldArguments) (result json.RawMessage, returnedErr error) {
	pageURL, err := cleanURL(args.URL)
	if err != nil || !validCSSSelector(args.Selector) || args.Field != "text" && args.Field != "value" {
		return nil, runtimecore.ErrInvalidRequest
	}
	marker, err := markerURL()
	if err != nil {
		return nil, runtimecore.ErrRuntimeStateUnavailable
	}
	if err := c.registry.begin(operationBinding{OperationID: request.OperationID, TaskID: request.TaskID, Action: "field", MarkerURL: marker}); err != nil {
		return nil, runtimecore.ErrOperationConflict
	}
	defer func() {
		if journalErr := c.registry.complete(request.OperationID); journalErr != nil && returnedErr == nil {
			result = nil
			returnedErr = runtimecore.ErrOperationUncertain
		}
	}()
	contextID, _, err := c.ensureTaskContext(ctx, request.TaskID, request.OperationID)
	if err != nil {
		return nil, err
	}
	control, ok := c.taskControl(request.TaskID)
	if !ok {
		return nil, runtimecore.ErrRuntimeStateUnavailable
	}
	ownedTargets, err := control.Targets(ctx)
	if err != nil {
		return nil, errors.New("owned browser context could not be inspected")
	}
	pageCount := 0
	for _, ownedTarget := range ownedTargets {
		if ownedTarget.Type == "page" && ownedTarget.BrowserContext == contextID {
			pageCount++
		}
	}
	if pageCount >= maxBrowserTargetsPerContext {
		return nil, errors.New("browser task has reached its tab limit")
	}
	targetID, err := control.CreateTarget(ctx, contextID, marker, true)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, runtimecore.ErrOperationUncertain
		}
		return nil, errors.New("browser observation page could not be created")
	}
	if err := c.registry.ownTarget(ownedTarget{TargetID: targetID, TaskID: request.TaskID, ContextID: contextID}); err != nil {
		return nil, runtimecore.ErrOperationUncertain
	}
	if err := c.registry.bindOperation(request.OperationID, contextID, targetID); err != nil {
		return nil, runtimecore.ErrOperationUncertain
	}
	connection, target, err := c.attachOwned(ctx, request.TaskID, targetID, contextID)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	if err := enablePage(ctx, target); err != nil {
		return nil, errors.New("browser observation page could not be prepared")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.registry.markEffectStarted(request.OperationID); err != nil {
		return nil, runtimecore.ErrOperationUncertain
	}
	navigation, err := navigatePage(ctx, target, pageURL)
	if err != nil || navigation.ErrorText != "" {
		return nil, runtimecore.ErrOperationUncertain
	}
	selectorJSON, err := json.Marshal(args.Selector)
	if err != nil {
		return nil, runtimecore.ErrInvalidRequest
	}
	readValue := `typeof node.innerText === "string" ? node.innerText : node.textContent`
	if args.Field == "value" {
		readValue = "node.value"
	}
	expression := fmt.Sprintf(`(async()=>{if(document.readyState!=="complete")await new Promise(resolve=>{window.addEventListener("load",resolve,{once:true});setTimeout(resolve,10000)});const nodes=document.querySelectorAll(%s);if(nodes.length!==1)return {origin:location.origin,count:nodes.length};const node=nodes[0];const value=%s;if(typeof value!=="string")return {origin:location.origin,count:1,valid:false};if(new TextEncoder().encode(value).length>%d)return {origin:location.origin,count:1,too_large:true};return {origin:location.origin,count:1,valid:true,value};})()`, selectorJSON, readValue, maxBrowserObservedValueBytes)
	encoded, err := evaluate(ctx, target, expression, true, true)
	if err != nil {
		return nil, errors.New("browser field observation failed")
	}
	var observation struct {
		Origin   string  `json:"origin"`
		Count    int     `json:"count"`
		Value    *string `json:"value"`
		Valid    bool    `json:"valid"`
		TooLarge bool    `json:"too_large"`
	}
	if err := json.Unmarshal(encoded, &observation); err != nil || observation.Count != 1 || observation.Value == nil || !observation.Valid || observation.TooLarge {
		return nil, errors.New("browser field observation failed")
	}
	originURL, err := url.Parse(observation.Origin)
	if err != nil || originURL == nil || (originURL.Scheme != "http" && originURL.Scheme != "https") || originURL.Host == "" || originURL.User != nil || originURL.Path != "" || originURL.RawQuery != "" || originURL.Fragment != "" {
		return nil, errors.New("browser field observation failed")
	}
	digest := sha256.Sum256([]byte(*observation.Value))
	return marshalResult(struct {
		Fingerprint string `json:"fingerprint"`
		Origin      string `json:"origin"`
		MatchCount  int    `json:"match_count"`
	}{Fingerprint: hex.EncodeToString(digest[:]), Origin: observation.Origin, MatchCount: observation.Count})
}

func validAccessibleName(value string) bool {
	return len(value) > 0 && len(value) <= maxBrowserNameBytes && utf8.ValidString(value) && strings.TrimSpace(value) == value
}

func validBrowserText(value string) bool {
	return utf8.ValidString(value) && len(value) <= maxBrowserTextBytes
}

func (c *Controller) openTab(ctx context.Context, request contract.ExecuteRequest, args openTabArguments) (result json.RawMessage, returnedErr error) {
	pageURL, err := cleanURL(args.URL)
	if err != nil {
		return nil, runtimecore.ErrInvalidRequest
	}
	marker, err := markerURL()
	if err != nil {
		return nil, runtimecore.ErrRuntimeStateUnavailable
	}
	binding := operationBinding{OperationID: request.OperationID, TaskID: request.TaskID, Action: "open_tab", MarkerURL: marker}
	if err := c.registry.begin(binding); err != nil {
		return nil, runtimecore.ErrOperationConflict
	}
	defer func() {
		if journalErr := c.registry.complete(request.OperationID); journalErr != nil && returnedErr == nil {
			result = nil
			returnedErr = runtimecore.ErrOperationUncertain
		}
	}()
	contextID, created, err := c.ensureTaskContext(ctx, request.TaskID, request.OperationID)
	if err != nil {
		return nil, err
	}
	cleanupFreshContext := func() bool {
		if !created {
			return true
		}
		return c.disposeTaskContext(context.Background(), request.TaskID, contextID) == nil
	}
	if err := ctx.Err(); err != nil {
		if cleanupFreshContext() {
			return nil, err
		}
		return nil, runtimecore.ErrOperationUncertain
	}
	if err := c.registry.bindOperation(request.OperationID, contextID, ""); err != nil {
		if cleanupFreshContext() {
			return nil, runtimecore.ErrRuntimeStateUnavailable
		}
		return nil, runtimecore.ErrOperationUncertain
	}
	control, ok := c.taskControl(request.TaskID)
	if !ok {
		if cleanupFreshContext() {
			return nil, runtimecore.ErrRuntimeStateUnavailable
		}
		return nil, runtimecore.ErrOperationUncertain
	}
	initialTargetURL := marker
	ownedTargets, err := control.Targets(ctx)
	if err != nil {
		return nil, errors.New("owned browser context could not be inspected")
	}
	pageCount := 0
	for _, ownedTarget := range ownedTargets {
		if ownedTarget.Type == "page" && ownedTarget.BrowserContext == contextID {
			pageCount++
		}
	}
	if pageCount >= maxBrowserTargetsPerContext {
		return nil, errors.New("browser task has reached its tab limit")
	}
	targetID, createErr := control.CreateTarget(ctx, contextID, initialTargetURL, true)
	if createErr != nil {
		if ctx.Err() != nil || errors.Is(createErr, context.Canceled) || errors.Is(createErr, context.DeadlineExceeded) {
			return nil, runtimecore.ErrOperationUncertain
		}
		if cleanupFreshContext() {
			return nil, errors.New("browser tab could not be created")
		}
		return nil, runtimecore.ErrOperationUncertain
	}
	if err := c.registry.ownTarget(ownedTarget{TargetID: targetID, TaskID: request.TaskID, ContextID: contextID}); err != nil {
		if cleanupFreshContext() {
			return nil, runtimecore.ErrRuntimeStateUnavailable
		}
		return nil, runtimecore.ErrOperationUncertain
	}
	if err := c.registry.bindOperation(request.OperationID, contextID, targetID); err != nil {
		if cleanupFreshContext() {
			return nil, runtimecore.ErrRuntimeStateUnavailable
		}
		return nil, runtimecore.ErrOperationUncertain
	}
	connection, target, err := c.attachOwned(ctx, request.TaskID, targetID, contextID)
	if err != nil {
		if ctx.Err() != nil {
			return nil, runtimecore.ErrOperationUncertain
		}
		if cleanupFreshContext() {
			return nil, errors.New("browser tab could not be attached")
		}
		return nil, runtimecore.ErrOperationUncertain
	}
	defer connection.Close()
	if err := enablePage(ctx, target); err != nil {
		return nil, errors.New("browser page could not be prepared")
	}
	if err := ctx.Err(); err != nil {
		return nil, runtimecore.ErrOperationUncertain
	}
	if err := c.registry.markEffectStarted(request.OperationID); err != nil {
		return nil, runtimecore.ErrOperationUncertain
	}
	navigation, err := navigatePage(ctx, target, pageURL)
	if err != nil || navigation.ErrorText != "" {
		return nil, runtimecore.ErrOperationUncertain
	}
	title := pageTitle(ctx, target)
	return marshalResult(struct {
		TargetID string `json:"target_id"`
		URL      string `json:"url"`
		Title    string `json:"title"`
	}{TargetID: targetID, URL: pageURL, Title: title})
}

func (c *Controller) runTargetAction(ctx context.Context, request contract.ExecuteRequest, action string, decoded any) (result json.RawMessage, returnedErr error) {
	var targetID string
	switch args := decoded.(type) {
	case navigateArguments:
		targetID = args.TargetID
	case targetArguments:
		targetID = args.TargetID
	case elementArguments:
		targetID = args.TargetID
	case typeArguments:
		targetID = args.TargetID
	case pressKeyArguments:
		targetID = args.TargetID
	default:
		return nil, runtimecore.ErrInvalidRequest
	}
	target, ok := c.registry.target(request.TaskID, targetID)
	if !ok {
		return nil, runtimecore.ErrFenceRejected
	}
	contextValue, ok := c.registry.taskContext(request.TaskID)
	if !ok || contextValue.ContextID != target.ContextID {
		return nil, runtimecore.ErrFenceRejected
	}
	if err := c.registry.begin(operationBinding{
		OperationID: request.OperationID, TaskID: request.TaskID, Action: action,
		ContextID: target.ContextID, TargetID: targetID,
	}); err != nil {
		return nil, runtimecore.ErrOperationConflict
	}
	defer func() {
		if journalErr := c.registry.complete(request.OperationID); journalErr != nil && returnedErr == nil {
			result = nil
			returnedErr = runtimecore.ErrOperationUncertain
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	connection, attached, err := c.attachOwned(ctx, request.TaskID, targetID, target.ContextID)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	switch action {
	case "navigate":
		args := decoded.(navigateArguments)
		pageURL, _ := cleanURL(args.URL)
		if err := enablePage(ctx, attached); err != nil {
			return nil, errors.New("browser page is unavailable")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := c.registry.markEffectStarted(request.OperationID); err != nil {
			return nil, runtimecore.ErrOperationUncertain
		}
		navigation, err := navigatePage(ctx, attached, pageURL)
		if err != nil || navigation.ErrorText != "" {
			return nil, runtimecore.ErrOperationUncertain
		}
		return marshalResult(struct {
			TargetID string `json:"target_id"`
			URL      string `json:"url"`
			Title    string `json:"title"`
		}{TargetID: targetID, URL: pageURL, Title: pageTitle(ctx, attached)})
	case "snapshot":
		value, err := evaluate(ctx, attached, "({url:location.href,title:document.title,text:(document.body&&document.body.innerText||'').slice(0,65536)})", true, false)
		if err != nil {
			return nil, errors.New("browser page snapshot failed")
		}
		var snapshot struct {
			URL   string `json:"url"`
			Title string `json:"title"`
			Text  string `json:"text"`
		}
		if err := json.Unmarshal(value, &snapshot); err != nil {
			return nil, errors.New("browser page snapshot was invalid")
		}
		snapshot.URL = boundedString(snapshot.URL, maxBrowserURLBytes)
		snapshot.Title = boundedString(snapshot.Title, maxBrowserNameBytes)
		snapshot.Text = boundedString(snapshot.Text, maxSnapshotTextBytes)
		return marshalResult(struct {
			TargetID string `json:"target_id"`
			URL      string `json:"url"`
			Title    string `json:"title"`
			Text     string `json:"text"`
		}{TargetID: targetID, URL: snapshot.URL, Title: snapshot.Title, Text: snapshot.Text})
	case "screenshot":
		return screenshot(ctx, attached, targetID)
	case "click":
		args := decoded.(elementArguments)
		if err := clickElement(ctx, attached, args.Role, args.Name, func() error { return c.registry.markEffectStarted(request.OperationID) }); err != nil {
			return nil, err
		}
		return marshalResult(struct {
			TargetID string `json:"target_id"`
			Matched  bool   `json:"matched"`
		}{TargetID: targetID, Matched: true})
	case "type":
		args := decoded.(typeArguments)
		if err := typeIntoElement(ctx, attached, args.Role, args.Name, args.Text, func() error { return c.registry.markEffectStarted(request.OperationID) }); err != nil {
			return nil, err
		}
		return marshalResult(struct {
			TargetID   string `json:"target_id"`
			TypedChars int    `json:"typed_chars"`
		}{TargetID: targetID, TypedChars: utf8.RuneCountInString(args.Text)})
	case "press_key":
		args := decoded.(pressKeyArguments)
		if err := pressKey(ctx, attached, args.Key, func() error { return c.registry.markEffectStarted(request.OperationID) }); err != nil {
			return nil, err
		}
		return marshalResult(struct {
			TargetID   string `json:"target_id"`
			Dispatched bool   `json:"dispatched"`
		}{TargetID: targetID, Dispatched: true})
	case "handoff":
		if err := c.registry.markEffectStarted(request.OperationID); err != nil {
			return nil, runtimecore.ErrOperationUncertain
		}
		if err := connection.ActivateTarget(ctx, targetID); err != nil {
			if ctx.Err() != nil {
				return nil, runtimecore.ErrOperationUncertain
			}
			return nil, errors.New("browser tab could not be brought to the foreground")
		}
		return marshalResult(struct {
			TargetID string `json:"target_id"`
			Active   bool   `json:"active"`
		}{TargetID: targetID, Active: true})
	default:
		return nil, runtimecore.ErrCapabilityUnavailable
	}
}

func (c *Controller) attachOwned(ctx context.Context, taskID, targetID, contextID string) (browserConnection, targetConnection, error) {
	if owned, ok := c.registry.target(taskID, targetID); !ok || owned.ContextID != contextID {
		return nil, nil, runtimecore.ErrFenceRejected
	}
	connection, err := c.connector.Connect(ctx)
	if err != nil {
		return nil, nil, errors.New("browser CDP is unavailable")
	}
	contexts, err := connection.BrowserContexts(ctx)
	if err != nil || !containsString(contexts, contextID) {
		_ = connection.Close()
		return nil, nil, errors.New("owned browser context is unavailable")
	}
	targets, err := connection.Targets(ctx)
	if err != nil || !targetInContext(targets, targetID, contextID) {
		_ = connection.Close()
		return nil, nil, runtimecore.ErrFenceRejected
	}
	target, err := connection.AttachTarget(ctx, targetID)
	if err != nil {
		_ = connection.Close()
		return nil, nil, errors.New("browser target is unavailable")
	}
	return connection, target, nil
}

func (c *Controller) ensureTaskContext(ctx context.Context, taskID, operationID string) (string, bool, error) {
	if existing, ok := c.registry.taskContext(taskID); ok {
		if control, controlExists := c.taskControl(taskID); controlExists {
			contexts, err := control.BrowserContexts(ctx)
			if err == nil && containsString(contexts, existing.ContextID) {
				return existing.ContextID, false, nil
			}
		}
		if err := c.disposeTaskContext(ctx, taskID, existing.ContextID); err != nil {
			return "", false, runtimecore.ErrOperationUncertain
		}
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	control, err := c.connector.Connect(ctx)
	if err != nil {
		return "", false, runtimecore.ErrRuntimeStateUnavailable
	}
	contextID, err := control.CreateBrowserContext(ctx, true)
	if err != nil {
		_ = control.Close()
		if ctx.Err() != nil {
			return "", false, runtimecore.ErrOperationUncertain
		}
		return "", false, errors.New("isolated browser context could not be created")
	}
	if err := c.setTaskControl(taskID, control); err != nil {
		_ = control.Close()
		return "", false, runtimecore.ErrRuntimeStateUnavailable
	}
	if err := c.registry.bindOperation(operationID, contextID, ""); err != nil {
		if c.disposeUnregisteredContext(context.Background(), contextID) == nil {
			c.closeTaskControl(taskID)
			return "", false, runtimecore.ErrRuntimeStateUnavailable
		}
		return "", false, runtimecore.ErrOperationUncertain
	}
	if err := c.registry.setTaskContext(taskID, contextID); err != nil {
		if c.disposeTaskContext(context.Background(), taskID, contextID) == nil {
			return "", false, runtimecore.ErrRuntimeStateUnavailable
		}
		return "", false, runtimecore.ErrOperationUncertain
	}
	return contextID, true, nil
}

func (c *Controller) taskControl(taskID string) (browserConnection, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	control, ok := c.controls[taskID]
	return control, ok && !c.closed
}

func (c *Controller) setTaskControl(taskID string, control browserConnection) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.controls[taskID] != nil {
		return runtimecore.ErrRuntimeStateUnavailable
	}
	c.controls[taskID] = control
	return nil
}

func (c *Controller) closeTaskControl(taskID string) {
	c.mu.Lock()
	control := c.controls[taskID]
	delete(c.controls, taskID)
	c.mu.Unlock()
	if control != nil {
		_ = control.Close()
	}
}

func (c *Controller) stop(ctx context.Context, operation contract.Operation) (runtimecore.StopResult, error) {
	binding, ok := c.registry.operation(operation.OperationID)
	if !ok || binding.TaskID != operation.TaskID || binding.Action != operation.Action {
		c.mu.Lock()
		done := c.active[operation.OperationID]
		c.mu.Unlock()
		if done == nil {
			return runtimecore.StopResult{VerifiedStopped: true}, nil
		}
		select {
		case <-done:
			binding, ok = c.registry.operation(operation.OperationID)
			if !ok {
				return runtimecore.StopResult{VerifiedStopped: true}, nil
			}
			if binding.TaskID != operation.TaskID || binding.Action != operation.Action {
				return runtimecore.StopResult{VerifiedStopped: false}, nil
			}
		case <-ctx.Done():
			return runtimecore.StopResult{VerifiedStopped: false, PendingIDs: []string{operation.OperationID}}, nil
		}
	}
	if binding.ContextID == "" {
		// open_tab may have created a context whose ID was lost before it could
		// be journaled. Never guess among unrelated contexts.
		if binding.Action == "open_tab" && binding.MarkerURL != "" {
			return runtimecore.StopResult{VerifiedStopped: false}, nil
		}
		return runtimecore.StopResult{VerifiedStopped: true}, nil
	}
	stopCtx, cancel := context.WithTimeout(ctx, contextStopTimeout)
	defer cancel()
	if err := c.disposeTaskContext(stopCtx, binding.TaskID, binding.ContextID); err != nil {
		return runtimecore.StopResult{VerifiedStopped: false, PendingIDs: []string{binding.ContextID}}, nil
	}
	c.mu.Lock()
	done := c.active[operation.OperationID]
	c.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-stopCtx.Done():
			return runtimecore.StopResult{VerifiedStopped: false, PendingIDs: []string{operation.OperationID}}, nil
		}
	}
	if latest, found := c.registry.operation(operation.OperationID); found {
		binding = latest
	} else {
		return runtimecore.StopResult{VerifiedStopped: false}, nil
	}
	if binding.EffectMayHaveRun {
		// Context disposal stops future work but cannot undo an input or
		// navigation that the page may already have accepted.
		return runtimecore.StopResult{VerifiedStopped: false}, nil
	}
	return runtimecore.StopResult{VerifiedStopped: true}, nil
}

func (c *Controller) disposeTaskContext(ctx context.Context, taskID, contextID string) error {
	if !validContextID(contextID) {
		return errors.New("browser context ID is invalid")
	}
	if taskID != "" && !c.registry.ownsContext(taskID, contextID) {
		return errors.New("browser context is not owned by this task")
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, contextStopTimeout)
		defer cancel()
	}
	connection, err := c.connector.Connect(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	contexts, err := connection.BrowserContexts(ctx)
	if err != nil {
		return err
	}
	if containsString(contexts, contextID) {
		if err := connection.DisposeBrowserContext(ctx, contextID); err != nil {
			return err
		}
	}
	if err := waitContextGone(ctx, connection, contextID); err != nil {
		return err
	}
	if taskID != "" {
		if err := c.registry.closeTaskContext(taskID, contextID); err != nil {
			return err
		}
		c.closeTaskControl(taskID)
	}
	return nil
}

func (c *Controller) disposeUnregisteredContext(ctx context.Context, contextID string) error {
	connection, err := c.connector.Connect(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	if err := connection.DisposeBrowserContext(ctx, contextID); err != nil {
		return err
	}
	return waitContextGone(ctx, connection, contextID)
}

func waitContextGone(ctx context.Context, connection browserConnection, contextID string) error {
	ticker := time.NewTicker(contextProbeInterval)
	defer ticker.Stop()
	for {
		contexts, err := connection.BrowserContexts(ctx)
		if err != nil {
			return err
		}
		targets, err := connection.Targets(ctx)
		if err != nil {
			return err
		}
		if !containsString(contexts, contextID) && !anyTargetInContext(targets, contextID) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *Controller) reconcilePreviousContexts(ctx context.Context) error {
	connection, err := c.connector.Connect(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	contexts, err := connection.BrowserContexts(ctx)
	if err != nil {
		return err
	}
	for _, owned := range c.registry.contexts() {
		if containsString(contexts, owned.ContextID) {
			if err := connection.DisposeBrowserContext(ctx, owned.ContextID); err != nil {
				return err
			}
			if err := waitContextGone(ctx, connection, owned.ContextID); err != nil {
				return err
			}
		}
		if err := c.registry.closeTaskContext(owned.TaskID, owned.ContextID); err != nil {
			return err
		}
	}
	contexts, err = connection.BrowserContexts(ctx)
	if err != nil {
		return err
	}
	return c.registry.reconcileContexts(contexts)
}

func (c *Controller) healthProbe(ctx context.Context) error {
	if !c.registry.healthy() {
		return errors.New("browser controller is unavailable")
	}
	connection, err := c.connector.Connect(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	if _, err := connection.Version(ctx); err != nil {
		return err
	}
	before, err := connection.Targets(ctx)
	if err != nil {
		return err
	}
	contextID, err := connection.CreateBrowserContext(ctx, true)
	if err != nil {
		return err
	}
	contexts, err := connection.BrowserContexts(ctx)
	if err != nil || !containsString(contexts, contextID) {
		return errors.New("isolated browser context could not be listed")
	}
	targetID, err := connection.CreateTarget(ctx, contextID, "about:blank", true)
	if err != nil {
		return err
	}
	targets, err := connection.Targets(ctx)
	if err != nil || !targetInContext(targets, targetID, contextID) {
		return errors.New("isolated browser target could not be verified")
	}
	if err := connection.DisposeBrowserContext(ctx, contextID); err != nil {
		return err
	}
	if err := waitContextGone(ctx, connection, contextID); err != nil {
		return err
	}
	after, err := connection.Targets(ctx)
	if err != nil {
		return err
	}
	for _, target := range before {
		if !containsTargetID(after, target.TargetID) {
			return errors.New("browser safety probe changed a pre-existing target")
		}
	}
	return nil
}

func markerURL() (string, error) {
	var marker [16]byte
	if _, err := rand.Read(marker[:]); err != nil {
		return "", err
	}
	return "about:blank#morde-" + hex.EncodeToString(marker[:]), nil
}

func targetInContext(targets []targetInfo, targetID, contextID string) bool {
	for _, target := range targets {
		if target.TargetID == targetID && target.Type == "page" && target.BrowserContext == contextID {
			return true
		}
	}
	return false
}

func anyTargetInContext(targets []targetInfo, contextID string) bool {
	for _, target := range targets {
		if target.BrowserContext == contextID {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsTargetID(values []targetInfo, want string) bool {
	for _, value := range values {
		if value.TargetID == want {
			return true
		}
	}
	return false
}

func enablePage(ctx context.Context, target targetConnection) error {
	return target.Call(ctx, "Page.enable", map[string]any{}, nil)
}

type navigateResult struct {
	ErrorText string `json:"errorText"`
}

func navigatePage(ctx context.Context, target targetConnection, pageURL string) (navigateResult, error) {
	var result navigateResult
	err := target.Call(ctx, "Page.navigate", map[string]any{"url": pageURL}, &result)
	return result, err
}

func pageTitle(ctx context.Context, target targetConnection) string {
	value, err := evaluate(ctx, target, "document.title", true, false)
	if err != nil {
		return ""
	}
	var title string
	if json.Unmarshal(value, &title) != nil {
		return ""
	}
	return boundedString(title, maxBrowserNameBytes)
}

func evaluate(ctx context.Context, target targetConnection, expression string, returnByValue, awaitPromise bool) (json.RawMessage, error) {
	var response struct {
		Result struct {
			Value     json.RawMessage `json:"value"`
			ObjectID  string          `json:"objectId"`
			Exception json.RawMessage `json:"exceptionDetails"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := target.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression": expression, "returnByValue": returnByValue, "awaitPromise": awaitPromise, "userGesture": false,
	}, &response); err != nil {
		return nil, err
	}
	if len(response.ExceptionDetails) > 0 && string(response.ExceptionDetails) != "null" || len(response.Result.Exception) > 0 && string(response.Result.Exception) != "null" {
		return nil, errors.New("browser evaluation failed")
	}
	if returnByValue && len(response.Result.Value) == 0 {
		return nil, errors.New("browser evaluation returned no value")
	}
	return response.Result.Value, nil
}

func screenshot(ctx context.Context, target targetConnection, targetID string) (json.RawMessage, error) {
	var response struct {
		Data string `json:"data"`
	}
	if err := target.Call(ctx, "Page.captureScreenshot", map[string]any{
		"format": "jpeg", "quality": 55, "fromSurface": true, "captureBeyondViewport": false,
	}, &response); err != nil {
		return nil, errors.New("browser screenshot failed")
	}
	if len(response.Data) > base64.StdEncoding.EncodedLen(maxBrowserScreenshotInputBytes) {
		return nil, runtimecore.ErrResultTooLarge
	}
	data, err := base64.StdEncoding.DecodeString(response.Data)
	if err != nil {
		return nil, runtimecore.ErrResultTooLarge
	}
	data, err = normalizeBrowserScreenshot(data)
	if err != nil {
		return nil, err
	}
	dataBase64 := base64.StdEncoding.EncodeToString(data)
	encoded, err := marshalResult(struct {
		TargetID   string `json:"target_id"`
		MIMEType   string `json:"mime_type"`
		DataBase64 string `json:"data_base64"`
	}{TargetID: targetID, MIMEType: "image/jpeg", DataBase64: dataBase64})
	if err != nil || len(encoded) > 700<<10 {
		return nil, runtimecore.ErrResultTooLarge
	}
	return encoded, nil
}

func boundedString(value string, max int) string {
	if len(value) <= max {
		return value
	}
	value = value[:max]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func accessibleExpression(role, name string) (string, error) {
	roleJSON, err := json.Marshal(role)
	if err != nil {
		return "", err
	}
	nameJSON, err := json.Marshal(name)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`(()=>{
const wantedRole=%s,wantedName=%s;
const nodes=Array.from(document.querySelectorAll('button,a,input,textarea,select,[role],[tabindex]'));
if(nodes.length>%d)return {count:-1};
const normalize=s=>(s||'').replace(/\s+/g,' ').trim();
const roleOf=e=>{const explicit=e.getAttribute('role');if(explicit)return explicit.trim().split(/\s+/)[0];const tag=e.tagName.toLowerCase();if(tag==='button')return 'button';if(tag==='a'&&e.hasAttribute('href'))return 'link';if(tag==='textarea')return 'textbox';if(tag==='select')return e.multiple?'listbox':'combobox';if(tag==='input'){const type=(e.type||'text').toLowerCase();if(type==='button'||type==='submit'||type==='reset'||type==='image')return 'button';if(type==='checkbox')return 'checkbox';if(type==='radio')return 'radio';if(type==='search')return 'searchbox';if(!['hidden','file','range','color'].includes(type))return 'textbox';}return '';};
const nameOf=e=>{let value=e.getAttribute('aria-label');if(!value){const ids=(e.getAttribute('aria-labelledby')||'').trim().split(/\s+/).filter(Boolean);if(ids.length)value=ids.map(id=>document.getElementById(id)?.innerText||'').join(' ');}if(!value&&e.labels&&e.labels.length)value=Array.from(e.labels).map(x=>x.innerText||x.textContent||'').join(' ');if(!value)value=e.innerText||e.textContent||'';if(!value)value=e.getAttribute('placeholder')||e.getAttribute('alt')||e.getAttribute('title')||e.value||'';return normalize(value).slice(0,1024);};
const visible=e=>{if(e.hidden||e.getAttribute('aria-hidden')==='true')return false;const s=getComputedStyle(e),r=e.getBoundingClientRect();return s.display!=='none'&&s.visibility!=='hidden'&&Number(s.opacity)!==0&&r.width>0&&r.height>0;};
const matches=nodes.filter(e=>roleOf(e)===wantedRole&&nameOf(e)===wantedName&&visible(e));
return {count:matches.length,element:matches.length===1?matches[0]:null};
})()`, string(roleJSON), string(nameJSON), maxBrowserRoleCandidates), nil
}

type remoteObject struct {
	Type     string          `json:"type"`
	Value    json.RawMessage `json:"value"`
	ObjectID string          `json:"objectId"`
}

type propertyDescriptor struct {
	Name  string       `json:"name"`
	Value remoteObject `json:"value"`
}

func resolveAccessibleElement(ctx context.Context, target targetConnection, role, name string) (string, int, error) {
	expression, err := accessibleExpression(role, name)
	if err != nil {
		return "", 0, err
	}
	var evaluated struct {
		Result remoteObject `json:"result"`
	}
	if err := target.Call(ctx, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": false}, &evaluated); err != nil || evaluated.Result.ObjectID == "" {
		return "", 0, errors.New("browser element query failed")
	}
	objectID := evaluated.Result.ObjectID
	defer func() {
		_ = target.Call(context.Background(), "Runtime.releaseObject", map[string]any{"objectId": objectID}, nil)
	}()
	var properties struct {
		Result []propertyDescriptor `json:"result"`
	}
	if err := target.Call(ctx, "Runtime.getProperties", map[string]any{"objectId": objectID, "ownProperties": true}, &properties); err != nil {
		return "", 0, errors.New("browser element query failed")
	}
	count, elementID := -1, ""
	for _, property := range properties.Result {
		switch property.Name {
		case "count":
			_ = json.Unmarshal(property.Value.Value, &count)
		case "element":
			elementID = property.Value.ObjectID
		}
	}
	if count != 1 || elementID == "" {
		return "", count, errors.New("browser element match is not unique")
	}
	return elementID, count, nil
}

func clickElement(ctx context.Context, target targetConnection, role, name string, beforeDispatch func() error) error {
	objectID, _, err := resolveAccessibleElement(ctx, target, role, name)
	if err != nil {
		return err
	}
	defer func() {
		_ = target.Call(context.Background(), "Runtime.releaseObject", map[string]any{"objectId": objectID}, nil)
	}()
	if err := target.Call(ctx, "Runtime.callFunctionOn", map[string]any{
		"objectId": objectID, "functionDeclaration": "function(){this.scrollIntoView({block:'center',inline:'center'});return true}", "returnByValue": true,
	}, nil); err != nil {
		return errors.New("browser element could not be prepared")
	}
	var node struct {
		NodeID int64 `json:"nodeId"`
	}
	if err := target.Call(ctx, "DOM.requestNode", map[string]any{"objectId": objectID}, &node); err != nil || node.NodeID <= 0 {
		return errors.New("browser element could not be prepared")
	}
	var model struct {
		Model struct {
			Content []float64 `json:"content"`
		} `json:"model"`
	}
	if err := target.Call(ctx, "DOM.getBoxModel", map[string]any{"nodeId": node.NodeID}, &model); err != nil || len(model.Model.Content) < 8 {
		return errors.New("browser element is not visible")
	}
	x, y := 0.0, 0.0
	for index := 0; index < 8; index += 2 {
		x += model.Model.Content[index]
		y += model.Model.Content[index+1]
	}
	x, y = x/4, y/4
	if math.IsNaN(x) || math.IsNaN(y) || math.IsInf(x, 0) || math.IsInf(y, 0) || x < 0 || y < 0 {
		return errors.New("browser element coordinates are invalid")
	}
	if err := beforeDispatch(); err != nil {
		return runtimecore.ErrOperationUncertain
	}
	if err := target.Call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mouseMoved", "x": x, "y": y}, nil); err != nil {
		return errors.New("browser click could not be dispatched")
	}
	if err := target.Call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mousePressed", "x": x, "y": y, "button": "left", "clickCount": 1}, nil); err != nil {
		return runtimecore.ErrOperationUncertain
	}
	if err := target.Call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mouseReleased", "x": x, "y": y, "button": "left", "clickCount": 1}, nil); err != nil {
		return runtimecore.ErrOperationUncertain
	}
	return nil
}

func typeIntoElement(ctx context.Context, target targetConnection, role, name, text string, beforeDispatch func() error) error {
	objectID, _, err := resolveAccessibleElement(ctx, target, role, name)
	if err != nil {
		return err
	}
	defer func() {
		_ = target.Call(context.Background(), "Runtime.releaseObject", map[string]any{"objectId": objectID}, nil)
	}()
	var node struct {
		NodeID int64 `json:"nodeId"`
	}
	if err := target.Call(ctx, "DOM.requestNode", map[string]any{"objectId": objectID}, &node); err != nil || node.NodeID <= 0 {
		return errors.New("browser textbox could not be prepared")
	}
	if err := target.Call(ctx, "DOM.focus", map[string]any{"nodeId": node.NodeID}, nil); err != nil {
		return errors.New("browser textbox could not be focused")
	}
	if err := ctx.Err(); err != nil {
		return runtimecore.ErrOperationUncertain
	}
	if err := beforeDispatch(); err != nil {
		return runtimecore.ErrOperationUncertain
	}
	if err := target.Call(ctx, "Input.insertText", map[string]any{"text": text}, nil); err != nil {
		return runtimecore.ErrOperationUncertain
	}
	return nil
}

func pressKey(ctx context.Context, target targetConnection, key string, beforeDispatch func() error) error {
	keyInfo := map[string]struct {
		code string
		vk   int
		text string
	}{
		"Enter": {"Enter", 13, "\r"}, "Tab": {"Tab", 9, "\t"}, "Escape": {"Escape", 27, ""},
		"Backspace": {"Backspace", 8, ""}, "Delete": {"Delete", 46, ""}, "ArrowUp": {"ArrowUp", 38, ""},
		"ArrowDown": {"ArrowDown", 40, ""}, "ArrowLeft": {"ArrowLeft", 37, ""}, "ArrowRight": {"ArrowRight", 39, ""},
		"Home": {"Home", 36, ""}, "End": {"End", 35, ""}, "PageUp": {"PageUp", 33, ""}, "PageDown": {"PageDown", 34, ""}, "Space": {"Space", 32, " "},
	}
	info, ok := keyInfo[key]
	if !ok {
		return runtimecore.ErrInvalidRequest
	}
	if err := beforeDispatch(); err != nil {
		return runtimecore.ErrOperationUncertain
	}
	if err := target.Call(ctx, "Input.dispatchKeyEvent", map[string]any{
		"type": "keyDown", "key": key, "code": info.code, "windowsVirtualKeyCode": info.vk, "nativeVirtualKeyCode": info.vk, "text": info.text,
	}, nil); err != nil {
		return errors.New("browser key could not be dispatched")
	}
	if err := target.Call(ctx, "Input.dispatchKeyEvent", map[string]any{
		"type": "keyUp", "key": key, "code": info.code, "windowsVirtualKeyCode": info.vk, "nativeVirtualKeyCode": info.vk,
	}, nil); err != nil {
		return runtimecore.ErrOperationUncertain
	}
	return nil
}

func marshalResult(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > runtimecore.MaxOperationResultBytes {
		return nil, runtimecore.ErrResultTooLarge
	}
	return json.RawMessage(encoded), nil
}
