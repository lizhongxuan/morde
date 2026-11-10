package dshguardian

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"morde/server/internal/app"
)

type operationLockMode uint8

const (
	operationLockExclusive operationLockMode = iota
	operationLockShared
)

type operationLocker struct {
	mu    sync.Mutex
	locks map[string]*operationLock
}

type operationLock struct {
	readers int
	writer  bool
	refs    int
	queue   []*operationWaiter
}

type operationWaiter struct {
	mode    operationLockMode
	ready   chan struct{}
	granted bool
}

type operationLockRequest struct {
	key  string
	mode operationLockMode
}

func (l *operationLocker) acquire(ctx context.Context, key string) (func(), error) {
	return l.acquireMode(ctx, key, operationLockExclusive)
}

func (l *operationLocker) acquireMode(ctx context.Context, key string, mode operationLockMode) (func(), error) {
	if ctx == nil || key == "" {
		return nil, errors.New("guardian operation identity or context is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[string]*operationLock)
	}
	lock := l.locks[key]
	if lock == nil {
		lock = &operationLock{}
		l.locks[key] = lock
	}
	lock.refs++
	waiter := &operationWaiter{mode: mode, ready: make(chan struct{})}
	if len(lock.queue) == 0 && !lock.writer && (mode == operationLockShared || lock.readers == 0) {
		grantOperationLock(lock, waiter)
	} else {
		lock.queue = append(lock.queue, waiter)
	}
	l.mu.Unlock()

	select {
	case <-waiter.ready:
		if err := ctx.Err(); err != nil {
			l.cancelAcquisition(key, lock, waiter)
			return nil, err
		}
		var once sync.Once
		return func() {
			once.Do(func() { l.release(key, lock, waiter.mode) })
		}, nil
	case <-ctx.Done():
		l.cancelAcquisition(key, lock, waiter)
		return nil, ctx.Err()
	}
}

func grantOperationLock(lock *operationLock, waiter *operationWaiter) {
	waiter.granted = true
	if waiter.mode == operationLockShared {
		lock.readers++
	} else {
		lock.writer = true
	}
	close(waiter.ready)
}

func (l *operationLocker) cancelAcquisition(key string, lock *operationLock, waiter *operationWaiter) {
	l.mu.Lock()
	if waiter.granted {
		l.releaseMode(lock, waiter.mode)
		lock.refs--
	} else {
		for index, queued := range lock.queue {
			if queued == waiter {
				lock.queue = append(lock.queue[:index], lock.queue[index+1:]...)
				lock.refs--
				break
			}
		}
	}
	l.grantQueued(lock)
	l.deleteIfUnused(key, lock)
	l.mu.Unlock()
}

func (l *operationLocker) release(key string, lock *operationLock, mode operationLockMode) {
	l.mu.Lock()
	l.releaseMode(lock, mode)
	lock.refs--
	l.grantQueued(lock)
	l.deleteIfUnused(key, lock)
	l.mu.Unlock()
}

func (l *operationLocker) releaseMode(lock *operationLock, mode operationLockMode) {
	if mode == operationLockShared {
		lock.readers--
	} else {
		lock.writer = false
	}
}

// grantQueued preserves FIFO order. Readers at the queue head may share the
// lock; a queued writer prevents later readers from bypassing it.
func (l *operationLocker) grantQueued(lock *operationLock) {
	if lock.writer || len(lock.queue) == 0 {
		return
	}
	if lock.readers > 0 && lock.queue[0].mode == operationLockExclusive {
		return
	}
	if lock.readers == 0 && lock.queue[0].mode == operationLockExclusive {
		waiter := lock.queue[0]
		lock.queue = lock.queue[1:]
		grantOperationLock(lock, waiter)
		return
	}
	for len(lock.queue) > 0 && lock.queue[0].mode == operationLockShared {
		waiter := lock.queue[0]
		lock.queue = lock.queue[1:]
		grantOperationLock(lock, waiter)
	}
}

func (l *operationLocker) deleteIfUnused(key string, lock *operationLock) {
	if lock.refs == 0 && l.locks[key] == lock {
		delete(l.locks, key)
	}
}

func (s *Service) lockOperation(ctx context.Context, key string) (func(), error) {
	if s == nil {
		return nil, errors.New("guardian operation service is unavailable")
	}
	return s.operationLocks.acquire(ctx, key)
}

func (s *Service) lockRuntimeTask(ctx context.Context, runtimeFence app.AccountAgentRuntimeFence,
	taskFence app.AgentInstanceFence) (func(), error) {
	return s.lockOperationRequests(ctx,
		operationLockRequest{key: accountRuntimeOperationKey(runtimeFence), mode: operationLockShared},
		operationLockRequest{key: taskOperationKey(taskFence), mode: operationLockExclusive},
	)
}

func (s *Service) lockOperationRequests(ctx context.Context, requests ...operationLockRequest) (func(), error) {
	if s == nil {
		return nil, errors.New("guardian operation service is unavailable")
	}
	requests = append([]operationLockRequest(nil), requests...)
	sort.Slice(requests, func(i, j int) bool { return requests[i].key < requests[j].key })
	unique := requests[:0]
	for _, request := range requests {
		if request.key == "" {
			return nil, errors.New("guardian operation identity is unavailable")
		}
		if len(unique) > 0 && unique[len(unique)-1].key == request.key {
			if request.mode == operationLockExclusive {
				unique[len(unique)-1].mode = operationLockExclusive
			}
			continue
		}
		unique = append(unique, request)
	}
	var releases []func()
	for _, request := range unique {
		release, err := s.operationLocks.acquireMode(ctx, request.key, request.mode)
		if err != nil {
			for index := len(releases) - 1; index >= 0; index-- {
				releases[index]()
			}
			return nil, err
		}
		releases = append(releases, release)
	}
	return func() {
		for index := len(releases) - 1; index >= 0; index-- {
			releases[index]()
		}
	}, nil
}

func accountRuntimeOperationKey(fence app.AccountAgentRuntimeFence) string {
	return fmt.Sprintf("account-runtime:%s:%s:%s:%s:%d", fence.ServiceInstanceID, fence.OwnerID,
		fence.ConversationID, fence.RuntimeID, fence.RuntimeGeneration)
}

func taskOperationKey(fence app.AgentInstanceFence) string {
	return fmt.Sprintf("task:%s:%s", fence.OwnerID, fence.TaskID)
}

func taskSandboxOperationKey(lifecycleID string) string {
	return "task-sandbox:" + lifecycleID
}
