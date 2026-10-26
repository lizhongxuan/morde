package gateway

import (
	"context"
	"sync"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
)

const (
	activityMinimumInterval              = 15 * time.Second
	hostActivityLeaseTTL                 = 45 * time.Second
	hostActivityLeaseRenewTimeout        = 3 * time.Second
	hostActivityLeaseReleaseTimeout      = 2 * time.Second
	hostMemberAuthorizationCheckInterval = 2 * time.Second
	hostAuthorizationCheckTimeout        = 2 * time.Second
)

type hostActivityLeaseGuard struct {
	store                      app.HostActivityLeaseStore
	lease                      app.HostActivityLease
	ctx                        context.Context
	cancel                     context.CancelFunc
	done                       chan struct{}
	once                       sync.Once
	err                        error
	checkAuthorization         func(context.Context) error
	authorizationCheckInterval time.Duration
}

func (g *gateway) acquireHostActivityLease(ctx context.Context, ref host.Ref, kind string, checkAuthorization func(context.Context) error) (*hostActivityLeaseGuard, context.Context, error) {
	leaseID, err := domain.NewOperationID()
	if err != nil {
		return nil, nil, domain.NewError(domain.CodeInternal, "could not create Host activity lease")
	}
	lease := app.HostActivityLease{
		HostID: ref.ID, LeaseID: leaseID, RuntimeGeneration: ref.RuntimeGeneration,
		ControlVersion: ref.ControlVersion, Kind: kind, TTL: hostActivityLeaseTTL,
	}
	if err := lease.ValidateForAcquireOrRenew(); err != nil {
		return nil, nil, err
	}
	if err := g.deps.ActivityLeases.AcquireHostActivityLease(ctx, lease); err != nil {
		return nil, nil, err
	}
	leaseCtx, cancel := context.WithCancel(ctx)
	guard := &hostActivityLeaseGuard{
		store: g.deps.ActivityLeases, lease: lease, ctx: leaseCtx, cancel: cancel,
		done: make(chan struct{}), checkAuthorization: checkAuthorization,
	}
	interval := g.activityRenewInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	guard.authorizationCheckInterval = g.memberAuthorizationCheckInterval
	go guard.renew(interval)
	return guard, leaseCtx, nil
}

func (g *hostActivityLeaseGuard) renew(interval time.Duration) {
	defer close(g.done)
	tickInterval := interval
	if g.checkAuthorization != nil && g.authorizationCheckInterval > 0 && g.authorizationCheckInterval < tickInterval {
		tickInterval = g.authorizationCheckInterval
	}
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	lastRenew := time.Now()
	var lastAuthorizationCheck time.Time
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			if g.checkAuthorization != nil && (lastAuthorizationCheck.IsZero() || now.Sub(lastAuthorizationCheck) >= g.authorizationCheckInterval) {
				checkCtx, cancel := context.WithTimeout(g.ctx, hostAuthorizationCheckTimeout)
				err := g.checkAuthorization(checkCtx)
				cancel()
				if err != nil {
					g.cancel()
					return
				}
				lastAuthorizationCheck = now
			}
			if now.Sub(lastRenew) < interval {
				continue
			}
			renewCtx, cancel := context.WithTimeout(g.ctx, hostActivityLeaseRenewTimeout)
			err := g.store.RenewHostActivityLease(renewCtx, g.lease)
			cancel()
			if err != nil {
				// If the shared lease cannot be renewed, stop the proxied request so
				// it cannot outlive the activity record protecting the Host.
				g.cancel()
				return
			}
			lastRenew = now
		}
	}
}

func (g *hostActivityLeaseGuard) Close() error {
	g.once.Do(func() {
		g.cancel()
		<-g.done
		ctx, cancel := context.WithTimeout(context.Background(), hostActivityLeaseReleaseTimeout)
		defer cancel()
		g.err = g.store.ReleaseHostActivityLease(ctx, g.lease)
	})
	return g.err
}

func (g *gateway) markActive(hostID string) {
	now := time.Now()
	g.activityMu.Lock()
	last := g.activityAt[hostID]
	if now.Sub(last) < activityMinimumInterval {
		g.activityMu.Unlock()
		return
	}
	if len(g.activityAt) >= 10000 {
		for id, at := range g.activityAt {
			if now.Sub(at) >= activityMinimumInterval {
				delete(g.activityAt, id)
			}
		}
	}
	g.activityAt[hostID] = now
	g.activityMu.Unlock()
	g.deps.Runtime.MarkActive(hostID)
}
