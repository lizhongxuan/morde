package app

import (
	"context"
	"fmt"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/runtimeclient"
)

type ApplicationLaunchResult struct {
	HostID            string
	RuntimeGeneration int64
	RuntimeInstanceID string
	ControlVersion    int64
	Operation         runtimeclient.ApplicationLaunchOperation
}

type CurrentApplicationLaunchResult struct {
	HostID            string
	RuntimeGeneration int64
	RuntimeInstanceID string
	ControlVersion    int64
	Operation         *runtimeclient.ApplicationLaunchOperation
}

func (s *ControlService) LaunchApplication(ctx context.Context, principal identity.Principal, hostID, appID, launchID string) (ApplicationLaunchResult, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return ApplicationLaunchResult{}, err
	}
	if !ValidHostApplicationID(appID) {
		return ApplicationLaunchResult{}, domain.InvalidArgument("app_id", "application is not supported")
	}
	if !ValidApplicationLaunchID(launchID) {
		return ApplicationLaunchResult{}, domain.InvalidArgument("launch_id", "launch id is invalid")
	}
	if _, err := authorizeTaskHost(ctx, s.authorizer, principal, hostID, controlOperateScope); err != nil {
		return ApplicationLaunchResult{}, hideTaskAccessError(err)
	}
	value, err := s.ownedRunningApplicationHost(ctx, principal, hostID)
	if err != nil {
		return ApplicationLaunchResult{}, err
	}
	session, err := s.runtime.Connect(ctx, hostID, nil)
	if err != nil {
		return ApplicationLaunchResult{}, err
	}
	defer session.Close()
	value, err = s.ownedRunningApplicationHost(ctx, principal, hostID)
	if err != nil {
		return ApplicationLaunchResult{}, err
	}
	if session.Fence.RuntimeGeneration != value.RuntimeGeneration || session.Fence.RuntimeInstanceID != value.RuntimeInstanceID ||
		session.Fence.ControlVersion != value.ControlVersion {
		return ApplicationLaunchResult{}, domain.NewError(domain.CodeConflict, "Host runtime fence changed")
	}
	releaseLease, err := s.acquireApplicationOperationLease(ctx, session.Fence)
	if err != nil {
		return ApplicationLaunchResult{}, err
	}
	defer releaseLease()
	store, err := s.applicationLaunchStore()
	if err != nil {
		return ApplicationLaunchResult{}, err
	}
	if err := s.audit.Write(ctx, audit.Entry{
		ActorID: principal.UserID, ActorKind: audit.ActorKind(principal.Kind), Action: audit.ActionHostApplicationLaunch, Target: hostID,
		Detail: map[string]any{"application_id": appID, "launch_id": launchID,
			"runtime_generation": session.Fence.RuntimeGeneration, "control_version": session.Fence.ControlVersion},
	}); err != nil {
		return ApplicationLaunchResult{}, fmt.Errorf("write application launch audit: %w", err)
	}
	reservation, created, err := store.ReserveApplicationLaunch(ctx, ApplicationLaunch{
		HostID: hostID, OwnerID: principal.UserID, ID: launchID, ApplicationID: appID,
		RuntimeGeneration: session.Fence.RuntimeGeneration, RuntimeInstanceID: session.Fence.RuntimeInstanceID,
		ControlVersion: session.Fence.ControlVersion, Status: ApplicationLaunchStarting,
	})
	if err != nil {
		return ApplicationLaunchResult{}, err
	}
	if created {
		operation, launchErr := session.LaunchApplication(ctx, appID, launchID)
		if launchErr != nil {
			reservation.Status = ApplicationLaunchUnknown
			reservation.ErrorCode = ""
			updated, updateErr := store.UpdateApplicationLaunch(ctx, reservation)
			if updateErr == nil {
				reservation = updated
			}
		} else {
			reservation = applicationLaunchFromRuntime(reservation, operation)
			updated, updateErr := store.UpdateApplicationLaunch(ctx, reservation)
			if updateErr != nil {
				return ApplicationLaunchResult{}, updateErr
			}
			reservation = updated
		}
	} else {
		if !applicationLaunchTerminal(reservation.Status) {
			reservation = s.reconcileApplicationLaunch(ctx, session, store, reservation)
		}
	}
	return applicationLaunchResult(reservation), nil
}

func (s *ControlService) CurrentApplicationLaunch(ctx context.Context, principal identity.Principal, hostID, appID string) (CurrentApplicationLaunchResult, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return CurrentApplicationLaunchResult{}, err
	}
	if !ValidHostApplicationID(appID) {
		return CurrentApplicationLaunchResult{}, domain.InvalidArgument("app_id", "application is not supported")
	}
	if _, err := authorizeTaskHost(ctx, s.authorizer, principal, hostID, controlObserveScope); err != nil {
		return CurrentApplicationLaunchResult{}, hideTaskAccessError(err)
	}
	value, err := s.ownedHost(ctx, principal, hostID)
	if err != nil {
		return CurrentApplicationLaunchResult{}, err
	}
	store, err := s.applicationLaunchStore()
	if err != nil {
		return CurrentApplicationLaunchResult{}, err
	}
	launch, err := store.GetLatestApplicationLaunch(ctx, hostID, principal.UserID, appID)
	if domain.CodeOf(err) == domain.CodeNotFound {
		return CurrentApplicationLaunchResult{
			HostID: hostID, RuntimeGeneration: value.RuntimeGeneration,
			RuntimeInstanceID: value.RuntimeInstanceID, ControlVersion: value.ControlVersion,
		}, nil
	}
	if err != nil {
		return CurrentApplicationLaunchResult{}, err
	}
	launch = s.reconcileStoredApplicationLaunch(ctx, value, store, launch)
	result := applicationLaunchResult(launch)
	return CurrentApplicationLaunchResult{
		HostID: result.HostID, RuntimeGeneration: result.RuntimeGeneration,
		RuntimeInstanceID: result.RuntimeInstanceID, ControlVersion: result.ControlVersion,
		Operation: &result.Operation,
	}, nil
}

func (s *ControlService) GetApplicationLaunch(ctx context.Context, principal identity.Principal, hostID, appID, launchID string) (ApplicationLaunchResult, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return ApplicationLaunchResult{}, err
	}
	if !ValidHostApplicationID(appID) {
		return ApplicationLaunchResult{}, domain.InvalidArgument("app_id", "application is not supported")
	}
	if !ValidApplicationLaunchID(launchID) {
		return ApplicationLaunchResult{}, domain.NewError(domain.CodeNotFound, "application launch not found")
	}
	if _, err := authorizeTaskHost(ctx, s.authorizer, principal, hostID, controlObserveScope); err != nil {
		return ApplicationLaunchResult{}, hideTaskAccessError(err)
	}
	value, err := s.ownedHost(ctx, principal, hostID)
	if err != nil {
		return ApplicationLaunchResult{}, err
	}
	store, err := s.applicationLaunchStore()
	if err != nil {
		return ApplicationLaunchResult{}, err
	}
	launch, err := store.GetApplicationLaunch(ctx, hostID, principal.UserID, appID, launchID)
	if err != nil {
		return ApplicationLaunchResult{}, err
	}
	launch = s.reconcileStoredApplicationLaunch(ctx, value, store, launch)
	return applicationLaunchResult(launch), nil
}

func (s *ControlService) StopApplicationLaunch(ctx context.Context, principal identity.Principal, hostID, appID, launchID string) (ApplicationLaunchResult, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return ApplicationLaunchResult{}, err
	}
	if !ValidHostApplicationID(appID) {
		return ApplicationLaunchResult{}, domain.InvalidArgument("app_id", "application is not supported")
	}
	if !ValidApplicationLaunchID(launchID) {
		return ApplicationLaunchResult{}, domain.NewError(domain.CodeNotFound, "application launch not found")
	}
	if _, err := authorizeTaskHost(ctx, s.authorizer, principal, hostID, controlOperateScope); err != nil {
		return ApplicationLaunchResult{}, hideTaskAccessError(err)
	}
	value, err := s.ownedHost(ctx, principal, hostID)
	if err != nil {
		return ApplicationLaunchResult{}, err
	}
	store, err := s.applicationLaunchStore()
	if err != nil {
		return ApplicationLaunchResult{}, err
	}
	launch, err := store.GetApplicationLaunch(ctx, hostID, principal.UserID, appID, launchID)
	if err != nil {
		return ApplicationLaunchResult{}, err
	}
	if applicationLaunchTerminal(launch.Status) {
		return applicationLaunchResult(launch), nil
	}
	if value.State != host.StateRunning || (value.ControlState != host.ControlAI && value.ControlState != host.ControlUser) {
		return ApplicationLaunchResult{}, domain.NewError(domain.CodeInvalidState, "Host is not ready for application control")
	}
	if err := s.audit.Write(ctx, audit.Entry{
		ActorID: principal.UserID, ActorKind: audit.ActorKind(principal.Kind), Action: audit.ActionHostApplicationStop, Target: hostID,
		Detail: map[string]any{"application_id": launch.ApplicationID, "launch_id": launch.ID,
			"runtime_generation": launch.RuntimeGeneration, "control_version": launch.ControlVersion},
	}); err != nil {
		return ApplicationLaunchResult{}, fmt.Errorf("write application stop audit: %w", err)
	}
	if !applicationLaunchMatchesHost(launch, value) || value.State != host.StateRunning {
		return applicationLaunchResult(s.markApplicationLaunchUnknown(ctx, store, launch)), nil
	}
	session, err := s.runtime.Connect(ctx, hostID, nil)
	if err != nil {
		return applicationLaunchResult(s.markApplicationLaunchUnknown(ctx, store, launch)), nil
	}
	defer session.Close()
	if session.Fence.HostID != hostID || session.Fence.RuntimeGeneration != launch.RuntimeGeneration {
		return applicationLaunchResult(s.markApplicationLaunchUnknown(ctx, store, launch)), nil
	}
	releaseLease, err := s.acquireApplicationOperationLease(ctx, session.Fence)
	if err != nil {
		return ApplicationLaunchResult{}, err
	}
	defer releaseLease()
	launch.Status = ApplicationLaunchStopping
	launch.ErrorCode = ""
	launch, err = store.UpdateApplicationLaunch(ctx, launch)
	if err != nil {
		return ApplicationLaunchResult{}, err
	}
	operation, stopErr := session.StopApplication(ctx, appID, launchID)
	if stopErr != nil {
		launch.Status = ApplicationLaunchUnknown
		launch.ErrorCode = ""
		if updated, updateErr := store.UpdateApplicationLaunch(ctx, launch); updateErr == nil {
			launch = updated
		}
		return applicationLaunchResult(launch), nil
	}
	launch = applicationLaunchFromRuntime(launch, operation)
	updated, err := store.UpdateApplicationLaunch(ctx, launch)
	if err != nil {
		return ApplicationLaunchResult{}, err
	}
	return applicationLaunchResult(updated), nil
}

func (s *ControlService) reconcileStoredApplicationLaunch(ctx context.Context, current host.Host, store ApplicationLaunchStore, launch ApplicationLaunch) ApplicationLaunch {
	if applicationLaunchTerminal(launch.Status) {
		return launch
	}
	if !applicationLaunchMatchesHost(launch, current) || current.State != host.StateRunning ||
		(current.ControlState != host.ControlAI && current.ControlState != host.ControlUser) {
		return s.markApplicationLaunchUnknown(ctx, store, launch)
	}
	session, err := s.runtime.Connect(ctx, current.ID, nil)
	if err != nil {
		return s.markApplicationLaunchUnknown(ctx, store, launch)
	}
	defer session.Close()
	return s.reconcileApplicationLaunch(ctx, session, store, launch)
}

func (s *ControlService) reconcileApplicationLaunch(ctx context.Context, session *RuntimeSession, store ApplicationLaunchStore, launch ApplicationLaunch) ApplicationLaunch {
	operation, err := session.GetApplicationLaunch(ctx, launch.ApplicationID, launch.ID)
	if err != nil {
		return s.markApplicationLaunchUnknown(ctx, store, launch)
	}
	if launch.Status == ApplicationLaunchStopping && operation.Status != runtimeclient.ApplicationLaunchStopping &&
		operation.Status != runtimeclient.ApplicationLaunchStopped && operation.Status != runtimeclient.ApplicationLaunchExited &&
		operation.Status != runtimeclient.ApplicationLaunchFailed && operation.Status != runtimeclient.ApplicationLaunchUnknown {
		return launch
	}
	updated := applicationLaunchFromRuntime(launch, operation)
	result, updateErr := store.UpdateApplicationLaunch(ctx, updated)
	if updateErr != nil {
		return launch
	}
	return result
}

func (s *ControlService) markApplicationLaunchUnknown(ctx context.Context, store ApplicationLaunchStore, launch ApplicationLaunch) ApplicationLaunch {
	if applicationLaunchTerminal(launch.Status) {
		return launch
	}
	launch.Status = ApplicationLaunchUnknown
	launch.ErrorCode = ""
	if result, err := store.UpdateApplicationLaunch(ctx, launch); err == nil {
		return result
	}
	return launch
}

func (s *ControlService) ownedRunningApplicationHost(ctx context.Context, principal identity.Principal, hostID string) (host.Host, error) {
	value, err := s.ownedHost(ctx, principal, hostID)
	if err != nil {
		return host.Host{}, err
	}
	if value.State != host.StateRunning || (value.ControlState != host.ControlAI && value.ControlState != host.ControlUser) {
		return host.Host{}, domain.NewError(domain.CodeInvalidState, "Host is not ready for application control")
	}
	return value, nil
}

func (s *ControlService) ownedHost(ctx context.Context, principal identity.Principal, hostID string) (host.Host, error) {
	value, err := s.hosts.GetHost(ctx, hostID)
	if err != nil || value.OwnerID != principal.UserID || value.DeletedAt != nil {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	return value, nil
}

func (s *ControlService) applicationLaunchStore() (ApplicationLaunchStore, error) {
	store, ok := s.hosts.(ApplicationLaunchStore)
	if !ok {
		return nil, domain.NewError(domain.CodeInternal, "application launch registry is unavailable")
	}
	return store, nil
}

// The Gateway activity lease is also the durable in-flight guard for an
// application operation started through Gateway. Its acquisition is serialized
// with Host release, and the release worker waits for it before deleting the
// sandbox. Runtime application requests have a 60-second deadline.
func (s *ControlService) acquireApplicationOperationLease(ctx context.Context, fence runtimeclient.HostFence) (func(), error) {
	store, ok := s.hosts.(HostActivityLeaseStore)
	if !ok {
		return nil, domain.NewError(domain.CodeInternal, "Host activity lease registry is unavailable")
	}
	leaseID, err := domain.NewOperationID()
	if err != nil {
		return nil, domain.NewError(domain.CodeInternal, "could not create Host activity lease")
	}
	lease := HostActivityLease{
		HostID: fence.HostID, LeaseID: leaseID, RuntimeGeneration: fence.RuntimeGeneration,
		ControlVersion: fence.ControlVersion, Kind: HostActivityLeaseKindGateway, TTL: 2 * time.Minute,
	}
	if err := store.AcquireHostActivityLease(ctx, lease); err != nil {
		return nil, err
	}
	return func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = store.ReleaseHostActivityLease(releaseCtx, lease)
	}, nil
}

func applicationLaunchFromRuntime(value ApplicationLaunch, operation runtimeclient.ApplicationLaunchOperation) ApplicationLaunch {
	value.Status = string(operation.Status)
	value.ErrorCode = operation.ErrorCode
	return value
}

func applicationLaunchResult(value ApplicationLaunch) ApplicationLaunchResult {
	return ApplicationLaunchResult{
		HostID: value.HostID, RuntimeGeneration: value.RuntimeGeneration, RuntimeInstanceID: value.RuntimeInstanceID,
		ControlVersion: value.ControlVersion,
		Operation: runtimeclient.ApplicationLaunchOperation{
			ID: value.ID, ApplicationID: value.ApplicationID, Status: runtimeclient.ApplicationLaunchStatus(value.Status),
			ErrorCode: value.ErrorCode, CreatedAt: value.CreatedAt.UTC(), UpdatedAt: value.UpdatedAt.UTC(),
		},
	}
}

func applicationLaunchMatchesHost(launch ApplicationLaunch, value host.Host) bool {
	// Runtime restarts rotate its instance identity, and a control handoff can
	// advance control_version, while the same-generation application cgroup and
	// durable launch journal remain available. Reconcile by Host generation;
	// RuntimeClient still requires its response to echo the current exact fence.
	return launch.HostID == value.ID && launch.OwnerID == value.OwnerID && launch.RuntimeGeneration == value.RuntimeGeneration
}

func applicationLaunchTerminal(value string) bool {
	switch value {
	case ApplicationLaunchStopped, ApplicationLaunchExited, ApplicationLaunchFailed:
		return true
	default:
		return false
	}
}
