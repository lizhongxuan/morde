package app

import (
	"context"
	"errors"
	"time"
)

// A Task result is deliverable only while its original execution fence and
// authorization are current, even if the provider call itself has succeeded.
type ModelTaskDeliveryGuard interface {
	ValidateTaskModelDelivery(context.Context, ModelCallReservation) error
}

// A revision-interrupted request may only keep reading a provider response
// while its exact persisted stop witness and authorization remain valid.
type ModelTaskSettlementGuard interface {
	CheckRevisionSettlement(context.Context, ModelTaskFence) (bool, error)
}

type ModelTaskRecoveryConfigurationGuard interface {
	ValidateTaskModelRecoveryConfiguration(context.Context, ModelCallReservation) error
}

const modelRevisionSettlementPollInterval = 100 * time.Millisecond

// taskModelProviderContext never outlives the original 45-second deadline or
// the service lifecycle. Only an explicitly proven revision cancellation can
// outlive the Task context, and it may then read usage but not deliver output.
func taskModelProviderContext(ctx, original context.Context, call ModelCallReservation, store ModelCallStore) (context.Context, func()) {
	lifecycle := ModelTaskLifecycle(ctx)
	explicit := ModelExplicitCancellation(ctx)
	guard, ok := store.(ModelTaskSettlementGuard)
	configuration, hasConfiguration := store.(ModelTaskRecoveryConfigurationGuard)
	deadline, hasDeadline := original.Deadline()
	if !ok || !hasConfiguration || call.Task == nil || lifecycle == nil || lifecycle.Done() == nil ||
		explicit == nil || explicit.Done() == nil || !hasDeadline {
		return original, func() {}
	}
	provider, cancelProvider := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	stopLifecycle := context.AfterFunc(lifecycle, cancelProvider)
	if lifecycle.Err() != nil {
		cancelProvider()
	}
	monitor, cancelMonitor := context.WithCancel(lifecycle)
	stopExplicit := context.AfterFunc(explicit, func() {
		cancelProvider()
		cancelMonitor()
	})
	if explicit.Err() != nil {
		cancelProvider()
		cancelMonitor()
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
		case <-monitor.Done():
			return
		case <-provider.Done():
			return
		}
		if lifecycle.Err() != nil || explicit.Err() != nil || !errors.Is(context.Cause(ctx), ErrTaskRevisionInterrupted) {
			cancelProvider()
			return
		}
		check := func() bool {
			checkCtx, cancel := context.WithTimeout(monitor, time.Second)
			defer cancel()
			allowed, err := guard.CheckRevisionSettlement(checkCtx, *call.Task)
			if err != nil || !allowed {
				return false
			}
			return configuration.ValidateTaskModelRecoveryConfiguration(checkCtx, call) == nil &&
				lifecycle.Err() == nil && explicit.Err() == nil && provider.Err() == nil
		}
		if !check() {
			cancelProvider()
			return
		}
		ticker := time.NewTicker(modelRevisionSettlementPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-monitor.Done():
				return
			case <-provider.Done():
				return
			case <-ticker.C:
				if !check() {
					cancelProvider()
					return
				}
			}
		}
	}()
	return provider, func() {
		stopLifecycle()
		stopExplicit()
		cancelMonitor()
		cancelProvider()
		<-finished
	}
}
