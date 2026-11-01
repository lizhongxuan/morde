// Package runtimebrowser provides bounded browser actions through an owned
// CDP context. The registry is kept in the root-only runtime data volume so a
// restarted runtime can stop, rather than replay, an unresolved action.
package runtimebrowser

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"morde-runtime/internal/operationstore"
)

const (
	browserRegistryDir       = "browser"
	maxBrowserOperations     = 1 << 16
	maxBrowserOwnedTargets   = 4096
	maxBrowserTaskContexts   = 4096
	maxBrowserOperationBytes = 16 << 20
)

var (
	errRegistryConflict = errors.New("browser operation registry conflict")
	errRegistryLimit    = errors.New("browser operation registry limit reached")
)

type taskContext struct {
	TaskID    string `json:"task_id"`
	ContextID string `json:"context_id"`
}

type ownedTarget struct {
	TargetID  string `json:"target_id"`
	TaskID    string `json:"task_id"`
	ContextID string `json:"context_id"`
}

type operationBinding struct {
	OperationID      string `json:"operation_id"`
	TaskID           string `json:"task_id"`
	Action           string `json:"action"`
	ContextID        string `json:"context_id,omitempty"`
	TargetID         string `json:"target_id,omitempty"`
	MarkerURL        string `json:"marker_url,omitempty"`
	EffectMayHaveRun bool   `json:"effect_may_have_run,omitempty"`
	Completed        bool   `json:"completed,omitempty"`
}

type registry struct {
	mu         sync.Mutex
	store      *operationstore.Store
	tasks      map[string]taskContext
	targets    map[string]ownedTarget
	operations map[string]operationBinding
	closed     bool
}

func openRegistry(storeDir string, expectedUID uint32) (*registry, error) {
	if !filepath.IsAbs(storeDir) || filepath.Clean(storeDir) != storeDir {
		return nil, errors.New("browser registry path is invalid")
	}
	parent, err := os.Lstat(storeDir)
	if err != nil || parent.Mode()&os.ModeSymlink != 0 || !parent.IsDir() || parent.Mode().Perm() != 0o700 {
		return nil, errors.New("runtime data directory is not private")
	}
	if err := ownedByUID(parent, expectedUID); err != nil {
		return nil, err
	}
	dir := filepath.Join(storeDir, browserRegistryDir)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create browser registry directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, errors.New("browser registry directory is not private")
	}
	if err := ownedByUID(info, expectedUID); err != nil {
		return nil, err
	}
	store, err := operationstore.Open(dir, expectedUID)
	if err != nil {
		return nil, fmt.Errorf("open browser registry: %w", err)
	}
	r := &registry{
		store: store, tasks: make(map[string]taskContext), targets: make(map[string]ownedTarget),
		operations: make(map[string]operationBinding),
	}
	events, err := store.Events()
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("read browser registry: %w", err)
	}
	for _, event := range events {
		if err := r.apply(event.Kind, event.Data); err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("replay browser registry: %w", err)
		}
	}
	return r, nil
}

func ownedByUID(info os.FileInfo, expected uint32) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != expected {
		return errors.New("browser registry owner is invalid")
	}
	return nil
}

func (r *registry) begin(binding operationBinding) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !validOperationID(binding.OperationID) || !validTaskID(binding.TaskID) || !validBrowserAction(binding.Action) ||
		binding.ContextID != "" && !validContextID(binding.ContextID) || binding.TargetID != "" && !validTargetID(binding.TargetID) ||
		binding.MarkerURL != "" && !validMarkerURL(binding.MarkerURL) {
		return errors.New("browser operation binding is invalid")
	}
	if _, ok := r.operations[binding.OperationID]; ok {
		// Browser actions are never replayed from this registry. A retry must be
		// reconciled by the runtime core, which returns its original operation.
		return errRegistryConflict
	}
	if len(r.operations) >= maxBrowserOperations {
		return errRegistryLimit
	}
	if err := r.append("operation", binding); err != nil {
		return err
	}
	r.operations[binding.OperationID] = binding
	return nil
}

func (r *registry) setTaskContext(taskID, contextID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !validTaskID(taskID) || !validContextID(contextID) {
		return errors.New("browser task context is invalid")
	}
	if current, ok := r.tasks[taskID]; ok {
		if current.ContextID == contextID {
			return nil
		}
		return errRegistryConflict
	}
	if len(r.tasks) >= maxBrowserTaskContexts {
		return errRegistryLimit
	}
	if err := r.append("task_context", taskContext{TaskID: taskID, ContextID: contextID}); err != nil {
		return err
	}
	r.tasks[taskID] = taskContext{TaskID: taskID, ContextID: contextID}
	return nil
}

func (r *registry) bindOperation(operationID, contextID, targetID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !validOperationID(operationID) || !validContextID(contextID) || targetID != "" && !validTargetID(targetID) {
		return errors.New("browser operation target is invalid")
	}
	binding, ok := r.operations[operationID]
	if !ok {
		return errRegistryConflict
	}
	if binding.ContextID != "" && binding.ContextID != contextID || binding.TargetID != "" && binding.TargetID != targetID {
		return errRegistryConflict
	}
	if binding.ContextID == contextID && binding.TargetID == targetID {
		return nil
	}
	binding.ContextID = contextID
	binding.TargetID = targetID
	if err := r.append("operation_binding", binding); err != nil {
		return err
	}
	r.operations[operationID] = binding
	return nil
}

func (r *registry) ownTarget(target ownedTarget) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !validTargetID(target.TargetID) || !validTaskID(target.TaskID) || !validContextID(target.ContextID) {
		return errors.New("browser target ownership is invalid")
	}
	if current, ok := r.targets[target.TargetID]; ok {
		if current == target {
			return nil
		}
		return errRegistryConflict
	}
	if len(r.targets) >= maxBrowserOwnedTargets {
		return errRegistryLimit
	}
	if err := r.append("target_owned", target); err != nil {
		return err
	}
	r.targets[target.TargetID] = target
	return nil
}

func (r *registry) operation(operationID string) (operationBinding, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	binding, ok := r.operations[operationID]
	return binding, ok
}

func (r *registry) taskContext(taskID string) (taskContext, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.tasks[taskID]
	return value, ok
}

func (r *registry) contexts() []taskContext {
	r.mu.Lock()
	defer r.mu.Unlock()
	contexts := make([]taskContext, 0, len(r.tasks))
	for _, value := range r.tasks {
		contexts = append(contexts, value)
	}
	return contexts
}

func (r *registry) ownsContext(taskID, contextID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if task, ok := r.tasks[taskID]; ok && task.ContextID == contextID {
		return true
	}
	for _, target := range r.targets {
		if target.TaskID == taskID && target.ContextID == contextID {
			return true
		}
	}
	for _, binding := range r.operations {
		if binding.TaskID == taskID && binding.ContextID == contextID {
			return true
		}
	}
	return false
}

func (r *registry) target(taskID, targetID string) (ownedTarget, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.targets[targetID]
	return value, ok && value.TaskID == taskID
}

func (r *registry) complete(operationID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("browser registry is closed")
	}
	binding, ok := r.operations[operationID]
	if !ok {
		return errRegistryConflict
	}
	if binding.Completed {
		return nil
	}
	binding.Completed = true
	if err := r.append("operation_completed", binding); err != nil {
		return err
	}
	r.operations[operationID] = binding
	return nil
}

func (r *registry) markEffectStarted(operationID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("browser registry is closed")
	}
	binding, ok := r.operations[operationID]
	if !ok {
		return errRegistryConflict
	}
	if binding.EffectMayHaveRun {
		return nil
	}
	binding.EffectMayHaveRun = true
	if err := r.append("operation_binding", binding); err != nil {
		return err
	}
	r.operations[operationID] = binding
	return nil
}

func (r *registry) closeTaskContext(taskID, contextID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !validTaskID(taskID) || !validContextID(contextID) {
		return errors.New("browser context close record is invalid")
	}
	if err := r.append("context_closed", taskContext{TaskID: taskID, ContextID: contextID}); err != nil {
		return err
	}
	if current, ok := r.tasks[taskID]; ok && current.ContextID == contextID {
		delete(r.tasks, taskID)
	}
	for targetID, target := range r.targets {
		if target.TaskID == taskID && target.ContextID == contextID {
			delete(r.targets, targetID)
		}
	}
	return nil
}

func (r *registry) reconcileContexts(live []string) error {
	liveSet := make(map[string]struct{}, len(live))
	for _, id := range live {
		liveSet[id] = struct{}{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for taskID, context := range r.tasks {
		if _, exists := liveSet[context.ContextID]; exists {
			// A BrowserContext created by the previous runtime connection should
			// have been disposed when that connection detached. Never adopt it.
			return errors.New("previous runtime browser context is still live")
		}
		if err := r.append("context_closed", context); err != nil {
			return err
		}
		delete(r.tasks, taskID)
		for targetID, target := range r.targets {
			if target.TaskID == taskID && target.ContextID == context.ContextID {
				delete(r.targets, targetID)
			}
		}
	}
	return nil
}

func (r *registry) append(kind string, data any) error {
	if r.store == nil {
		return errors.New("browser registry is unavailable")
	}
	return r.store.Append("browser."+kind, data)
}

func (r *registry) apply(kind string, data json.RawMessage) error {
	if len(data) == 0 || len(data) > maxBrowserOperationBytes {
		return errors.New("browser registry event is invalid")
	}
	decode := func(target any) error {
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(target); err != nil {
			return err
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return errors.New("browser registry event has trailing JSON")
		}
		return nil
	}
	switch kind {
	case "browser.operation", "browser.operation_binding", "browser.operation_completed":
		var binding operationBinding
		if err := decode(&binding); err != nil || !validOperationID(binding.OperationID) || !validTaskID(binding.TaskID) || !validBrowserAction(binding.Action) {
			return errors.New("browser operation event is invalid")
		}
		if binding.ContextID != "" && !validContextID(binding.ContextID) || binding.TargetID != "" && !validTargetID(binding.TargetID) || binding.MarkerURL != "" && !validMarkerURL(binding.MarkerURL) {
			return errors.New("browser operation fence is invalid")
		}
		r.operations[binding.OperationID] = binding
	case "browser.task_context":
		var context taskContext
		if err := decode(&context); err != nil || !validTaskID(context.TaskID) || !validContextID(context.ContextID) {
			return errors.New("browser task context event is invalid")
		}
		r.tasks[context.TaskID] = context
	case "browser.target_owned":
		var target ownedTarget
		if err := decode(&target); err != nil || !validTargetID(target.TargetID) || !validTaskID(target.TaskID) || !validContextID(target.ContextID) {
			return errors.New("browser target event is invalid")
		}
		r.targets[target.TargetID] = target
	case "browser.context_closed":
		var context taskContext
		if err := decode(&context); err != nil || !validTaskID(context.TaskID) || !validContextID(context.ContextID) {
			return errors.New("browser context close event is invalid")
		}
		if current, ok := r.tasks[context.TaskID]; ok && current.ContextID == context.ContextID {
			delete(r.tasks, context.TaskID)
		}
		for targetID, target := range r.targets {
			if target.TaskID == context.TaskID && target.ContextID == context.ContextID {
				delete(r.targets, targetID)
			}
		}
	default:
		return fmt.Errorf("unknown browser registry event %q", kind)
	}
	if len(r.operations) > maxBrowserOperations || len(r.targets) > maxBrowserOwnedTargets || len(r.tasks) > maxBrowserTaskContexts {
		return errRegistryLimit
	}
	return nil
}

func (r *registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return r.store.Close()
}

func (r *registry) healthy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.closed && r.store != nil && r.store.Ready()
}
