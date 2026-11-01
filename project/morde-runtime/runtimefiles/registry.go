package runtimefiles

import (
	"context"
	"sync"

	"morde-runtime/runtimecore"
)

const maxCompletedWrites = 2048

type actionRun struct {
	done      chan struct{}
	write     bool
	completed bool
	committed bool
}

type operationRegistry struct {
	mu        sync.Mutex
	runs      map[string]*actionRun
	completed []string
}

func newOperationRegistry() *operationRegistry {
	return &operationRegistry{runs: make(map[string]*actionRun)}
}

func (r *operationRegistry) begin(operationID string, write bool) (*actionRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.runs[operationID]; exists {
		return nil, false
	}
	run := &actionRun{done: make(chan struct{}), write: write}
	r.runs[operationID] = run
	return run, true
}

func (r *operationRegistry) finish(operationID string, run *actionRun, committed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, exists := r.runs[operationID]; !exists || current != run {
		return
	}
	run.committed = committed
	run.completed = true
	close(run.done)
	if !run.write {
		delete(r.runs, operationID)
		return
	}
	r.completed = append(r.completed, operationID)
	for len(r.completed) > maxCompletedWrites {
		oldest := r.completed[0]
		r.completed = r.completed[1:]
		if old, ok := r.runs[oldest]; ok && old.completed {
			delete(r.runs, oldest)
		}
	}
}

func (r *operationRegistry) stop(ctx context.Context, operationID string, write bool) (runtimecore.StopResult, error) {
	r.mu.Lock()
	run, exists := r.runs[operationID]
	r.mu.Unlock()
	if !exists {
		// A write without state may have been evicted after commit, so the
		// runtime cannot prove its outcome. Read/list/find have no side effects.
		return runtimecore.StopResult{VerifiedStopped: !write}, nil
	}
	select {
	case <-run.done:
	case <-ctx.Done():
		return runtimecore.StopResult{}, nil
	}
	r.mu.Lock()
	committed := run.committed
	r.mu.Unlock()
	return runtimecore.StopResult{VerifiedStopped: !committed}, nil
}
