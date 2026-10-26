package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
)

type Runtime struct {
	hosts     HostStore
	sandboxes SandboxRouter
	sealer    *Sealer
	ttl       time.Duration

	mu        sync.Mutex
	upstreams map[upstreamKey]cachedUpstream
	active    map[string]time.Time
}

type upstreamKey struct {
	hostID            string
	runtimeGeneration int64
	sandboxID         string
	port              int
}

type cachedUpstream struct {
	upstream Upstream
	expires  time.Time
}

func NewRuntime(hosts HostStore, sandboxes SandboxRouter, sealer *Sealer, cacheTTL time.Duration) (*Runtime, error) {
	if hosts == nil || sandboxes == nil || sealer == nil || cacheTTL <= 0 {
		return nil, fmt.Errorf("runtime dependencies and positive cache ttl are required")
	}
	return &Runtime{
		hosts: hosts, sandboxes: sandboxes, sealer: sealer, ttl: cacheTTL,
		upstreams: make(map[upstreamKey]cachedUpstream), active: make(map[string]time.Time),
	}, nil
}

func (r *Runtime) Lookup(ctx context.Context, hostID string) (host.Ref, error) {
	value, err := r.hosts.GetHost(ctx, hostID)
	if err != nil {
		return host.Ref{}, err
	}
	if value.State == host.StateDeleted || value.DeletedAt != nil {
		return host.Ref{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	return host.Ref{
		ID: value.ID, OwnerID: value.OwnerID, State: value.State,
		RuntimeGeneration: value.RuntimeGeneration, ControlVersion: value.ControlVersion,
		ControlState: value.ControlState,
	}, nil
}

func (r *Runtime) Resolve(ctx context.Context, hostID string, port int) (Upstream, error) {
	if port < 1 || port > 65535 {
		return Upstream{}, domain.InvalidArgument("port", "port is invalid")
	}
	value, err := r.hosts.GetHost(ctx, hostID)
	if err != nil {
		return Upstream{}, err
	}
	if value.State != host.StateRunning {
		if value.State == host.StateCreating {
			return Upstream{}, domain.NewError(domain.CodeHostNotReady, "host is not ready")
		}
		return Upstream{}, domain.NewError(domain.CodeInvalidState, "host is not running")
	}
	if value.SandboxID == "" {
		return Upstream{}, domain.NewError(domain.CodeUpstreamError, "host has no sandbox")
	}
	key := upstreamKey{hostID: hostID, runtimeGeneration: value.RuntimeGeneration, sandboxID: value.SandboxID, port: port}
	now := time.Now()
	r.mu.Lock()
	for candidate, cached := range r.upstreams {
		if !now.Before(cached.expires) {
			delete(r.upstreams, candidate)
		}
	}
	if cached, ok := r.upstreams[key]; ok {
		r.mu.Unlock()
		return cloneUpstream(cached.upstream), nil
	}
	r.mu.Unlock()

	aioKey, err := r.sealer.Open(value.AIOKeyEnc)
	if err != nil {
		return Upstream{}, fmt.Errorf("open host api key: %w", err)
	}
	client, err := r.sandboxes.For(value.RuntimeProfile)
	if err != nil {
		return Upstream{}, sandboxError("select sandbox client", err)
	}
	upstream, err := client.Endpoint(ctx, value.SandboxID, port)
	if err != nil {
		return Upstream{}, sandboxError("resolve sandbox endpoint", err)
	}
	upstream.APIKey = string(aioKey)
	r.mu.Lock()
	r.upstreams[key] = cachedUpstream{upstream: cloneUpstream(upstream), expires: now.Add(r.ttl)}
	r.mu.Unlock()
	return upstream, nil
}

func (r *Runtime) InvalidateUpstream(hostID string) {
	r.mu.Lock()
	for key := range r.upstreams {
		if key.hostID == hostID {
			delete(r.upstreams, key)
		}
	}
	r.mu.Unlock()
}

func (r *Runtime) MarkActive(hostID string) {
	r.mu.Lock()
	r.active[hostID] = time.Now()
	r.mu.Unlock()
}

func (r *Runtime) ActivityAt(hostID string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.active[hostID]
	return at, ok
}

func (r *Runtime) TakeActivity() map[string]time.Time {
	r.mu.Lock()
	active := r.active
	r.active = make(map[string]time.Time)
	r.mu.Unlock()
	return active
}

func (r *Runtime) FlushActivity(ctx context.Context) error {
	active := r.TakeActivity()
	if len(active) == 0 {
		return nil
	}
	ids := make([]string, 0, len(active))
	for id := range active {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if err := r.hosts.TouchHosts(ctx, ids, time.Now().UTC()); err != nil {
		r.mu.Lock()
		for id, at := range active {
			if current, ok := r.active[id]; !ok || at.After(current) {
				r.active[id] = at
			}
		}
		r.mu.Unlock()
		return fmt.Errorf("flush active hosts: %w", err)
	}
	return nil
}

func cloneUpstream(value Upstream) Upstream {
	headers := make(map[string]string, len(value.Headers))
	for key, header := range value.Headers {
		headers[key] = header
	}
	value.Headers = headers
	return value
}

func sandboxError(operation string, err error) error {
	switch {
	case errors.Is(err, ErrSandboxNotFound):
		return domain.NewError(domain.CodeNotFound, "sandbox not found")
	case errors.Is(err, ErrSandboxUnsupported):
		return domain.NewError(domain.CodeUnsupported, "sandbox operation unsupported")
	case errors.Is(err, ErrSandboxConflict):
		return domain.NewError(domain.CodeInvalidState, "sandbox state changed")
	default:
		return fmt.Errorf("%s: %w", operation, domain.NewError(domain.CodeUpstreamError, "sandbox service unavailable"))
	}
}

var _ HostRuntime = (*Runtime)(nil)
